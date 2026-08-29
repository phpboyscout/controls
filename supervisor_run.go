package controls

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"gitlab.com/phpboyscout/go/errors"
)

// Supervisor runs children that attach and detach while the process is running.
//
// # Registered means required; attached does not
//
// A [Service] registered with a [Controller] is a requirement for operation: if
// its probe fails, the whole process reports unready. A child attached to a
// Supervisor is not. A failed child never makes the supervisor unready — at any
// proportion, including all of them — because whether that child mattered is
// the consumer's judgement and the consumer has the context to make it.
//
// What the supervisor does instead is report [CheckDegraded] while any child has
// terminally failed, and hand the consumer a [Failure] to act on.
//
// # Which of the two to use
//
// A Controller manages a fixed set registered before Start, with ordered
// shutdown. A Supervisor manages a set that changes, with concurrent shutdown
// and no ordering between children — they must not depend on each other. A
// process has one Controller and may have several Supervisors.
//
// A Supervisor is itself a Service: register it with a Controller and its
// children are supervised beneath it.
type Supervisor struct {
	mu       sync.Mutex
	children map[string]*supervisedChild
	order    []string

	ctx     context.Context //nolint:containedctx // Start's context, needed to launch children attached later.
	cancel  context.CancelFunc
	started bool

	onFailure   func(Failure)
	dispatch    chan Failure
	dispatchWG  sync.WaitGroup
	failures    chan Failure
	failuresOn  bool
	droppedRpts atomic.Int64

	wg sync.WaitGroup
}

type supervisedChild struct {
	spec   Child
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	state    ChildState
	restarts int
	panics   int
	lastErr  error
}

// SupervisorOption configures a [Supervisor].
type SupervisorOption func(*Supervisor)

// WithOnFailure registers a callback for a child that has exhausted its restart
// policy.
//
// It runs on a dedicated dispatch goroutine, behind a recover. That buys three
// things: a slow callback cannot stall supervision, failures arrive in order,
// and a callback that calls back into the supervisor — Detach on the child that
// just died is the obvious thing to write — cannot deadlock against a lock held
// while notifying.
//
// The callback returns nothing. A consumer that wants the supervisor to act
// calls its API; an instruction returned from a notification would make the
// callback load-bearing, synchronous and re-entrant.
func WithOnFailure(fn func(Failure)) SupervisorOption {
	return func(s *Supervisor) { s.onFailure = fn }
}

// NewSupervisor returns a supervisor with no children.
func NewSupervisor(opts ...SupervisorOption) *Supervisor {
	s := &Supervisor{children: map[string]*supervisedChild{}}

	for _, o := range opts {
		o(s)
	}

	return s
}

// Failures returns a channel of children that have exhausted their restart
// policy.
//
// The channel is created on first call and bounded at
// [DefaultFailureBufferSize]. A consumer that never calls this never has one to
// fill; a consumer that does is expected to drain it, and sends that would block
// are dropped and counted rather than stalling the supervisor. See
// [Supervisor.DroppedReports].
func (s *Supervisor) Failures() <-chan Failure {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.failuresOn {
		s.failures = make(chan Failure, DefaultFailureBufferSize)
		s.failuresOn = true
	}

	return s.failures
}

// DroppedReports is how many failure notifications could not be delivered to
// the channel because it was full.
//
// It should be zero. A drop nobody counts is indistinguishable from a system
// with nothing to report, which is the failure this whole type is arranged to
// avoid.
func (s *Supervisor) DroppedReports() int64 { return s.droppedRpts.Load() }

// Attach adds a child, before or after [Supervisor.Start].
//
// Attaching after Start is the point of this type: a Controller cannot do it,
// and says so — a late registration there "is never started, monitored, or
// stopped". A child attached here is supervised identically whenever it arrives.
func (s *Supervisor) Attach(c Child) error {
	if c.Name == "" {
		return errors.New("controls: a child needs a name")
	}

	if c.Start == nil {
		return errors.Newf("controls: child %q has no Start", c.Name)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.children[c.Name]; ok {
		return errors.Wrapf(ErrChildAttached, "%q", c.Name)
	}

	child := &supervisedChild{spec: c, done: make(chan struct{}), state: ChildStopped}
	s.children[c.Name] = child
	s.order = append(s.order, c.Name)

	if s.started {
		// The supervisor's own context is the right parent: a child's lifetime
		// belongs to the supervisor, not to whoever happened to attach it.
		s.launch(s.ctx, child) //nolint:contextcheck // s.ctx is derived from Start's ctx; Attach has no caller context to inherit.
	}

	return nil
}

// Detach stops one child and forgets it. Every other child is untouched.
//
// It waits for the child to stop, bounded by ctx. If the budget expires the
// child is still forgotten and [ErrDetachTimeout] is returned, so a caller
// learns that a goroutine outlived its detach rather than discovering it later
// as a leak nothing reports.
func (s *Supervisor) Detach(ctx context.Context, name string) error {
	s.mu.Lock()
	child, ok := s.children[name]

	if ok {
		delete(s.children, name)
		s.order = removeName(s.order, name)
	}
	s.mu.Unlock()

	if !ok {
		return errors.Wrapf(ErrChildNotAttached, "%q", name)
	}

	s.stopChild(ctx, child)

	select {
	case <-child.done:
		return nil
	case <-ctx.Done():
		return errors.Wrapf(ErrDetachTimeout, "%q", name)
	}
}

// Start begins supervising, and is a [StartFunc] so a Supervisor registers with
// a Controller like any other service.
//
// It returns once children are launched; the supervisor serves in the
// background from then on.
func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return errors.New("controls: the supervisor is already started")
	}

	sctx, cancel := context.WithCancel(ctx)
	s.ctx, s.cancel = sctx, cancel
	s.started = true

	s.dispatch = make(chan Failure, DefaultFailureBufferSize)
	s.dispatchWG.Add(1)

	// The channel is passed in rather than read from the field inside the
	// goroutine: closeDispatch nils the field, and a goroutine reading it is a
	// data race the detector finds immediately.
	go s.dispatchLoop(s.dispatch)

	for _, name := range s.order {
		s.launch(sctx, s.children[name])
	}

	return nil
}

// Stop cancels every child at once and waits for all of them.
//
// Concurrent, not ordered: shutdown is bounded by the slowest child rather than
// by their number. Ten children taking 100ms each stop in about 100ms, where
// one at a time would take a second. The price is that children must not depend
// on each other — a consumer whose units do wants a Controller, which provides
// reverse-registration ordering deliberately.
func (s *Supervisor) Stop(ctx context.Context) {
	s.mu.Lock()
	kids := make([]*supervisedChild, 0, len(s.children))

	for _, c := range s.children {
		kids = append(kids, c)
	}

	cancel := s.cancel
	s.mu.Unlock()

	var wg sync.WaitGroup

	for _, c := range kids {
		wg.Add(1)

		go func(c *supervisedChild) {
			defer wg.Done()

			s.stopChild(ctx, c)
			<-c.done
		}(c)
	}

	wg.Wait()

	if cancel != nil {
		cancel()
	}

	s.wg.Wait()
	s.closeDispatch()
}

// stopChild cancels a child and calls its Stop, if it has one.
func (s *Supervisor) stopChild(ctx context.Context, c *supervisedChild) {
	if c.cancel != nil {
		c.cancel()
	}

	if c.spec.Stop != nil {
		callStop(ctx, c.spec.Stop)
	}
}

// launch starts one child's supervision goroutine. The lock must be held, and
// parent is the supervisor's own context — taken as a parameter rather than
// read from the field so the flow is visible at every call site.
func (s *Supervisor) launch(parent context.Context, c *supervisedChild) {
	ctx, cancel := context.WithCancel(parent)
	c.cancel = cancel

	s.wg.Add(1)

	go func() {
		defer s.wg.Done()
		defer close(c.done)

		s.supervise(ctx, c)
	}()
}

// supervise runs one child until it is cancelled or exhausts its policy.
//
// The backoff arithmetic is the Services machinery — resolveRestartTimings,
// calculateNextBackoff and the reset-interval rule — rather than a second
// implementation of it. What differs is where a failure goes: a Service sends
// on the controller's error channel, a child produces a Failure for the
// consumer.
func (s *Supervisor) supervise(ctx context.Context, c *supervisedChild) {
	// A nil policy means no restarts at all, and cannot be expressed through the
	// Service rule below — that reads MaxRestarts <= 0 as unlimited, so no value
	// of it means "never". Hence an explicit flag rather than a sentinel value
	// that would have to lie.
	noRestart := c.spec.RestartPolicy == nil

	policy := c.spec.RestartPolicy
	if policy == nil {
		policy = &RestartPolicy{}
	}

	timings := resolveRestartTimings(policy)
	restarts := 0

	for {
		c.setState(ChildRunning)

		runStarted := time.Now()
		err, panicked := s.runChildOnce(ctx, c)

		if ctx.Err() != nil {
			c.setState(ChildStopped)

			return
		}

		if err == nil {
			c.setState(ChildStopped)

			return
		}

		c.record(err, panicked)

		if time.Since(runStarted) >= timings.resetInterval {
			restarts = 0
			timings.backoff = initialBackoff(policy)
		}

		if exhausted(noRestart, policy, restarts) {
			s.fail(c, restarts, err, panicked)

			return
		}

		restarts++
		c.setRestarts(restarts)

		select {
		case <-time.After(timings.backoff):
			timings.backoff = calculateNextBackoff(timings.backoff, timings.maxBackoff)
		case <-ctx.Done():
			c.setState(ChildStopped)

			return
		}
	}
}

// exhausted reports whether a child has used up its restart allowance.
//
// The same rule a Service uses (services.go:345): MaxRestarts <= 0 means
// UNLIMITED, not none. Writing it the other way round is precisely the
// divergence sharing RestartPolicy exists to prevent, and the first draft of
// this loop had it.
func exhausted(noRestart bool, policy *RestartPolicy, restarts int) bool {
	if noRestart {
		return true
	}

	return policy.MaxRestarts > 0 && restarts >= policy.MaxRestarts
}

// runChildOnce calls the child's Start and converts a panic into an error.
//
// Not optional: an unrecovered panic on any goroutine terminates the process,
// so a supervisor that launches one and leaves it unguarded has moved where the
// crash comes from rather than contained it. The same line callStop and
// callProbe already draw around caller-supplied functions.
func (s *Supervisor) runChildOnce(ctx context.Context, c *supervisedChild) (err error, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			err = errors.Newf("controls: child %q panicked: %v", c.spec.Name, r)
		}
	}()

	return c.spec.Start(ctx), false
}

// fail marks a child failed and reports it to the consumer.
func (s *Supervisor) fail(c *supervisedChild, restarts int, err error, panicked bool) {
	c.setState(ChildFailed)

	f := Failure{
		Name:     c.spec.Name,
		Err:      errors.Wrapf(err, "controls: child %q exhausted its restart policy", c.spec.Name),
		Restarts: restarts,
		Panicked: panicked,
	}

	s.mu.Lock()
	ch, on := s.failures, s.failuresOn
	dispatch := s.dispatch
	s.mu.Unlock()

	if on {
		select {
		case ch <- f:
		default:
			s.droppedRpts.Add(1)
		}
	}

	if dispatch != nil {
		select {
		case dispatch <- f:
		default:
			s.droppedRpts.Add(1)
		}
	}
}

// dispatchLoop delivers failures to the callback, one at a time and in order.
func (s *Supervisor) dispatchLoop(ch <-chan Failure) {
	defer s.dispatchWG.Done()

	for f := range ch {
		if s.onFailure == nil {
			continue
		}

		func() {
			defer func() { _ = recover() }()

			s.onFailure(f)
		}()
	}
}

func (s *Supervisor) closeDispatch() {
	s.mu.Lock()
	d := s.dispatch
	s.dispatch = nil
	s.mu.Unlock()

	if d != nil {
		close(d)
		s.dispatchWG.Wait()
	}
}

func removeName(names []string, name string) []string {
	for i, n := range names {
		if n == name {
			return append(names[:i], names[i+1:]...)
		}
	}

	return names
}

func (c *supervisedChild) setState(state ChildState) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.state = state
}

func (c *supervisedChild) setRestarts(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.restarts = n
}

func (c *supervisedChild) record(err error, panicked bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.lastErr = err

	if panicked {
		c.panics++
	}
}

func (c *supervisedChild) status() ChildStatus {
	c.mu.Lock()
	defer c.mu.Unlock()

	return ChildStatus{State: c.state, Restarts: c.restarts, Panics: c.panics, LastErr: c.lastErr}
}

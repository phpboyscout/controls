package controls

import (
	"context"
	"fmt"
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
//
// # Single use, and what each call does out of order
//
// A Supervisor moves through new, running, stopping and stopped, and never goes
// back. Once shutdown begins, [Supervisor.Attach] and [Supervisor.Start] return
// [ErrSupervisorStopped] and [Supervisor.Readiness] reports it, rather than
// accepting work that will never be supervised.
//
// [Supervisor.Stop] and [Supervisor.Detach] are bounded by the context they are
// given, including across a [Child.Stop] that blocks. A child that outlives the
// budget is abandoned rather than allowed to hold shutdown open, which is the
// same bargain [Controller] strikes at its own shutdown deadline.
type Supervisor struct {
	mu       sync.Mutex
	children map[string]*supervisedChild
	order    []string

	state   supState
	ctx     context.Context //nolint:containedctx // Start's context, needed to launch children attached later.
	cancel  context.CancelFunc
	stopped chan struct{}

	onFailure   func(Failure)
	queue       []Failure
	queueWake   chan struct{}
	queueClosed bool

	failures    chan Failure
	failuresOn  bool
	droppedRpts atomic.Int64

	wg sync.WaitGroup
}

// supState is the supervisor's lifecycle. It is ordered, and compared with >=,
// so a new state must be inserted in the position its transitions belong.
type supState int

const (
	supNew supState = iota
	supRunning
	supStopping
	supStopped
)

type supervisedChild struct {
	spec   Child
	policy *RestartPolicy

	// cancel and launched are guarded by the SUPERVISOR's mutex, not the
	// child's: they are written by launch, which runs under it, and read by
	// Stop and Detach deciding what there is to wait for.
	cancel   context.CancelFunc
	launched bool

	done chan struct{}

	// stopOnce guarantees one shutdown per child however many paths reach it.
	// Detach racing Stop would otherwise call a caller's Stop twice, and most
	// close a channel or dispose a resource, so the second call panics into a
	// recover that reports nothing.
	stopOnce sync.Once
	stopDone chan struct{}

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
// It runs on a dedicated dispatch goroutine, behind a recover, from an ordered
// queue. That buys three things: a slow callback cannot stall supervision,
// failures arrive in order, and none is lost while the callback is busy. The
// queue is unbounded because a terminal failure is rare by construction — a
// child reaches one only after exhausting its restart policy — and because a
// callback is not opt-in the way [Supervisor.Failures] is, so shedding from it
// would lose notifications a consumer never agreed to lose.
//
// A callback may call back into the supervisor, including [Supervisor.Stop].
// Nothing is held while it runs, and Stop does not wait for the dispatch
// goroutine: a callback still executing when Stop returns runs to completion,
// and the goroutine exits once the queue drains.
//
// The callback returns nothing. A consumer that wants the supervisor to act
// calls its API; an instruction returned from a notification would make the
// callback load-bearing, synchronous and re-entrant.
func WithOnFailure(fn func(Failure)) SupervisorOption {
	return func(s *Supervisor) { s.onFailure = fn }
}

// NewSupervisor returns a supervisor with no children.
func NewSupervisor(opts ...SupervisorOption) *Supervisor {
	s := &Supervisor{
		children: map[string]*supervisedChild{},
		stopped:  make(chan struct{}),
	}

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
//
// It is never closed. Ranging over it does not terminate at shutdown, so a
// consumer that wants a loop to end should select on it alongside its own done
// channel.
func (s *Supervisor) Failures() <-chan Failure {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.failuresOn {
		s.failures = make(chan Failure, DefaultFailureBufferSize)
		s.failuresOn = true
	}

	return s.failures
}

// DroppedReports is how many failures could not be delivered to the channel
// returned by [Supervisor.Failures] because it was full.
//
// It should be zero, and it counts that channel alone: the callback registered
// by [WithOnFailure] has its own unbounded queue and loses nothing. A drop
// nobody counts is indistinguishable from a system with nothing to report,
// which is the failure this whole type is arranged to avoid.
func (s *Supervisor) DroppedReports() int64 { return s.droppedRpts.Load() }

// Attach adds a child, before or after [Supervisor.Start].
//
// Attaching after Start is the point of this type: a Controller cannot do it,
// and says so — a late registration there "is never started, monitored, or
// stopped". A child attached here is supervised identically whenever it arrives.
//
// Once shutdown has begun it returns [ErrSupervisorStopped].
func (s *Supervisor) Attach(c Child) error {
	if c.Name == "" {
		return errors.New("controls: a child needs a name")
	}

	if c.Start == nil {
		return errors.Newf("controls: child %q has no Start", c.Name)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state >= supStopping {
		return errors.Wrapf(ErrSupervisorStopped, "attaching %q", c.Name)
	}

	if _, ok := s.children[c.Name]; ok {
		return errors.Wrapf(ErrChildAttached, "%q", c.Name)
	}

	child := &supervisedChild{
		spec:     c,
		policy:   copyRestartPolicy(c.RestartPolicy),
		done:     make(chan struct{}),
		stopDone: make(chan struct{}),
		state:    ChildPending,
	}
	s.children[c.Name] = child
	s.order = append(s.order, c.Name)

	if s.state == supRunning {
		// The supervisor's own context is the right parent: a child's lifetime
		// belongs to the supervisor, not to whoever happened to attach it.
		s.launch(s.ctx, child) //nolint:contextcheck // s.ctx is derived from Start's ctx; Attach has no caller context to inherit.
	}

	return nil
}

// Detach stops one child and forgets it. Every other child is untouched.
//
// It waits for the child to stop, bounded by ctx across both the child's own
// goroutine and its [Child.Stop]. If the budget expires the child is still
// forgotten and [ErrDetachTimeout] is returned, so a caller learns that a
// goroutine outlived its detach rather than discovering it later as a leak
// nothing reports.
//
// Detaching a child the supervisor never started is immediate and returns nil.
// Its Start was never called, so there is nothing to stop and nothing to wait
// for, and reporting a timeout for it would be a lie about what happened.
func (s *Supervisor) Detach(ctx context.Context, name string) error {
	s.mu.Lock()
	child, ok := s.children[name]

	if ok {
		delete(s.children, name)
		s.order = removeName(s.order, name)
	}

	launched := ok && child.launched
	s.mu.Unlock()

	if !ok {
		return errors.Wrapf(ErrChildNotAttached, "%q", name)
	}

	if !launched {
		return nil
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
// background from then on. A Supervisor is single use: after Stop it returns
// [ErrSupervisorStopped] rather than starting again.
func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch s.state {
	case supRunning:
		return errors.New("controls: the supervisor is already started")
	case supStopping, supStopped:
		return ErrSupervisorStopped
	case supNew:
	}

	sctx, cancel := context.WithCancel(ctx)
	s.ctx, s.cancel = sctx, cancel
	s.state = supRunning

	if s.onFailure != nil {
		s.queueWake = make(chan struct{}, 1)

		go s.dispatchLoop()
	}

	for _, name := range s.order {
		s.launch(sctx, s.children[name])
	}

	return nil
}

// Stop cancels every child at once and waits for all of them, bounded by ctx.
//
// Concurrent, not ordered: shutdown is bounded by the slowest child rather than
// by their number. Ten children taking 100ms each stop in about 100ms, where
// one at a time would take a second. The price is that children must not depend
// on each other — a consumer whose units do wants a Controller, which provides
// reverse-registration ordering deliberately.
//
// A child that outlives ctx is abandoned, the same bargain a Controller strikes
// at its shutdown deadline, because a stop that cannot end is worse than one
// that reports an incomplete result. [Supervisor.Health] still lists it.
//
// Calling Stop before Start stops nothing: no child's Start was called, so
// there is nothing to cancel and no [Child.Stop] to run. A second Stop waits
// for the first to finish rather than reporting a completion that has not
// happened.
func (s *Supervisor) Stop(ctx context.Context) {
	kids, mine := s.beginStop(ctx)
	if !mine {
		return
	}

	var wg sync.WaitGroup

	for _, c := range kids {
		wg.Add(1)

		go func(c *supervisedChild) {
			defer wg.Done()

			s.stopChild(ctx, c)

			select {
			case <-c.done:
			case <-ctx.Done():
			}
		}(c)
	}

	awaitBounded(ctx, wg.Wait)
	awaitBounded(ctx, s.wg.Wait)

	s.closeQueue()

	s.mu.Lock()
	s.state = supStopped
	close(s.stopped)
	s.mu.Unlock()
}

// beginStop claims the shutdown under the lock and reports whether this caller
// owns it. A caller that does not own it has already waited for the one that
// does, or had nothing to stop.
//
// The state transition and the snapshot happen in one critical section, which is
// what stops Attach adding a child the snapshot has already missed, and what
// makes every wg.Add strictly before the Wait below. Cancellation happens here
// too, so every child sees shutdown at once rather than in whatever order the
// per-child goroutines are scheduled.
func (s *Supervisor) beginStop(ctx context.Context) (kids []*supervisedChild, mine bool) {
	s.mu.Lock()

	switch s.state {
	case supStopped:
		s.mu.Unlock()

		return nil, false

	case supStopping:
		done := s.stopped
		s.mu.Unlock()

		select {
		case <-done:
		case <-ctx.Done():
		}

		return nil, false

	case supNew:
		// Nothing was ever launched, so there is nothing to wait on. Waiting on
		// a child's done channel here would block for ever: only the goroutine
		// launch spawns closes it, and none was spawned.
		s.state = supStopped
		close(s.stopped)
		s.mu.Unlock()

		return nil, false

	case supRunning:
	}

	s.state = supStopping
	kids = make([]*supervisedChild, 0, len(s.children))

	for _, c := range s.children {
		if c.launched {
			kids = append(kids, c)
		}
	}

	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()

	return kids, true
}

// awaitBounded runs wait on its own goroutine and returns when it finishes or
// ctx expires, whichever comes first. An abandoned wait is left to finish on its
// own, as Services.stop does at the controller's shutdown deadline.
func awaitBounded(ctx context.Context, wait func()) {
	done := make(chan struct{})

	go func() {
		defer close(done)

		wait()
	}()

	select {
	case <-done:
	case <-ctx.Done():
	}
}

// stopChild cancels a child and calls its Stop, once however many callers reach
// it, and returns when that has finished or ctx expires.
func (s *Supervisor) stopChild(ctx context.Context, c *supervisedChild) {
	c.stopOnce.Do(func() {
		s.mu.Lock()
		cancel := c.cancel
		s.mu.Unlock()

		if cancel != nil {
			cancel()
		}

		if c.spec.Stop == nil {
			close(c.stopDone)

			return
		}

		// A caller's Stop that blocks must not outlast the caller's budget, so
		// it runs on its own goroutine and the wait below is what ctx bounds.
		go func() {
			defer close(c.stopDone)

			_ = callStopFunc(ctx, c.spec.Stop)
		}()
	})

	select {
	case <-c.stopDone:
	case <-ctx.Done():
	}
}

// launch starts one child's supervision goroutine. The lock must be held, and
// parent is the supervisor's own context — taken as a parameter rather than
// read from the field so the flow is visible at every call site.
//
// wg.Add happens here, under the lock, and Stop transitions to stopping under
// the same lock before it waits. Attach refuses once stopping has begun, so an
// Add can never race the Wait.
func (s *Supervisor) launch(parent context.Context, c *supervisedChild) {
	ctx, cancel := context.WithCancel(parent)
	c.cancel = cancel
	c.launched = true

	s.wg.Add(1)

	go func() {
		defer s.wg.Done()
		defer close(c.done)
		// Without this, a child that ends on its own leaves a cancelCtx
		// registered against the supervisor's context for the process lifetime.
		defer cancel()

		s.supervise(ctx, c)
	}()
}

// supervise runs one child until it is cancelled or exhausts its policy.
//
// The restart machinery is the Services machinery — classifyOutcome,
// restartsExhausted, resolveRestartTimings, calculateNextBackoff and the
// reset-interval rule — rather than a second implementation of it. What differs
// is where a failure goes: a Service sends on the controller's error channel, a
// child produces a Failure for the consumer.
//
// classifyOutcome takes the child's own ValidError, so an expected terminal
// error means the same thing here as it does for a registered service.
func (s *Supervisor) supervise(ctx context.Context, c *supervisedChild) {
	// A nil policy means no restarts at all, and cannot be expressed through the
	// Service rule below — that reads MaxRestarts <= 0 as unlimited, so no value
	// of it means "never". Hence an explicit flag rather than a sentinel value
	// that would have to lie.
	noRestart := c.policy == nil

	policy := c.policy
	if policy == nil {
		policy = &RestartPolicy{}
	}

	timings := resolveRestartTimings(policy)
	restarts := 0

	for {
		c.setState(ChildRunning)

		runStarted := time.Now()
		err, panicked := s.runChildOnce(ctx, c)

		// Recorded before the outcome is classified. A child that panics on its
		// way out during shutdown classifies as cancelled, and recording after
		// that check threw the panic away — the half a consumer most wants.
		if err != nil {
			c.record(err, panicked)
		}

		if outcome := classifyOutcome(ctx, err, c.spec.ValidError); outcome != outcomeError {
			c.setState(ChildStopped)

			return
		}

		if time.Since(runStarted) >= timings.resetInterval {
			restarts = 0
			timings.backoff = initialBackoff(policy)
		}

		if noRestart || restartsExhausted(policy, restarts) {
			s.fail(c, restarts, err, panicked)

			return
		}

		restarts++
		c.setRestarts(restarts)
		c.setState(ChildBackoff)

		select {
		case <-time.After(timings.backoff):
			timings.backoff = calculateNextBackoff(timings.backoff, timings.maxBackoff)
		case <-ctx.Done():
			c.setState(ChildStopped)

			return
		}
	}
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
		Err:      exhaustedWith(fmt.Sprintf("controls: child %q exhausted its restart policy", c.spec.Name), err),
		Restarts: restarts,
		Panicked: panicked,
	}

	s.mu.Lock()
	ch, on := s.failures, s.failuresOn
	s.mu.Unlock()

	if on {
		select {
		case ch <- f:
		default:
			s.droppedRpts.Add(1)
		}
	}

	if s.onFailure != nil {
		s.enqueue(f)
	}
}

// enqueue adds a failure to the callback's ordered queue and wakes the dispatch
// goroutine. It never blocks the caller, which is a supervision goroutine.
func (s *Supervisor) enqueue(f Failure) {
	s.mu.Lock()

	if s.queueClosed {
		s.mu.Unlock()

		return
	}

	s.queue = append(s.queue, f)
	wake := s.queueWake
	s.mu.Unlock()

	select {
	case wake <- struct{}{}:
	default:
	}
}

// dispatchLoop delivers failures to the callback, one at a time and in order,
// until the queue is closed and drained.
//
// Nothing joins this goroutine. Stop closing the queue is what ends it, and a
// callback that calls Stop would otherwise be waiting for itself.
func (s *Supervisor) dispatchLoop() {
	for {
		s.mu.Lock()

		if len(s.queue) == 0 {
			closed, wake := s.queueClosed, s.queueWake
			s.mu.Unlock()

			if closed {
				return
			}

			<-wake

			continue
		}

		f := s.queue[0]
		s.queue = s.queue[1:]
		s.mu.Unlock()

		s.invoke(f)
	}
}

// invoke calls the consumer's callback behind a recover, so a panic in it kills
// neither the dispatch goroutine nor the process.
func (s *Supervisor) invoke(f Failure) {
	defer func() { _ = recover() }()

	s.onFailure(f)
}

func (s *Supervisor) closeQueue() {
	s.mu.Lock()
	s.queueClosed = true
	wake := s.queueWake
	s.mu.Unlock()

	if wake == nil {
		return
	}

	select {
	case wake <- struct{}{}:
	default:
	}
}

// copyRestartPolicy takes the caller's policy by value, as WithRestartPolicy
// does for a Service, so a caller reusing its struct cannot change a running
// child's rules underneath it. Nil is preserved: it is the never-restart marker.
func copyRestartPolicy(p *RestartPolicy) *RestartPolicy {
	if p == nil {
		return nil
	}

	c := *p

	return &c
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

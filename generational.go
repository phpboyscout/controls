package controls

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"gitlab.com/phpboyscout/go/errors"
)

// Errors returned by Generational. They are sentinels because a caller needs to
// distinguish "there is nothing to talk to" from "the thing said no", and that
// distinction must survive a process boundary.
var (
	// ErrNoGeneration is returned by Use when no generation is live: before the
	// first Start, during a stop, and after one. It is deliberately one error
	// rather than three, because the caller's response is the same.
	ErrNoGeneration = errors.NewSentinel("controls.no_generation", "controls: no live generation")

	// ErrGenerationRunning is returned by a Start that would have built a rival
	// generation alongside a live one.
	ErrGenerationRunning = errors.NewSentinel("controls.generation_running", "controls: a generation is already running")

	// ErrPredecessorLive is returned by a Start whose predecessor has not
	// finished releasing. It never reflects consumer code: a lease is bounded
	// by its own call, so only a Release that ignores its context can hold it.
	ErrPredecessorLive = errors.NewSentinel("controls.predecessor_live", "controls: the previous generation still holds resources")

	// ErrStopTimeout is returned by a Stop whose budget expired before the
	// generation's resources were released. The generation remains un-released
	// and a later Start is refused until it is: the error is semantic, not
	// decoration.
	ErrStopTimeout = errors.NewSentinel("controls.stop_timeout", "controls: stop budget expired before release completed")
)

// releaseAttempt bounds one call to Release. The reaper retries rather than
// abandoning, so this is a retry interval and not a deadline for the whole
// operation.
const releaseAttempt = 250 * time.Millisecond

// releaseAttempts bounds the synchronous release of a generation that lost a
// concurrent Start.
//
// It is bounded where the disposer's retry is not, because this one runs in the
// caller's goroutine: retrying for ever here would hang Start rather than
// merely refusing a later one, and the caller already has an error to act on.
const releaseAttempts = 4

// Generational owns one generation of R at a time, for a service that can be
// stopped and started again in-process.
//
// # Why this exists
//
// A restart re-invokes a service's [StartFunc], so anything that closure
// captured is shared across generations. Five times across this estate a module
// has captured something single-use — a supervisor, a gRPC server, an
// http.Server, an exit status, a per-tenant handle — and then failed in one of
// two ways: the restart could not work, or the stopped thing carried on looking
// healthy. Two properties answer those two failures, and neither subsumes the
// other:
//
//   - a new generation is built whole, from one recipe, never revived; and
//   - a stopped generation refuses further use, loudly.
//
// This type provides both. A [Supervisor] is single-use by design, so a service
// that owns one and must survive a restart wraps it here rather than reaching
// for a restartable supervisor.
//
// # Using it
//
//	g := &controls.Generational[*run]{
//		Build:   func(ctx context.Context) (*run, error) { return open(ctx) },
//		Release: func(ctx context.Context, r *run) error { return r.close(ctx) },
//	}
//
//	controller.Register("thing",
//		controls.WithStart(g.Start),
//		controls.WithStopErr(g.Stop),
//		controls.WithStatus(g.Healthy),
//	)
//
// The zero value is not usable: Build and Release are required. It is safe for
// concurrent use, and Use is on the hot path — it takes no mutex.
type Generational[R any] struct {
	// Build creates everything one generation needs. It is called once per
	// Start, and anything it acquires that needs releasing must be reachable
	// from the returned R, or it leaks. That obligation is the whole contract
	// this type places on a consumer.
	Build func(ctx context.Context) (R, error)

	// Release frees everything Build acquired. It must be idempotent, and it
	// must be safe to call concurrently with in-flight work that still holds a
	// lease, because a lease that outlives the stop budget is disowned rather
	// than waited for.
	//
	// A nil error means every resource is gone, and that is the only thing that
	// permits a new generation to start.
	Release func(ctx context.Context, r R) error

	// Probe reports the health of the current generation. Optional; nil means
	// always healthy while running.
	Probe func(r R) error

	// mu guards the start/stop transition and prev. It is never held across
	// Build or Release, both of which do I/O.
	mu   sync.Mutex
	gen  uint64
	prev *installed[R]

	// current is atomic so Use takes no lock: it is the hot path, and a mutex
	// there would put every call behind whatever Stop is waiting for.
	current atomic.Pointer[installed[R]]
}

// installed is one live generation and the machinery that retires it.
type installed[R any] struct {
	gen   uint64
	value R

	// admission is a gated refcount rather than a WaitGroup: WaitGroup
	// documents Add-after-Wait as a race, and a caller that loaded this
	// generation an instant before the gate shut is exactly that Add.
	open   atomic.Bool
	leases atomic.Int64

	idleOnce sync.Once
	idle     chan struct{}

	releaseOnce sync.Once
	done        chan struct{} // closed when Release has returned nil
}

// Start builds a generation and installs it.
//
// It refuses rather than overlapping: a second Start while one is live returns
// ErrGenerationRunning, and a Start whose predecessor is still releasing waits for
// it within ctx and then returns ErrPredecessorLive.
func (g *Generational[R]) Start(ctx context.Context) error {
	g.mu.Lock()

	if g.current.Load() != nil {
		g.mu.Unlock()

		return ErrGenerationRunning
	}

	prev := g.prev
	g.mu.Unlock()

	if prev != nil {
		select {
		case <-prev.done:
		case <-ctx.Done():
			return ErrPredecessorLive
		}
	}

	// Built outside the lock: Build does I/O, and holding a mutex across it
	// would make Stop unable to reach its own cancellation.
	value, err := g.Build(ctx)
	if err != nil {
		return err
	}

	g.mu.Lock()

	if g.current.Load() != nil {
		g.mu.Unlock()

		// Another Start won while this one was building. Dispose of what we
		// built rather than overwriting: an overwritten generation is wholly
		// live and unowned, which is the same defect as a partial install.
		//
		// Released SYNCHRONOUSLY, unlike a stopped generation. Nothing records
		// a loser, so nothing would ever wait for it: an asynchronous release
		// here means Start returns an error while the resources it built are
		// still live, and no later Start is gated on them. A Start that fails
		// must leave nothing behind.
		releaseErr := g.releaseNow(ctx, value)

		return errors.Join(ErrGenerationRunning, releaseErr)
	}

	g.gen++
	inst := &installed[R]{
		gen:   g.gen,
		value: value,
		idle:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	inst.open.Store(true)

	g.current.Store(inst)
	g.prev = nil
	g.mu.Unlock()

	return nil
}

// Use runs fn against the live generation, holding a lease so Release cannot
// begin while fn runs.
//
// It is the ONLY accessor. There is deliberately no method returning R for a
// caller to keep: a retained handle with no staleness protection is the defect
// this type exists to prevent, and it has already happened once in this estate.
func (g *Generational[R]) Use(fn func(R) error) error {
	inst := g.current.Load()
	if inst == nil || !inst.open.Load() {
		return ErrNoGeneration
	}

	inst.leases.Add(1)

	// Re-checked after the increment, which is what a WaitGroup cannot express:
	// without this, a lease acquired an instant before the gate shut would run
	// against a generation already being released.
	if !inst.open.Load() {
		g.release(inst)

		return ErrNoGeneration
	}

	defer g.release(inst)

	return fn(inst.value)
}

// Healthy reports the current generation's health, or ErrNoGeneration when there
// is none. A stopped generation is never reported healthy.
func (g *Generational[R]) Healthy() error {
	inst := g.current.Load()
	if inst == nil || !inst.open.Load() {
		return ErrNoGeneration
	}

	if g.Probe == nil {
		return nil
	}

	return g.Probe(inst.value)
}

// Generation is the current generation's number, or zero when none is live. It
// increases by one per successful Start and never repeats.
func (g *Generational[R]) Generation() uint64 {
	if inst := g.current.Load(); inst != nil {
		return inst.gen
	}

	return 0
}

// Stop closes admission, drains outstanding leases within ctx, and releases the
// generation's resources.
//
// It is bounded: it returns within the caller's budget whatever the consumer's
// code does. What it does not promise is that every leased call has returned —
// a call that ignores its own cancellation is disowned rather than waited for,
// because Go cannot end a goroutine. Resources are still released, and a later
// Start is refused until they are.
//
// It is idempotent, and stopping when nothing is running is not an error.
func (g *Generational[R]) Stop(ctx context.Context) error {
	g.mu.Lock()
	inst := g.current.Swap(nil)

	if inst != nil {
		g.prev = inst
	}

	g.mu.Unlock()

	if inst == nil {
		return nil
	}

	// Admission closes BEFORE the drain, so a caller using this continuously
	// cannot starve the drain.
	inst.open.Store(false)

	if inst.leases.Load() == 0 {
		inst.idleOnce.Do(func() { close(inst.idle) })
	}

	select {
	case <-inst.idle:
	case <-ctx.Done():
		// Leases outstanding past the budget are disowned. Release must be safe
		// against them, which is why that is in its contract.
	}

	g.dispose(ctx, inst)

	select {
	case <-inst.done:
		return nil
	case <-ctx.Done():
		return ErrStopTimeout
	}
}

// dispose releases a generation exactly once, retrying for as long as Release
// keeps failing.
//
// It deliberately never abandons: abandoning an unreleased generation is how a
// resource the next generation will duplicate stays alive, and a duplicate is
// worse than being unavailable. A consumer whose Release ignores its context
// therefore blocks reactivation, loudly, via ErrPredecessorLive.
// The caller's ctx is detached rather than dropped: its values survive, so
// tracing and logging carry through, while its cancellation does not. Running
// teardown under an already-cancelled context is the trap the old abandonStart
// fell into — it cancels the very work that is supposed to clean up.
// releaseNow releases a value the caller still owns, bounded and retried like
// the disposer but in the caller's own goroutine, so Start does not return
// until what it built is gone.
func (g *Generational[R]) releaseNow(parent context.Context, value R) error {
	detached := context.WithoutCancel(parent)

	for attempt := range releaseAttempts {
		ctx, cancel := context.WithTimeout(detached, releaseAttempt)
		err := g.Release(ctx, value)

		cancel()

		if err == nil {
			return nil
		}

		if attempt == releaseAttempts-1 {
			return errors.Wrap(err, "controls: releasing a generation that lost a concurrent start")
		}
	}

	return nil
}

func (g *Generational[R]) dispose(parent context.Context, inst *installed[R]) {
	detached := context.WithoutCancel(parent)

	inst.releaseOnce.Do(func() {
		go func() {
			for {
				ctx, cancel := context.WithTimeout(detached, releaseAttempt)
				err := g.Release(ctx, inst.value)

				cancel()

				if err == nil {
					break
				}
			}

			close(inst.done)
		}()
	})
}

func (g *Generational[R]) release(inst *installed[R]) {
	if inst.leases.Add(-1) == 0 && !inst.open.Load() {
		inst.idleOnce.Do(func() { close(inst.idle) })
	}
}

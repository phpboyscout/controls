package controls_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitlab.com/phpboyscout/go/errors"

	"gitlab.com/phpboyscout/go/controls"
)

// The wrong-order pairs. A Supervisor has four public lifecycle calls and the
// tests that get written walk them forwards. Every defect in this file was
// found by calling them in an order nothing had tried, and none of them needed
// concurrency to reproduce.

// within fails the test if fn has not returned within d. It is the shape every
// unbounded-wait defect takes, so it is worth having once.
func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()

	done := make(chan struct{})

	go func() { defer close(done); fn() }()

	select {
	case <-done:
	case <-time.After(d):
		t.Errorf("%s did not return within %s", what, d)
	}
}

// awaitTrue polls until cond holds or the budget expires, and fails saying what
// it was waiting for. Polling rather than sleeping-then-asserting is what stops
// a test racing state it has not been told is ready yet.
func awaitTrue(t *testing.T, d time.Duration, what string, cond func() bool) bool {
	t.Helper()

	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Errorf("timed out after %s waiting for %s", d, what)

	return false
}

// TestSupervisorStopBeforeStart — Stop must not wait on a child whose
// supervision goroutine was never launched, because nothing will ever close its
// done channel.
func TestSupervisorStopBeforeStart(t *testing.T) {
	t.Parallel()

	var ran atomic.Int64

	s := controls.NewSupervisor()
	_ = s.Attach(controls.Child{Name: "a", Start: blocksUntilCancelled(&ran)})

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	within(t, 2*time.Second, "Stop before Start", func() { s.Stop(ctx) })

	if got := ran.Load(); got != 0 {
		t.Errorf("the child ran %d times; Stop before Start must not start anything", got)
	}
}

// TestSupervisorStopBeforeStartSkipsChildStop — a child whose Start was never
// called has nothing to stop, and calling its Stop would be a lie about what
// happened to it.
func TestSupervisorStopBeforeStartSkipsChildStop(t *testing.T) {
	t.Parallel()

	var stopped atomic.Int64

	s := controls.NewSupervisor()
	_ = s.Attach(controls.Child{
		Name:  "a",
		Start: blocksUntilCancelled(nil),
		Stop:  func(context.Context) { stopped.Add(1) },
	})

	s.Stop(t.Context())

	if got := stopped.Load(); got != 0 {
		t.Errorf("Child.Stop was called %d times for a child that never started", got)
	}
}

// TestSupervisorDetachBeforeStart — the child never ran, so detaching it is a
// success, not a timeout.
func TestSupervisorDetachBeforeStart(t *testing.T) {
	t.Parallel()

	s := controls.NewSupervisor()
	_ = s.Attach(controls.Child{Name: "a", Start: blocksUntilCancelled(nil)})

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := s.Detach(ctx, "a")
	took := time.Since(start)

	if err != nil {
		t.Errorf("Detach before Start: %v, want nil for a child that never ran", err)
	}

	if took > 50*time.Millisecond {
		t.Errorf("Detach before Start took %s; it waited for a goroutine that does not exist", took)
	}

	if _, ok := s.Health()["a"]; ok {
		t.Error("the child is still reported after Detach")
	}
}

// TestSupervisorAttachAfterStop — accepting a child a stopped supervisor will
// never supervise is worse than refusing it: the caller believes it is running.
func TestSupervisorAttachAfterStop(t *testing.T) {
	t.Parallel()

	var ran atomic.Int64

	s := controls.NewSupervisor()
	_ = s.Start(t.Context())
	s.Stop(t.Context())

	err := s.Attach(controls.Child{Name: "late", Start: func(ctx context.Context) error {
		ran.Add(1)

		return ctx.Err()
	}})

	if !errors.Is(err, controls.ErrSupervisorStopped) {
		t.Errorf("Attach after Stop: %v, want %v", err, controls.ErrSupervisorStopped)
	}

	time.Sleep(100 * time.Millisecond)

	if got := ran.Load(); got != 0 {
		t.Errorf("the child ran %d times against a stopped supervisor", got)
	}
}

// TestSupervisorStartAfterStop — a Supervisor is single-use, and the error must
// say which rule was broken rather than reporting it as already running.
func TestSupervisorStartAfterStop(t *testing.T) {
	t.Parallel()

	s := controls.NewSupervisor()
	_ = s.Start(t.Context())
	s.Stop(t.Context())

	if err := s.Start(t.Context()); !errors.Is(err, controls.ErrSupervisorStopped) {
		t.Errorf("Start after Stop: %v, want %v", err, controls.ErrSupervisorStopped)
	}
}

// TestSupervisorStartTwice.
func TestSupervisorStartTwice(t *testing.T) {
	t.Parallel()

	s := controls.NewSupervisor()

	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err := s.Start(t.Context()); err == nil {
		t.Error("Start twice returned nil; the second call would relaunch every child")
	}

	s.Stop(t.Context())
}

// TestSupervisorStopTwice — the second caller must not return before shutdown
// has finished, or it reports a completion that has not happened.
func TestSupervisorStopTwice(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})

	s := controls.NewSupervisor()
	_ = s.Attach(controls.Child{Name: "slow", Start: func(ctx context.Context) error {
		<-ctx.Done()
		<-release

		return ctx.Err()
	}})
	_ = s.Start(t.Context())
	time.Sleep(50 * time.Millisecond)

	var (
		wg         sync.WaitGroup
		returned   [2]time.Time
		releasedAt time.Time
	)

	wg.Add(2)

	for i := range 2 {
		go func() {
			defer wg.Done()

			s.Stop(context.Background())

			returned[i] = time.Now()
		}()
	}

	time.Sleep(100 * time.Millisecond)
	releasedAt = time.Now()

	close(release)

	within(t, 2*time.Second, "two concurrent Stops", wg.Wait)

	// Both must return AFTER the child was let go. A second Stop that sees
	// stopping and returns early reports a completion that has not happened,
	// and its caller carries on tearing down what the child is still using.
	for i, at := range returned {
		if at.Before(releasedAt) {
			t.Errorf("Stop %d returned %s before the child was released", i, releasedAt.Sub(at))
		}
	}
}

// TestSupervisorConcurrentStartAndStop is the interleaving -race cannot see
// unless a test performs it.
func TestSupervisorConcurrentStartAndStop(t *testing.T) {
	t.Parallel()

	for range 50 {
		s := controls.NewSupervisor()
		_ = s.Attach(controls.Child{Name: "a", Start: blocksUntilCancelled(nil)})

		var wg sync.WaitGroup

		wg.Add(2)

		go func() { defer wg.Done(); _ = s.Start(context.Background()) }()

		go func() {
			defer wg.Done()

			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()

			s.Stop(ctx)
		}()

		within(t, 3*time.Second, "concurrent Start and Stop", wg.Wait)
	}
}

// TestSupervisorConcurrentAttachAndStop — Attach must not add a child to a
// supervisor that has already snapshotted its children, and must not touch a
// WaitGroup that is being waited on.
func TestSupervisorConcurrentAttachAndStop(t *testing.T) {
	t.Parallel()

	for i := range 200 {
		s := controls.NewSupervisor(controls.WithOnFailure(func(controls.Failure) {}))
		_ = s.Start(context.Background())

		var wg sync.WaitGroup

		wg.Add(1)

		go func() {
			defer wg.Done()

			_ = s.Attach(controls.Child{
				Name:  "late",
				Start: func(context.Context) error { return errors.New("boom") },
			})
		}()

		s.Stop(context.Background())
		wg.Wait()

		if i == 0 {
			continue
		}
	}
}

// TestSupervisorStopIsBoundedByItsContext — a child that ignores cancellation
// must not hold shutdown open for ever. The Controller abandons a stop at its
// deadline (services.go, awaitSupervisors); a Supervisor registered with one
// must not be the thing that outlives it.
func TestSupervisorStopIsBoundedByItsContext(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	defer close(release)

	s := controls.NewSupervisor()
	_ = s.Attach(controls.Child{Name: "stubborn", Start: func(context.Context) error {
		<-release

		return nil
	}})
	_ = s.Start(t.Context())
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	within(t, 2*time.Second, "Stop with a 100ms budget against a child that ignores cancellation",
		func() { s.Stop(ctx) })
}

// TestSupervisorDetachIsBoundedWhenChildStopBlocks — Detach's budget must cover
// the child's Stop callback, not only its Start goroutine.
func TestSupervisorDetachIsBoundedWhenChildStopBlocks(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	defer close(release)

	s := controls.NewSupervisor()
	_ = s.Attach(controls.Child{
		Name:  "a",
		Start: blocksUntilCancelled(nil),
		Stop:  func(context.Context) { <-release },
	})
	_ = s.Start(t.Context())
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	within(t, 2*time.Second, "Detach with a 100ms budget against a blocking Child.Stop",
		func() { _ = s.Detach(ctx, "a") })
}

// TestSupervisorStopIsBoundedWhenChildStopBlocks.
func TestSupervisorStopIsBoundedWhenChildStopBlocks(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	defer close(release)

	s := controls.NewSupervisor()
	_ = s.Attach(controls.Child{
		Name:  "a",
		Start: blocksUntilCancelled(nil),
		Stop:  func(context.Context) { <-release },
	})
	_ = s.Start(t.Context())
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	within(t, 2*time.Second, "Stop with a 100ms budget against a blocking Child.Stop",
		func() { s.Stop(ctx) })
}

// TestSupervisorChildStopRunsExactlyOnce — Detach racing Stop must not invoke a
// caller's shutdown callback twice. Most close a channel or dispose a resource,
// and the second call panics into a recover that reports nothing.
func TestSupervisorChildStopRunsExactlyOnce(t *testing.T) {
	t.Parallel()

	for range 200 {
		var stops atomic.Int64

		s := controls.NewSupervisor()
		_ = s.Attach(controls.Child{
			Name:  "a",
			Start: blocksUntilCancelled(nil),
			Stop:  func(context.Context) { stops.Add(1) },
		})
		_ = s.Start(context.Background())

		var wg sync.WaitGroup

		wg.Add(2)

		go func() { defer wg.Done(); _ = s.Detach(context.Background(), "a") }()
		go func() { defer wg.Done(); s.Stop(context.Background()) }()

		wg.Wait()

		if got := stops.Load(); got != 1 {
			t.Fatalf("Child.Stop ran %d times, want exactly 1", got)
		}
	}
}

// TestSupervisorReadinessAfterStop — a stopped supervisor reporting ready is
// the register/attach boundary failing in the other direction.
func TestSupervisorReadinessAfterStop(t *testing.T) {
	t.Parallel()

	s := controls.NewSupervisor()

	if err := s.Readiness(); !errors.Is(err, controls.ErrSupervisorNotStarted) {
		t.Errorf("Readiness before Start: %v, want %v", err, controls.ErrSupervisorNotStarted)
	}

	_ = s.Start(t.Context())

	if err := s.Readiness(); err != nil {
		t.Errorf("Readiness while running: %v, want nil", err)
	}

	s.Stop(t.Context())

	if err := s.Readiness(); !errors.Is(err, controls.ErrSupervisorStopped) {
		t.Errorf("Readiness after Stop: %v, want %v", err, controls.ErrSupervisorStopped)
	}
}

// TestSupervisorStopFromFailureCallback — the callback is documented as safe to
// call back into the supervisor from. Detach was the only case tested, and Stop
// deadlocked the dispatcher against itself.
func TestSupervisorStopFromFailureCallback(t *testing.T) {
	t.Parallel()

	returned := make(chan struct{})

	var s *controls.Supervisor

	s = controls.NewSupervisor(controls.WithOnFailure(func(controls.Failure) {
		s.Stop(context.Background())
		close(returned)
	}))

	_ = s.Attach(controls.Child{Name: "doomed", Start: alwaysFails})
	_ = s.Start(t.Context())

	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop called from the failure callback deadlocked against its own dispatch goroutine")
	}
}

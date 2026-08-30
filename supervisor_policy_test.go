package controls_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"gitlab.com/phpboyscout/go/errors"

	"gitlab.com/phpboyscout/go/controls"
)

// The claims the code makes and nothing asserted. Each of these was found by
// mutating the implementation and watching the suite stay green.

// TestSupervisorNilPolicyNeverRestarts — documented on Child.RestartPolicy, and
// the one rule that cannot be expressed through MaxRestarts, since a Service
// reads MaxRestarts <= 0 as unlimited.
func TestSupervisorNilPolicyNeverRestarts(t *testing.T) {
	t.Parallel()

	var runs atomic.Int64

	s := controls.NewSupervisor()
	_ = s.Attach(controls.Child{Name: "once", Start: func(context.Context) error {
		runs.Add(1)

		return errors.New("nope")
	}})
	_ = s.Start(t.Context())

	time.Sleep(settle)

	if got := runs.Load(); got != 1 {
		t.Errorf("a nil-policy child ran %d times, want exactly 1", got)
	}

	if st := s.Health()["once"].State; st != controls.ChildFailed {
		t.Errorf("state = %s, want failed", st)
	}

	s.Stop(t.Context())
}

// TestSupervisorZeroMaxRestartsIsUnlimited — MaxRestarts <= 0 means UNLIMITED
// for a Child exactly as for a Service. The first draft of the supervisor read
// it as never, and no test could tell.
func TestSupervisorZeroMaxRestartsIsUnlimited(t *testing.T) {
	t.Parallel()

	var runs atomic.Int64

	s := controls.NewSupervisor()
	_ = s.Attach(controls.Child{
		Name:          "spinner",
		RestartPolicy: &controls.RestartPolicy{InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond},
		Start: func(context.Context) error {
			runs.Add(1)

			return errors.New("nope")
		},
	})
	_ = s.Start(t.Context())

	time.Sleep(200 * time.Millisecond)
	s.Stop(t.Context())

	if got := runs.Load(); got < 10 {
		t.Errorf("ran %d times in 200ms with MaxRestarts 0; unlimited restarts are not happening", got)
	}

	if st := s.Health()["spinner"].State; st == controls.ChildFailed {
		t.Error("a child with unlimited restarts reached a terminal failure")
	}
}

// TestSupervisorBackoffGrows — the backoff arithmetic is shared with a Service
// and nothing measured it for a child. Timestamps, not a count.
func TestSupervisorBackoffGrows(t *testing.T) {
	t.Parallel()

	var (
		mu    = make(chan struct{}, 1)
		times []time.Time
	)

	mu <- struct{}{}

	s := controls.NewSupervisor()
	_ = s.Attach(controls.Child{
		Name:          "flappy",
		RestartPolicy: &controls.RestartPolicy{MaxRestarts: 3, InitialBackoff: 40 * time.Millisecond, MaxBackoff: time.Second},
		Start: func(context.Context) error {
			<-mu
			times = append(times, time.Now())
			mu <- struct{}{}

			return errors.New("nope")
		},
	})
	_ = s.Start(t.Context())

	time.Sleep(700 * time.Millisecond)
	s.Stop(t.Context())

	<-mu

	if len(times) != 4 {
		t.Fatalf("ran %d times, want 4 (initial + 3 restarts)", len(times))
	}

	gaps := []time.Duration{times[1].Sub(times[0]), times[2].Sub(times[1]), times[3].Sub(times[2])}

	for i, want := range []time.Duration{40 * time.Millisecond, 80 * time.Millisecond, 160 * time.Millisecond} {
		if gaps[i] < want/2 {
			t.Errorf("gap %d was %s, want at least %s; the backoff is not being applied", i+1, gaps[i], want/2)
		}
	}

	if gaps[1] <= gaps[0] || gaps[2] <= gaps[1] {
		t.Errorf("gaps %v do not grow; the backoff is not doubling", gaps)
	}
}

// TestSupervisorChildStopIsCalled — a documented public field that no test set.
func TestSupervisorChildStopIsCalled(t *testing.T) {
	t.Parallel()

	var stops atomic.Int64

	s := controls.NewSupervisor()
	_ = s.Attach(controls.Child{
		Name:  "a",
		Start: blocksUntilCancelled(nil),
		Stop:  func(context.Context) { stops.Add(1) },
	})
	_ = s.Start(t.Context())
	time.Sleep(50 * time.Millisecond)

	s.Stop(t.Context())

	if got := stops.Load(); got != 1 {
		t.Errorf("Child.Stop ran %d times on shutdown, want 1", got)
	}
}

// TestSupervisorPanickingChildStopIsContained — callStop draws the line, and
// nothing proved a child's Stop was behind it.
func TestSupervisorPanickingChildStopIsContained(t *testing.T) {
	t.Parallel()

	s := controls.NewSupervisor()
	_ = s.Attach(controls.Child{
		Name:  "a",
		Start: blocksUntilCancelled(nil),
		Stop:  func(context.Context) { panic("boom") },
	})
	_ = s.Attach(controls.Child{Name: "b", Start: blocksUntilCancelled(nil)})
	_ = s.Start(t.Context())
	time.Sleep(50 * time.Millisecond)

	within(t, 2*time.Second, "Stop with a panicking Child.Stop", func() { s.Stop(t.Context()) })
}

// TestSupervisorFailurePanickedIsReported — Failure.Panicked exists so a
// consumer can tell a bug from a capacity problem, and no test read it.
func TestSupervisorFailurePanickedIsReported(t *testing.T) {
	t.Parallel()

	s := controls.NewSupervisor()
	ch := s.Failures()

	_ = s.Attach(controls.Child{Name: "panicky", Start: func(context.Context) error { panic("boom") }})
	_ = s.Attach(controls.Child{Name: "erroring", Start: alwaysFails})
	_ = s.Start(t.Context())

	seen := map[string]controls.Failure{}

	for range 2 {
		select {
		case f := <-ch:
			seen[f.Name] = f
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d failures arrived", len(seen))
		}
	}

	if !seen["panicky"].Panicked {
		t.Error("a panicking child reported Panicked false")
	}

	if seen["erroring"].Panicked {
		t.Error("a child that returned an error reported Panicked true")
	}

	s.Stop(t.Context())
}

// TestSupervisorPanicDuringShutdownIsCounted — the panic is the interesting
// half, and classifying the run as cancelled first threw it away.
func TestSupervisorPanicDuringShutdownIsCounted(t *testing.T) {
	t.Parallel()

	s := controls.NewSupervisor()
	_ = s.Attach(controls.Child{Name: "a", Start: func(ctx context.Context) error {
		<-ctx.Done()

		panic("boom on the way out")
	}})
	_ = s.Start(t.Context())
	time.Sleep(50 * time.Millisecond)

	s.Stop(t.Context())

	if got := s.Health()["a"].Panics; got != 1 {
		t.Errorf("Panics = %d for a child that panicked during shutdown, want 1", got)
	}
}

// TestSupervisorCancelledErrorIsNotAFailure — parity with a Service, which
// treats context.Canceled as a clean cancellation even when its own context is
// still live (services.go, classifyRun).
func TestSupervisorCancelledErrorIsNotAFailure(t *testing.T) {
	t.Parallel()

	var runs atomic.Int64

	s := controls.NewSupervisor()
	_ = s.Attach(controls.Child{
		Name:          "a",
		RestartPolicy: policy(3),
		Start: func(context.Context) error {
			runs.Add(1)

			return context.Canceled
		},
	})
	_ = s.Start(t.Context())

	time.Sleep(settle)

	if got := runs.Load(); got != 1 {
		t.Errorf("a child returning context.Canceled ran %d times, want 1; a Service would not be restarted", got)
	}

	if st := s.Health()["a"].State; st == controls.ChildFailed {
		t.Error("a child returning context.Canceled was reported as a failure")
	}

	s.Stop(t.Context())
}

// TestSupervisorHealthCheckIsHealthyWithNoFailures — the healthy path, and the
// message, neither of which a five-of-five fixture can constrain.
func TestSupervisorHealthCheckIsHealthyWithNoFailures(t *testing.T) {
	t.Parallel()

	s := controls.NewSupervisor()
	_ = s.Attach(controls.Child{Name: "a", Start: blocksUntilCancelled(nil)})
	_ = s.Attach(controls.Child{Name: "b", Start: blocksUntilCancelled(nil)})
	_ = s.Attach(controls.Child{Name: "doomed", Start: alwaysFails})
	_ = s.Start(t.Context())

	check := s.HealthCheck("workers")

	if check.Name != "workers" {
		t.Errorf("HealthCheck.Name = %q, want the name it was given", check.Name)
	}

	if check.Type != controls.CheckTypeReadiness {
		t.Errorf("HealthCheck.Type = %v, want CheckTypeReadiness", check.Type)
	}

	time.Sleep(settle)

	res := check.Check(t.Context())
	if res.Status != controls.CheckDegraded {
		t.Fatalf("status = %v with one child failed, want CheckDegraded", res.Status)
	}

	if res.Message != "1 of 3 supervised children have failed" {
		t.Errorf("message = %q, want \"1 of 3 supervised children have failed\"", res.Message)
	}

	if err := s.Detach(t.Context(), "doomed"); err != nil {
		t.Fatal(err)
	}

	if res := check.Check(t.Context()); res.Status != controls.CheckHealthy {
		t.Errorf("status = %v with no failed children, want CheckHealthy", res.Status)
	}

	s.Stop(t.Context())
}

// TestSupervisorAttachRejectsBadChildren.
func TestSupervisorAttachRejectsBadChildren(t *testing.T) {
	t.Parallel()

	s := controls.NewSupervisor()

	if err := s.Attach(controls.Child{Start: blocksUntilCancelled(nil)}); err == nil {
		t.Error("a child with no name was accepted")
	}

	if err := s.Attach(controls.Child{Name: "a"}); err == nil {
		t.Error("a child with no Start was accepted")
	}

	if err := s.Attach(controls.Child{Name: "a", Start: blocksUntilCancelled(nil)}); err != nil {
		t.Fatal(err)
	}

	err := s.Attach(controls.Child{Name: "a", Start: blocksUntilCancelled(nil)})
	if !errors.Is(err, controls.ErrChildAttached) {
		t.Errorf("attaching a duplicate name: %v, want %v", err, controls.ErrChildAttached)
	}
}

// TestSupervisorRestartPolicyIsCopied — a Service gets a defensive copy through
// WithRestartPolicy; a Child handed a pointer must not read the caller's later
// edits mid-supervision.
func TestSupervisorRestartPolicyIsCopied(t *testing.T) {
	t.Parallel()

	var runs atomic.Int64

	p := controls.RestartPolicy{MaxRestarts: 2, InitialBackoff: 5 * time.Millisecond, MaxBackoff: 10 * time.Millisecond}

	s := controls.NewSupervisor()
	_ = s.Attach(controls.Child{Name: "a", RestartPolicy: &p, Start: func(context.Context) error {
		runs.Add(1)

		return errors.New("nope")
	}})

	p.MaxRestarts = 99 // the caller reuses its policy struct

	_ = s.Start(t.Context())
	time.Sleep(settle)

	if got := runs.Load(); got != 3 {
		t.Errorf("ran %d times, want 3 (initial + 2 restarts); the policy was read after Attach", got)
	}

	s.Stop(t.Context())
}

// TestSupervisorNoDropsWhenDrained — DroppedReports is documented as "should be
// zero", and only ever asserted non-zero.
func TestSupervisorNoDropsWhenDrained(t *testing.T) {
	t.Parallel()

	s := controls.NewSupervisor()
	ch := s.Failures()

	const children = 8

	for i := range children {
		_ = s.Attach(controls.Child{Name: string(rune('a' + i)), Start: alwaysFails})
	}

	_ = s.Start(t.Context())

	for range children {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatal("a failure never arrived")
		}
	}

	if got := s.DroppedReports(); got != 0 {
		t.Errorf("DroppedReports = %d with a drained channel, want 0", got)
	}

	s.Stop(t.Context())
}

// TestSupervisorSlowCallbackLosesNothing — D7 promises delivery by callback.
// The channel is opt-in and sheddable; the callback is neither.
func TestSupervisorSlowCallbackLosesNothing(t *testing.T) {
	t.Parallel()

	const children = 40

	release := make(chan struct{})
	seen := make(chan string, children*2)

	s := controls.NewSupervisor(controls.WithOnFailure(func(f controls.Failure) {
		<-release
		seen <- f.Name
	}))

	for i := range children {
		_ = s.Attach(controls.Child{
			Name:  "c" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			Start: alwaysFails,
		})
	}

	_ = s.Start(t.Context())
	time.Sleep(settle)
	close(release)

	deadline := time.After(5 * time.Second)

	for range children {
		select {
		case <-seen:
		case <-deadline:
			t.Fatalf("the callback was invoked %d times for %d failures; notifications were dropped",
				len(seen), children)
		}
	}

	s.Stop(t.Context())
}

// TestSupervisorEndedChildReleasesItsContext — a child that ends on its own is
// never reached by Stop or Detach, so if its supervision goroutine does not
// release the context it derived, a cancelCtx stays registered against the
// supervisor's for the process lifetime. One per tenant, in the use case the
// docs advertise.
func TestSupervisorEndedChildReleasesItsContext(t *testing.T) {
	t.Parallel()

	got := make(chan context.Context, 1)

	s := controls.NewSupervisor()
	_ = s.Attach(controls.Child{Name: "doomed", Start: func(ctx context.Context) error {
		select {
		case got <- ctx:
		default:
		}

		return errors.New("nope")
	}})
	_ = s.Start(t.Context())

	var childCtx context.Context

	select {
	case childCtx = <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("the child never ran")
	}

	if !awaitTrue(t, 2*time.Second, "the child to reach failed", func() bool {
		return s.Health()["doomed"].State == controls.ChildFailed
	}) {
		return
	}

	// The supervisor is still running, so nothing else has cancelled it.
	if err := s.Readiness(); err != nil {
		t.Fatalf("Readiness = %v; the supervisor stopped and the assertion below would be meaningless", err)
	}

	// Poll rather than assert once. setState(ChildFailed) happens inside fail(),
	// and the deferred cancel() runs when supervise RETURNS, so the state is
	// observable a moment before the context is released. Asserting on sight of
	// the state flaked about one run in fifteen under -race.
	awaitTrue(t, 2*time.Second,
		"the ended child's context to be released; a cancelCtx registered against the supervisor's is a leak",
		func() bool { return childCtx.Err() != nil })

	s.Stop(t.Context())
}

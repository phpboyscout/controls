package controls_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"gitlab.com/phpboyscout/go/errors"

	"gitlab.com/phpboyscout/go/controls"
)

func alwaysFails(context.Context) error { return errors.New("nope") }

// TestSupervisorFailedChildNeverMakesItUnready is the register/attach boundary,
// and the assertion most likely to be broken by somebody making health "more
// accurate". A Supervisor is registered with a Controller, and a registered
// service whose probe fails takes the WHOLE PROCESS out of rotation.
func TestSupervisorFailedChildNeverMakesItUnready(t *testing.T) {
	t.Parallel()

	s := controls.NewSupervisor()

	// Every child fails. Not one: all of them.
	for i := range 5 {
		_ = s.Attach(controls.Child{Name: string(rune('a' + i)), RestartPolicy: policy(1), Start: alwaysFails})
	}

	_ = s.Start(t.Context())
	time.Sleep(settle)

	failed := 0

	for _, st := range s.Health() {
		if st.State == controls.ChildFailed {
			failed++
		}
	}

	if failed != 5 {
		t.Fatalf("%d children failed, want 5 — the fixture is not testing what it claims", failed)
	}

	if err := s.Readiness(); err != nil {
		t.Errorf("Readiness = %v with every child dead; a failed child must never make the supervisor unready", err)
	}

	res := s.HealthCheck("bus").Check(t.Context())
	if res.Status != controls.CheckDegraded {
		t.Errorf("health check = %v, want CheckDegraded", res.Status)
	}

	s.Stop(t.Context())
}

// TestSupervisorFailureReachesBothMechanisms.
func TestSupervisorFailureReachesBothMechanisms(t *testing.T) {
	t.Parallel()

	var called atomic.Int64

	got := make(chan controls.Failure, 1)

	s := controls.NewSupervisor(controls.WithOnFailure(func(f controls.Failure) {
		called.Add(1)

		select {
		case got <- f:
		default:
		}
	}))

	ch := s.Failures()

	_ = s.Attach(controls.Child{Name: "doomed", RestartPolicy: policy(1), Start: alwaysFails})
	_ = s.Start(t.Context())

	awaitFailure(t, ch, "the channel")
	awaitFailure(t, got, "the callback")

	s.Stop(t.Context())
}

// awaitFailure asserts one Failure arrives on a channel and describes the
// doomed child correctly.
func awaitFailure(t *testing.T, ch <-chan controls.Failure, via string) {
	t.Helper()

	select {
	case f := <-ch:
		if f.Name != "doomed" {
			t.Errorf("%s: Failure.Name = %q, want \"doomed\"", via, f.Name)
		}

		if f.Restarts != 1 {
			t.Errorf("%s: Failure.Restarts = %d, want 1", via, f.Restarts)
		}

		if f.Err == nil {
			t.Errorf("%s: Failure.Err is nil", via)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: no failure arrived", via)
	}
}

// TestSupervisorUndrainedChannelDoesNotStall — a reporting mechanism must not be
// able to stall the thing it reports on, and a drop nobody counts is
// indistinguishable from a system with nothing to say.
func TestSupervisorUndrainedChannelDoesNotStall(t *testing.T) {
	t.Parallel()

	s := controls.NewSupervisor()
	_ = s.Failures() // opt in, and never drain it

	const children = 40

	for i := range children {
		_ = s.Attach(controls.Child{Name: "c" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			RestartPolicy: policy(1), Start: alwaysFails})
	}

	done := make(chan struct{})

	go func() {
		_ = s.Start(t.Context())
		time.Sleep(settle)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor stalled on an undrained failure channel")
	}

	failed := 0

	for _, st := range s.Health() {
		if st.State == controls.ChildFailed {
			failed++
		}
	}

	if failed != children {
		t.Errorf("%d of %d children reached failed; supervision stalled", failed, children)
	}

	if s.DroppedReports() == 0 {
		t.Errorf("%d failures against a %d-slot channel and DroppedReports() is 0; drops are not counted",
			children, controls.DefaultFailureBufferSize)
	}

	s.Stop(t.Context())
}

// TestSupervisorCallbackMayReenter — Detach on the child that just died is the
// first thing anybody will write. If notification held a lock it would hang.
func TestSupervisorCallbackMayReenter(t *testing.T) {
	t.Parallel()

	detached := make(chan error, 1)

	var s *controls.Supervisor

	s = controls.NewSupervisor(controls.WithOnFailure(func(f controls.Failure) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		select {
		case detached <- s.Detach(ctx, f.Name):
		default:
		}
	}))

	_ = s.Attach(controls.Child{Name: "doomed", RestartPolicy: policy(1), Start: alwaysFails})
	_ = s.Start(t.Context())

	select {
	case err := <-detached:
		if err != nil {
			t.Errorf("re-entrant Detach: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a callback that called back into the supervisor deadlocked")
	}

	s.Stop(t.Context())
}

// TestSupervisorAndServiceRestartAlike is D3's claim, asserted rather than
// assumed: the shared machinery now has two callers and only one of them had
// tests before.
func TestSupervisorAndServiceRestartAlike(t *testing.T) {
	t.Parallel()

	p := controls.RestartPolicy{MaxRestarts: 3, InitialBackoff: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond}

	var serviceRuns, childRuns atomic.Int64

	ctrl := controls.NewController(t.Context())
	ctrl.Register("svc",
		controls.WithStart(func(context.Context) error {
			serviceRuns.Add(1)

			return errors.New("nope")
		}),
		controls.WithRestartPolicy(p),
	)
	ctrl.Start()

	sup := controls.NewSupervisor()
	_ = sup.Attach(controls.Child{Name: "child", RestartPolicy: &p, Start: func(context.Context) error {
		childRuns.Add(1)

		return errors.New("nope")
	}})
	_ = sup.Start(t.Context())

	time.Sleep(600 * time.Millisecond)

	sup.Stop(t.Context())
	ctrl.Stop()

	if serviceRuns.Load() != childRuns.Load() {
		t.Errorf("a Service ran %d times and a Child ran %d with the same policy; the shared rule has diverged",
			serviceRuns.Load(), childRuns.Load())
	}
}

package controls_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"gitlab.com/phpboyscout/go/errors"

	"gitlab.com/phpboyscout/go/controls"
)

const settle = 400 * time.Millisecond

// blocksUntilCancelled is a healthy child: it serves until told to stop.
func blocksUntilCancelled(started *atomic.Int64) controls.StartFunc {
	return func(ctx context.Context) error {
		if started != nil {
			started.Add(1)
		}

		<-ctx.Done()

		return ctx.Err()
	}
}

func policy(maxRestarts int) *controls.RestartPolicy {
	return &controls.RestartPolicy{
		MaxRestarts:    maxRestarts,
		InitialBackoff: 5 * time.Millisecond,
		MaxBackoff:     20 * time.Millisecond,
	}
}

// TestSupervisorPanicIsContainedAndSiblingsSurvive is the assertion the type
// exists for. A test that only checks the process survived would pass against a
// supervisor that took everything down and restarted it, so the sibling is
// asserted separately.
func TestSupervisorPanicIsContainedAndSiblingsSurvive(t *testing.T) {
	t.Parallel()

	var healthy, panicky atomic.Int64

	s := controls.NewSupervisor()

	if err := s.Attach(controls.Child{Name: "healthy", Start: blocksUntilCancelled(&healthy)}); err != nil {
		t.Fatal(err)
	}

	if err := s.Attach(controls.Child{Name: "panicky", RestartPolicy: policy(2), Start: func(context.Context) error {
		panicky.Add(1)
		panic("boom")
	}}); err != nil {
		t.Fatal(err)
	}

	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}

	time.Sleep(settle)
	h := s.Health()

	if h["healthy"].State != controls.ChildRunning {
		t.Errorf("the healthy sibling is %s, want running", h["healthy"].State)
	}

	if h["panicky"].State != controls.ChildFailed {
		t.Errorf("the panicking child is %s, want failed", h["panicky"].State)
	}

	if h["panicky"].Panics == 0 {
		t.Error("panics were not counted")
	}

	if got := panicky.Load(); got != 3 {
		t.Errorf("panicking child ran %d times, want 3 (initial + 2 restarts)", got)
	}

	s.Stop(t.Context())
}

// TestSupervisorCrashLoopIsBounded — a failing child must land in a terminal
// state rather than spinning for ever.
func TestSupervisorCrashLoopIsBounded(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int64

	s := controls.NewSupervisor()
	_ = s.Attach(controls.Child{Name: "loop", RestartPolicy: policy(3), Start: func(context.Context) error {
		attempts.Add(1)

		return errors.New("always fails")
	}})
	_ = s.Start(t.Context())

	time.Sleep(settle)

	if got := attempts.Load(); got != 4 {
		t.Errorf("ran %d times, want 4 (initial + 3 restarts)", got)
	}

	if st := s.Health()["loop"].State; st != controls.ChildFailed {
		t.Errorf("state = %s, want failed", st)
	}

	s.Stop(t.Context())
}

// TestSupervisorAttachAfterStart is the asymmetry a Controller has and a
// Supervisor must not.
func TestSupervisorAttachAfterStart(t *testing.T) {
	t.Parallel()

	var late atomic.Int64

	s := controls.NewSupervisor()
	_ = s.Start(t.Context())
	time.Sleep(30 * time.Millisecond)

	if err := s.Attach(controls.Child{Name: "late", RestartPolicy: policy(2), Start: func(ctx context.Context) error {
		if late.Add(1) == 1 {
			return errors.New("first run fails, to prove it is supervised")
		}

		<-ctx.Done()

		return ctx.Err()
	}}); err != nil {
		t.Fatal(err)
	}

	time.Sleep(settle)

	if got := late.Load(); got < 2 {
		t.Errorf("late child ran %d times; it was not restarted, so it was not supervised", got)
	}

	if st := s.Health()["late"].State; st != controls.ChildRunning {
		t.Errorf("state = %s, want running", st)
	}

	s.Stop(t.Context())
}

// TestSupervisorDetach asserts both halves: the child stops, the sibling does not.
func TestSupervisorDetach(t *testing.T) {
	t.Parallel()

	s := controls.NewSupervisor()
	_ = s.Attach(controls.Child{Name: "a", Start: blocksUntilCancelled(nil)})
	_ = s.Attach(controls.Child{Name: "b", Start: blocksUntilCancelled(nil)})
	_ = s.Start(t.Context())
	time.Sleep(50 * time.Millisecond)

	if err := s.Detach(t.Context(), "a"); err != nil {
		t.Fatalf("Detach: %v", err)
	}

	h := s.Health()

	if _, ok := h["a"]; ok {
		t.Error("the detached child is still reported")
	}

	if h["b"].State != controls.ChildRunning {
		t.Errorf("the sibling is %s, want running", h["b"].State)
	}

	if err := s.Detach(t.Context(), "a"); !errors.Is(err, controls.ErrChildNotAttached) {
		t.Errorf("detaching twice: %v, want %v", err, controls.ErrChildNotAttached)
	}

	s.Stop(t.Context())
}

// TestSupervisorDetachTimeout — a child that outlives its budget is still
// forgotten, and the caller is told rather than discovering a leak later.
func TestSupervisorDetachTimeout(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})

	s := controls.NewSupervisor()
	_ = s.Attach(controls.Child{Name: "stubborn", Start: func(ctx context.Context) error {
		<-ctx.Done()
		<-release

		return ctx.Err()
	}})
	_ = s.Start(t.Context())
	time.Sleep(30 * time.Millisecond)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := s.Detach(ctx, "stubborn")
	took := time.Since(start)

	if !errors.Is(err, controls.ErrDetachTimeout) {
		t.Errorf("Detach: %v, want %v", err, controls.ErrDetachTimeout)
	}

	if took > 200*time.Millisecond {
		t.Errorf("Detach took %s; it did not respect the budget", took)
	}

	close(release)
	s.Stop(t.Context())
}

// TestSupervisorStopIsConcurrent — bounded by the slowest child, not by their
// number. Ten children at 100ms each: about 100ms, not a second.
func TestSupervisorStopIsConcurrent(t *testing.T) {
	t.Parallel()

	s := controls.NewSupervisor()

	for i := range 10 {
		_ = s.Attach(controls.Child{Name: string(rune('a' + i)), Start: func(ctx context.Context) error {
			<-ctx.Done()
			time.Sleep(100 * time.Millisecond)

			return ctx.Err()
		}})
	}

	_ = s.Start(t.Context())
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	s.Stop(t.Context())
	took := time.Since(start)

	if took > 500*time.Millisecond {
		t.Errorf("stopping ten children took %s; sequential would be ~1s, concurrent ~100ms", took)
	}
}

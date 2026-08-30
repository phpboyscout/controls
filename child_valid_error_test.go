package controls_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"gitlab.com/phpboyscout/go/errors"

	"gitlab.com/phpboyscout/go/controls"
)

// Issue 6. A Service can declare an expected terminal error through the
// controller's WithValidError; a Child could not, so the canonical case, an
// HTTP server returning http.ErrServerClosed on a clean shutdown, was a restart
// storm ending in a terminal Failure.

// TestChildValidErrorIsNotAFailure is the case the issue was raised for.
func TestChildValidErrorIsNotAFailure(t *testing.T) {
	t.Parallel()

	var runs atomic.Int64

	s := controls.NewSupervisor()
	ch := s.Failures()

	_ = s.Attach(controls.Child{
		Name:          "api",
		RestartPolicy: policy(3),
		ValidError:    func(err error) bool { return errors.Is(err, http.ErrServerClosed) },
		Start: func(context.Context) error {
			runs.Add(1)

			return http.ErrServerClosed
		},
	})
	_ = s.Start(t.Context())

	time.Sleep(settle)

	if got := runs.Load(); got != 1 {
		t.Errorf("the child ran %d times, want 1; a valid terminal error must not restart", got)
	}

	if st := s.Health()["api"].State; st == controls.ChildFailed {
		t.Error("a child returning its valid error was reported as failed")
	}

	select {
	case f := <-ch:
		t.Errorf("a Failure was delivered for a valid terminal error: %v", f.Err)
	default:
	}

	s.Stop(t.Context())
}

// TestChildValidErrorOnlyExemptsWhatItMatches — the predicate must not become a
// blanket exemption. Without this, returning true unconditionally would pass the
// test above and nothing would notice.
func TestChildValidErrorOnlyExemptsWhatItMatches(t *testing.T) {
	t.Parallel()

	var runs atomic.Int64

	s := controls.NewSupervisor()

	_ = s.Attach(controls.Child{
		Name:          "api",
		RestartPolicy: policy(2),
		ValidError:    func(err error) bool { return errors.Is(err, http.ErrServerClosed) },
		Start: func(context.Context) error {
			runs.Add(1)

			return errors.New("a real failure")
		},
	})
	_ = s.Start(t.Context())

	time.Sleep(settle)

	if got := runs.Load(); got != 3 {
		t.Errorf("the child ran %d times, want 3 (initial + 2 restarts); an unmatched error must still fail", got)
	}

	if st := s.Health()["api"].State; st != controls.ChildFailed {
		t.Errorf("state = %s, want failed", st)
	}

	s.Stop(t.Context())
}

// TestChildAndServiceAgreeOnValidError is the parity claim spec 0002 D3 makes,
// for the capability that was missing when it was written.
func TestChildAndServiceAgreeOnValidError(t *testing.T) {
	t.Parallel()

	valid := func(err error) bool { return errors.Is(err, http.ErrServerClosed) }

	var serviceRuns, childRuns atomic.Int64

	ctrl := controls.NewController(t.Context(), controls.WithValidError(valid))
	ctrl.Register("svc",
		controls.WithStart(func(context.Context) error {
			serviceRuns.Add(1)

			return http.ErrServerClosed
		}),
		controls.WithRestartPolicy(*policy(3)),
	)
	ctrl.Start()

	sup := controls.NewSupervisor()
	_ = sup.Attach(controls.Child{
		Name:          "child",
		RestartPolicy: policy(3),
		ValidError:    valid,
		Start: func(context.Context) error {
			childRuns.Add(1)

			return http.ErrServerClosed
		},
	})
	_ = sup.Start(t.Context())

	time.Sleep(settle)

	sup.Stop(t.Context())
	ctrl.Stop()

	if serviceRuns.Load() != childRuns.Load() {
		t.Errorf("a Service ran %d times and a Child ran %d with the same valid-error predicate",
			serviceRuns.Load(), childRuns.Load())
	}

	if got := childRuns.Load(); got != 1 {
		t.Errorf("both ran %d times, want 1; a valid terminal error must not restart either", got)
	}
}

// TestChildWithNoValidErrorIsUnchanged — the field is optional and nil must mean
// what it meant before it existed.
func TestChildWithNoValidErrorIsUnchanged(t *testing.T) {
	t.Parallel()

	var runs atomic.Int64

	s := controls.NewSupervisor()
	_ = s.Attach(controls.Child{
		Name:          "api",
		RestartPolicy: policy(2),
		Start: func(context.Context) error {
			runs.Add(1)

			return http.ErrServerClosed
		},
	})
	_ = s.Start(t.Context())

	time.Sleep(settle)

	if got := runs.Load(); got != 3 {
		t.Errorf("ran %d times, want 3; with no predicate ErrServerClosed is an ordinary error", got)
	}

	s.Stop(t.Context())
}

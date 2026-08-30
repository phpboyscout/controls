package controls

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// Issue 7. runCheck derived its timeout from the caller's context and then
// selected on ctx.Done(), so once the controller's context was cancelled every
// check reported "health check timed out" however fast it returned. Both arms
// of that select were ready, so the result was a coin toss and the message was
// false either way, and it was indistinguishable from a check that really hung.

// TestCheckWithCancelledParentIsDeterministicAndNotATimeout runs enough times
// that any nondeterminism would show. A single run could pass against the bug.
//
// The first draft of this test asserted the check's own result. That was the
// wrong expectation: with a dead parent the check has not run, so there is no
// real result to report, and inventing one would be as false as the timeout
// message was. What the contract owes is a truthful, stable answer.
func TestCheckWithCancelledParentIsDeterministicAndNotATimeout(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var invoked atomic.Int64

	for i := range 200 {
		e := &healthCheckEntry{
			check: HealthCheck{
				Name: "fast",
				Check: func(context.Context) CheckResult {
					invoked.Add(1)

					return CheckResult{Status: CheckHealthy, Message: "fine"}
				},
			},
		}

		e.runCheck(ctx)

		r := e.lastResult.Load()
		if r == nil {
			t.Fatalf("iteration %d: no result stored", i)
		}

		if r.Message == "health check timed out" {
			t.Fatalf("iteration %d: a cancelled parent was reported as a timeout", i)
		}

		if r.Message != "health check cancelled: the controller is shutting down" {
			t.Fatalf("iteration %d: message = %q, want the cancellation message on every run", i, r.Message)
		}

		if r.Status != CheckUnhealthy {
			t.Fatalf("iteration %d: status = %v, want %v; a check with no result cannot say a dependency is well",
				i, r.Status, CheckUnhealthy)
		}
	}

	if got := invoked.Load(); got != 0 {
		t.Errorf("the consumer's check was invoked %d times with a context that was dead on arrival", got)
	}
}

// TestCheckThatGenuinelyOverrunsStillReportsATimeout — the message must keep
// working for the case it exists for.
func TestCheckThatGenuinelyOverrunsStillReportsATimeout(t *testing.T) {
	t.Parallel()

	e := &healthCheckEntry{
		check: HealthCheck{
			Name:    "slow",
			Timeout: 20 * time.Millisecond,
			Check: func(ctx context.Context) CheckResult {
				<-ctx.Done()

				return CheckResult{Status: CheckHealthy, Message: "too late"}
			},
		},
	}

	e.runCheck(context.Background())

	r := e.lastResult.Load()
	if r == nil {
		t.Fatal("no result stored")
	}

	if r.Status != CheckUnhealthy {
		t.Errorf("status = %v, want %v for a check that overran its timeout", r.Status, CheckUnhealthy)
	}

	if r.Message != "health check timed out" {
		t.Errorf("message = %q, want \"health check timed out\"", r.Message)
	}
}

// TestCheckCancelledMidFlightIsNotATimeout — the parent is cancelled while the
// check is still running, which is the shutdown case rather than a slow check.
func TestCheckCancelledMidFlightIsNotATimeout(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	release := make(chan struct{})

	e := &healthCheckEntry{
		check: HealthCheck{
			Name: "midflight",
			Check: func(context.Context) CheckResult {
				<-release

				return CheckResult{Status: CheckHealthy, Message: "returned anyway"}
			},
		},
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
		time.Sleep(20 * time.Millisecond)
		close(release)
	}()

	e.runCheck(ctx)

	r := e.lastResult.Load()
	if r == nil {
		t.Fatal("no result stored")
	}

	if r.Message == "health check timed out" {
		t.Error("a check interrupted by a cancelled parent reported a timeout it did not have")
	}
}

// TestShutdownDoesNotFakeTimeouts is the end-to-end shape from the issue.
func TestShutdownDoesNotFakeTimeouts(t *testing.T) {
	t.Parallel()

	c := NewController(context.Background())
	c.Register("svc", WithStart(func(ctx context.Context) error { <-ctx.Done(); return nil }))

	if err := c.RegisterHealthCheck(HealthCheck{
		Name: "fast", Type: CheckTypeReadiness,
		Check: func(context.Context) CheckResult {
			return CheckResult{Status: CheckHealthy, Timestamp: time.Now()}
		},
	}); err != nil {
		t.Fatal(err)
	}

	c.Start()
	time.Sleep(50 * time.Millisecond)
	c.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.WaitContext(ctx); err != nil {
		t.Fatalf("shutdown did not complete: %v", err)
	}

	for _, s := range c.Status().Services {
		if s.Name == "fast" && s.Error == "health check timed out" {
			t.Error("a check that returns instantly reported a timeout after shutdown")
		}
	}
}

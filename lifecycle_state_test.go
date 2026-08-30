package controls_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"gitlab.com/phpboyscout/go/errors"

	"gitlab.com/phpboyscout/go/controls"
)

// Spec 0003. The wrong-order pairs first, then the rules.

func blocker(started *atomic.Int64) controls.StartFunc {
	return func(ctx context.Context) error {
		if started != nil {
			started.Add(1)
		}

		<-ctx.Done()

		return ctx.Err()
	}
}

// waitFor bounds a controller's shutdown wait so a test that breaks the
// lifecycle reports it rather than hanging until the binary timeout. Wait() is
// unbounded by design, which is the right default in production and the wrong
// one in a suite.
func waitFor(t *testing.T, c *controls.Controller) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.WaitContext(ctx); err != nil {
		t.Errorf("shutdown did not complete: %v", err)
	}
}

// TestReadinessIsTrueOnlyWhileRunning is D2, and the assertion the whole spec
// exists for. Both halves: a test that only checks a stopped controller is
// unready passes against a controller that is always unready.
func TestReadinessIsTrueOnlyWhileRunning(t *testing.T) {
	t.Parallel()

	c := controls.NewController(t.Context())
	c.Register("svc", controls.WithStart(blocker(nil)))

	if c.Readiness().OverallHealthy {
		t.Error("readiness is true before Start")
	}

	c.Start()
	time.Sleep(50 * time.Millisecond)

	if !c.Readiness().OverallHealthy {
		t.Error("readiness is false while running with a healthy service")
	}

	c.Stop()
	waitFor(t, c)

	if c.Readiness().OverallHealthy {
		t.Error("readiness is true after Stop")
	}
}

// TestReadinessIsFalseDuringShutdown — after Stop returns is the easy half. The
// window traffic actually arrives in is while services are still stopping.
func TestReadinessIsFalseDuringShutdown(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	defer close(release)

	c := controls.NewController(t.Context())
	c.Register("slow",
		controls.WithStart(blocker(nil)),
		controls.WithStop(func(context.Context) { <-release }),
	)
	c.Start()
	time.Sleep(50 * time.Millisecond)

	go c.Stop()
	time.Sleep(100 * time.Millisecond)

	if st := c.GetState(); st != controls.Stopping {
		t.Fatalf("state = %s, want stopping; the fixture is not testing what it claims", st)
	}

	if c.Readiness().OverallHealthy {
		t.Error("readiness is true while shutting down; traffic is still being routed here")
	}
}

// TestLivenessIsUnaffectedByShutdown is the assertion that stops a later change
// made in the name of accuracy inviting a SIGKILL into a correct drain.
func TestLivenessIsUnaffectedByShutdown(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	defer close(release)

	c := controls.NewController(t.Context())
	c.Register("slow",
		controls.WithStart(blocker(nil)),
		controls.WithStop(func(context.Context) { <-release }),
	)
	c.Start()
	time.Sleep(50 * time.Millisecond)

	go c.Stop()
	time.Sleep(100 * time.Millisecond)

	if !c.Liveness().OverallHealthy {
		t.Error("liveness went false during shutdown; the orchestrator may now kill a correct drain")
	}
}

// TestNewControllerStartsNeverStarted is D3.
func TestNewControllerStartsNeverStarted(t *testing.T) {
	t.Parallel()

	c := controls.NewController(t.Context())

	if st := c.GetState(); st != controls.NeverStarted {
		t.Errorf("state = %s, want %s", st, controls.NeverStarted)
	}
}

// TestZeroValueControllerReportsUnknown — Unknown means the state could not be
// determined, which is exactly a Controller built without NewController.
func TestZeroValueControllerReportsUnknown(t *testing.T) {
	t.Parallel()

	var c controls.Controller

	if st := c.GetState(); st != controls.Unknown {
		t.Errorf("state = %q, want %s for a zero-valued Controller", st, controls.Unknown)
	}
}

// TestStopBeforeStartStaysNeverStarted is D6, and the deliberate divergence
// from Supervisor, which is single-use.
func TestStopBeforeStartStaysNeverStarted(t *testing.T) {
	t.Parallel()

	var runs atomic.Int64

	c := controls.NewController(t.Context())
	c.Register("svc", controls.WithStart(blocker(&runs)))

	c.Stop()

	if st := c.GetState(); st != controls.NeverStarted {
		t.Errorf("state = %s after Stop-before-Start, want %s", st, controls.NeverStarted)
	}

	c.Start()
	time.Sleep(50 * time.Millisecond)

	if got := runs.Load(); got != 1 {
		t.Errorf("service ran %d times; the controller was not startable after Stop-before-Start", got)
	}

	c.Stop()
	waitFor(t, c)
}

// TestUnableToStartNeedsBothConditions is D4. The two negative cases are the
// test: a positive-only test passes against an implementation that trips on any
// error.
func TestUnableToStartNeedsBothConditions(t *testing.T) {
	t.Parallel()

	t.Run("recovers before exhausting: not unable to start", func(t *testing.T) {
		t.Parallel()

		var runs atomic.Int64

		c := controls.NewController(t.Context())
		c.Register("flaky",
			controls.WithStart(func(ctx context.Context) error {
				if runs.Add(1) == 1 {
					return errors.New("dependency not up yet")
				}

				<-ctx.Done()

				return ctx.Err()
			}),
			controls.WithRestartPolicy(controls.RestartPolicy{
				MaxRestarts: 3, InitialBackoff: 5 * time.Millisecond, MaxBackoff: 10 * time.Millisecond,
			}),
		)
		c.Start()
		time.Sleep(300 * time.Millisecond)

		if st := c.GetState(); st != controls.Running {
			t.Errorf("state = %s after a service recovered on restart, want running", st)
		}

		c.Stop()
		waitFor(t, c)
	})

	t.Run("started cleanly then exhausted: not unable to start", func(t *testing.T) {
		t.Parallel()

		var runs atomic.Int64

		c := controls.NewController(t.Context())
		c.Register("later-failure",
			controls.WithStart(func(context.Context) error {
				if runs.Add(1) == 1 {
					return nil // a clean start: it served, then something else fails
				}

				return errors.New("died later")
			}),
			controls.WithStatus(func() error { return errors.New("unhealthy") }),
			controls.WithRestartPolicy(controls.RestartPolicy{
				MaxRestarts: 1, InitialBackoff: 5 * time.Millisecond, MaxBackoff: 10 * time.Millisecond,
				HealthFailureThreshold: 1, HealthCheckInterval: 10 * time.Millisecond,
			}),
		)
		c.Start()
		time.Sleep(400 * time.Millisecond)

		if st := c.GetState(); st == controls.UnableToStart {
			t.Error("a service that started cleanly and failed later was reported unable to start")
		}

		c.Stop()
		waitFor(t, c)
	})

	t.Run("never started and exhausted: unable to start", func(t *testing.T) {
		t.Parallel()

		c := controls.NewController(t.Context())
		c.Register("doomed",
			controls.WithStart(func(context.Context) error { return errors.New("nope") }),
			controls.WithRestartPolicy(controls.RestartPolicy{
				MaxRestarts: 2, InitialBackoff: 5 * time.Millisecond, MaxBackoff: 10 * time.Millisecond,
			}),
		)
		c.Start()
		time.Sleep(400 * time.Millisecond)

		if st := c.GetState(); st != controls.UnableToStart {
			t.Errorf("state = %s, want %s", st, controls.UnableToStart)
		}

		if c.Readiness().OverallHealthy {
			t.Error("readiness is true for a controller that cannot start")
		}

		c.Stop()
		waitFor(t, c)
	})
}

// TestNoRestartPolicyIsExhaustedImmediately — the runOnce path has no retries,
// so its first error is terminal. Easy to forget, because the restart loop is
// the code being reasoned about.
func TestNoRestartPolicyIsExhaustedImmediately(t *testing.T) {
	t.Parallel()

	c := controls.NewController(t.Context())
	c.Register("doomed", controls.WithStart(func(context.Context) error { return errors.New("nope") }))
	c.Start()
	time.Sleep(200 * time.Millisecond)

	if st := c.GetState(); st != controls.UnableToStart {
		t.Errorf("state = %s, want %s for a no-policy service that failed its only run", st, controls.UnableToStart)
	}

	c.Stop()
	waitFor(t, c)
}

// TestUnableToStartStopsNothing is D5. The other services keep serving, so a
// diagnosis endpoint is still reachable.
func TestUnableToStartStopsNothing(t *testing.T) {
	t.Parallel()

	var healthy atomic.Int64

	c := controls.NewController(t.Context())
	c.Register("doomed", controls.WithStart(func(context.Context) error { return errors.New("nope") }))
	c.Register("admin", controls.WithStart(blocker(&healthy)))
	c.Start()
	time.Sleep(200 * time.Millisecond)

	if st := c.GetState(); st != controls.UnableToStart {
		t.Fatalf("state = %s, want %s; the fixture is not testing what it claims", st, controls.UnableToStart)
	}

	if got := healthy.Load(); got != 1 {
		t.Errorf("the healthy sibling ran %d times, want 1; it should not have been stopped", got)
	}

	if !c.Liveness().OverallHealthy {
		t.Error("liveness went false; the process is alive, it just cannot serve")
	}

	c.Stop()
	waitFor(t, c)
}

// TestStopWorksFromUnableToStart is D8, and asserted on the services rather
// than on the state: reading the state back would pass against a Stop that set
// it and did nothing else.
func TestStopWorksFromUnableToStart(t *testing.T) {
	t.Parallel()

	stopped := make(chan struct{})

	c := controls.NewController(t.Context())
	c.Register("doomed", controls.WithStart(func(context.Context) error { return errors.New("nope") }))
	c.Register("admin",
		controls.WithStart(blocker(nil)),
		controls.WithStop(func(context.Context) { close(stopped) }),
	)
	c.Start()
	time.Sleep(200 * time.Millisecond)

	if st := c.GetState(); st != controls.UnableToStart {
		t.Fatalf("state = %s, want %s; the fixture is not testing what it claims", st, controls.UnableToStart)
	}

	c.Stop()

	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop from UnableToStart did not stop the running services")
	}

	waitFor(t, c)

	if st := c.GetState(); st != controls.Stopped {
		t.Errorf("state = %s after Stop, want %s", st, controls.Stopped)
	}
}

// TestRegistrationGuardsUseNeverStarted — the guards moved from Unknown, and a
// registration after Start must still be refused.
func TestRegistrationGuardsUseNeverStarted(t *testing.T) {
	t.Parallel()

	c := controls.NewController(t.Context())

	if err := c.RegisterHealthCheck(controls.HealthCheck{
		Name:  "before",
		Check: func(context.Context) controls.CheckResult { return controls.CheckResult{Status: controls.CheckHealthy} },
	}); err != nil {
		t.Fatalf("RegisterHealthCheck before Start: %v", err)
	}

	c.Start()

	if err := c.RegisterHealthCheck(controls.HealthCheck{
		Name:  "after",
		Check: func(context.Context) controls.CheckResult { return controls.CheckResult{Status: controls.CheckHealthy} },
	}); err == nil {
		t.Error("RegisterHealthCheck after Start was accepted")
	}

	c.Stop()
	waitFor(t, c)
}

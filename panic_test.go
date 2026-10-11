package controls_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	goerrors "gitlab.com/phpboyscout/go/errors"

	"gitlab.com/phpboyscout/go/controls"
)

// panicLog collects what a WithOnPanic callback was given.
type panicLog struct {
	mu      sync.Mutex
	reports []controls.PanicReport
}

func (l *panicLog) record(r controls.PanicReport) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.reports = append(l.reports, r)
}

func (l *panicLog) all() []controls.PanicReport {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]controls.PanicReport(nil), l.reports...)
}

// The panicking functions are named so a test can find them in a stack.

func panicsAtStart(context.Context) error { panic("start bug") }

func panicsInStatus() error { panic("status bug") }

func panicsInProbe() error { panic("probe bug") }

func panicsInStop(context.Context) { panic("stop bug") }

func panicsInStopErr(context.Context) error { panic("stoperr bug") }

func panicsInCheck(context.Context) controls.CheckResult { panic("check bug") }

func requireOrigin(t *testing.T, err error, fn string) {
	t.Helper()

	require.Contains(t, goerrors.OriginStack(err).String(), fn, "the origin stack must name the panic site")
}

// loggedController records the controller's log lines.
func loggedController(t *testing.T, opts ...controls.ControllerOpt) (*controls.Controller, *recordingHandler) {
	t.Helper()

	h := &recordingHandler{}

	return controls.NewController(context.Background(),
		append([]controls.ControllerOpt{controls.WithLogger(slog.New(h))}, opts...)...), h
}

func (h *recordingHandler) count(msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	n := 0

	for _, r := range h.records {
		if r.Message == msg {
			n++
		}
	}

	return n
}

// stackOn returns the stack attribute on the failure line for service, and
// whether the line had one.
func (h *recordingHandler) stackOn(service string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, r := range h.records {
		if r.Level != slog.LevelError {
			continue
		}

		var (
			named, kinded, found bool
			stack                string
		)

		r.Attrs(func(a slog.Attr) bool {
			named = named || (a.Key == "service_name" && a.Value.String() == service)
			kinded = kinded || a.Key == "kind"

			if a.Key == "stack" {
				found, stack = true, a.Value.String()
			}

			return true
		})

		if named && kinded {
			return stack, found
		}
	}

	return "", false
}

func readinessError(c *controls.Controller, name string) string {
	for _, s := range c.Readiness().Services {
		if s.Name == name {
			return s.Error
		}
	}

	return ""
}

// --- Wrong-order cases ---

// A start that panics once shutdown has cancelled its context is shutdown, not
// a failure (spec 0009 D5). Released on the context, not on Stop returning: a
// failure between the hand-off and the cancel is legitimately published.
func TestAStartPanicAfterTheCancelIsNotAFailure(t *testing.T) {
	t.Parallel()

	var (
		events eventLog
		panics panicLog
	)

	c, logs := loggedController(t, controls.WithOnEvent(events.record), controls.WithOnPanic(panics.record))
	c.Register("late", controls.WithStart(func(ctx context.Context) error {
		<-ctx.Done()
		panic("on the way out")
	}))
	c.Start()
	c.Stop()
	requireDone(t, c, 2*time.Second)

	require.Empty(t, events.all())
	require.Zero(t, logs.failuresLogged("late"))

	info, ok := c.GetServiceInfo("late")
	require.True(t, ok)
	require.ErrorIs(t, info.Error, controls.ErrPanicked)

	require.Len(t, panics.all(), 1, "the panic is before the cutoff, so it is reported")
	require.Empty(t, requireOutcome(t, c).Unfinished)
}

// A start that ignores the cancel and panics after Done is already abandoned,
// and publishes nothing (spec 0009 D5).
func TestAStartPanicAfterDoneIsAbandonedQuietly(t *testing.T) {
	t.Parallel()

	var events eventLog

	release := make(chan struct{})
	c, logs := loggedController(t, controls.WithOnEvent(events.record), controls.WithShutdownTimeout(50*time.Millisecond))
	c.Register("stuck", controls.WithStart(func(context.Context) error {
		<-release
		panic("long after")
	}))
	c.Start()
	c.Stop()
	requireDone(t, c, 2*time.Second)

	o := requireOutcome(t, c)
	require.Equal(t, []controls.Unfinished{{Service: "stuck", Reason: controls.SupervisorAbandoned}}, o.Unfinished)

	close(release)

	awaitTrue(t, 2*time.Second, "the late panic to be recorded", func() bool {
		info, _ := c.GetServiceInfo("stuck")

		return errors.Is(info.Error, controls.ErrPanicked)
	})

	require.Empty(t, events.all())
	require.Zero(t, logs.failuresLogged("stuck"), "classified as cancelled, so not logged either")
}

// A check panicking as shutdown cancels it is unhealthy either way.
func TestAHealthCheckPanickingAtShutdownIsUnhealthy(t *testing.T) {
	t.Parallel()

	entered, panicked := make(chan struct{}), make(chan struct{})
	c := newQuietController(t)
	require.NoError(t, c.RegisterHealthCheck(controls.HealthCheck{
		Name:     "dep",
		Interval: time.Hour,
		Check: func(ctx context.Context) controls.CheckResult {
			close(entered)
			<-ctx.Done()
			defer close(panicked)
			panic("as it closes")
		},
	}))
	c.Start()
	<-entered
	c.Stop()
	requireDone(t, c, 2*time.Second)
	<-panicked

	awaitTrue(t, 2*time.Second, "a result", func() bool {
		_, ok := c.GetCheckResult("dep")

		return ok
	})

	r, _ := c.GetCheckResult("dep")
	require.Equal(t, controls.CheckUnhealthy, r.Status)
}

// A panic is a defect, never an expected finish (spec 0009 D5).
func TestAPanicIsNeverAValidError(t *testing.T) {
	t.Parallel()

	var events eventLog

	c := newQuietController(t,
		controls.WithOnEvent(events.record),
		controls.WithValidError(func(error) bool { return true }))
	c.Register("bad", controls.WithStart(panicsAtStart))
	c.Start()

	awaitTrue(t, 2*time.Second, "an event", func() bool { return len(events.all()) == 1 })
	require.Equal(t, controls.UnableToStart, c.GetState())

	c.Stop()
	requireDone(t, c, 2*time.Second)

	require.Equal(t, []controls.EventKind{controls.EventUnableToStart}, events.kinds())
}

// The panic value is formatted, not wrapped, so a panicking context error
// cannot pass as a cancellation (spec 0009 D2).
func TestPanickingWithContextCanceledIsAFailure(t *testing.T) {
	t.Parallel()

	var events eventLog

	c := newQuietController(t, controls.WithOnEvent(events.record))
	c.Register("bad", controls.WithStart(func(context.Context) error { panic(context.Canceled) }))
	c.Start()

	awaitTrue(t, 2*time.Second, "an event", func() bool { return len(events.all()) == 1 })
	c.Stop()
	requireDone(t, c, 2*time.Second)

	ev := events.all()[0]
	require.Equal(t, controls.EventUnableToStart, ev.Kind)
	require.ErrorIs(t, ev.Err, controls.ErrPanicked)
	require.NotErrorIs(t, ev.Err, context.Canceled)
}

// Before Start, during the drain and after Done, a probe panic is recovered
// and reported unhealthy, but never reaches the callback (spec 0009 D11).
func TestProbePanicsOutsideTheWindowAreNotReported(t *testing.T) {
	t.Parallel()

	var panics panicLog

	hold := make(chan struct{})
	c := newQuietController(t,
		controls.WithOnPanic(panics.record),
		controls.WithOnEvent(func(controls.ServiceEvent) { <-hold }))
	c.Register("fails", controls.WithStart(failsWith(errors.New("boom"))))
	c.Register("probed", controls.WithReadiness(panicsInProbe))

	require.Contains(t, readinessError(c, "probed"), "probe panicked: probe bug", "before Start")

	c.Start()
	awaitTrue(t, 2*time.Second, "UnableToStart", func() bool { return c.GetState() == controls.UnableToStart })
	c.Stop()
	awaitTrue(t, 2*time.Second, "the drain", func() bool { return c.GetState() == controls.Stopped })

	require.Contains(t, readinessError(c, "probed"), "probe panicked", "during the drain")

	close(hold)
	requireDone(t, c, 2*time.Second)

	require.Contains(t, readinessError(c, "probed"), "probe panicked", "after Done")

	time.Sleep(20 * time.Millisecond)
	require.Empty(t, panics.all())
}

// The sink is wired at construction, so a report racing Start reads nothing
// Start writes. Meaningful only under -race.
func TestReadinessRacingStartReadsNoSinkWrite(t *testing.T) {
	t.Parallel()

	for range 50 {
		c := newQuietController(t, controls.WithOnPanic(func(controls.PanicReport) {}))
		c.Register("probed", controls.WithReadiness(panicsInProbe))
		require.NoError(t, c.RegisterHealthCheck(controls.HealthCheck{Name: "dep", Check: panicsInCheck}))

		var wg sync.WaitGroup

		wg.Go(func() {
			for range 20 {
				c.Readiness()
			}
		})

		c.Start()
		wg.Wait()
		c.Stop()
		requireDone(t, c, 2*time.Second)
	}
}

// A panicking callback is logged and never reported, and the next report still
// arrives (spec 0009 D11).
func TestAPanickingPanicCallbackIsLoggedNotReported(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	c, logs := loggedController(t, controls.WithOnPanic(func(controls.PanicReport) {
		if calls.Add(1) == 1 {
			panic("callback bug")
		}
	}))
	c.Register("probed", controls.WithReadiness(panicsInProbe))
	c.Start()

	c.Readiness()
	awaitTrue(t, 2*time.Second, "the callback's panic to be logged", func() bool {
		return logs.count("WithOnPanic callback panicked") == 1
	})

	c.Readiness()
	awaitTrue(t, 2*time.Second, "the second report", func() bool { return calls.Load() == 2 })

	c.Stop()
	requireDone(t, c, 2*time.Second)

	require.Equal(t, int64(2), calls.Load(), "the callback's own panic is not reported")
	require.Equal(t, 1, logs.count("WithOnPanic callback panicked"))
}

func TestAPanicCallbackCanStopTheController(t *testing.T) {
	t.Parallel()

	returned := make(chan struct{})

	var c *controls.Controller

	c = newQuietController(t, controls.WithOnPanic(func(controls.PanicReport) {
		c.Stop()
		close(returned)
	}))
	c.Register("bad", controls.WithStart(panicsAtStart))
	c.Start()

	requireDone(t, c, 2*time.Second)

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop from the panic callback did not return")
	}

	require.Equal(t, controls.CauseStop, requireOutcome(t, c).Cause)
}

// The Supervisor never joins its panic dispatch, so a callback calling Stop is
// not waiting for itself (spec 0009 D11).
func TestAChildPanicCallbackCanStopTheSupervisor(t *testing.T) {
	t.Parallel()

	returned := make(chan struct{})

	var s *controls.Supervisor

	s = controls.NewSupervisor(controls.WithOnChildPanic(func(controls.PanicReport) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		s.Stop(ctx)
		close(returned)
	}))
	require.NoError(t, s.Attach(controls.Child{Name: "crasher", Start: panicsAtStart}))
	require.NoError(t, s.Start(t.Context()))

	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop from the child panic callback did not return")
	}
}

// --- Forward cases ---

func TestAStartPanicWithNoPolicyIsUnableToStart(t *testing.T) {
	t.Parallel()

	var events eventLog

	stopped := make(chan struct{})
	c := newQuietController(t, controls.WithOnEvent(events.record))
	errs := c.Errors()

	c.Register("good",
		controls.WithStart(func(ctx context.Context) error { <-ctx.Done(); return nil }),
		controls.WithStop(func(context.Context) { close(stopped) }))
	c.Register("bad", controls.WithStart(panicsAtStart))
	c.Start()

	var fromChannel error

	select {
	case fromChannel = <-errs:
	case <-time.After(2 * time.Second):
		t.Fatal("no error on the channel")
	}

	require.Equal(t, controls.UnableToStart, c.GetState())

	c.Stop()
	requireDone(t, c, 2*time.Second)
	<-stopped

	require.ErrorIs(t, fromChannel, controls.ErrPanicked)

	ev := events.all()
	require.Len(t, ev, 1)
	require.Equal(t, controls.EventUnableToStart, ev[0].Kind)
	require.ErrorIs(t, ev[0].Err, controls.ErrPanicked)

	info, _ := c.GetServiceInfo("bad")
	require.ErrorIs(t, info.Error, controls.ErrPanicked)
	require.Equal(t, `controls: service "bad" panicked: start bug`, info.Error.Error())
	requireOrigin(t, info.Error, "panicsAtStart")
	require.Empty(t, requireOutcome(t, c).Unfinished, "the other service stops cleanly")
}

func TestAStartPanicUnderAPolicyRetriesThenExhausts(t *testing.T) {
	t.Parallel()

	var events eventLog

	c := newQuietController(t, controls.WithOnEvent(events.record))
	c.Register("bad", controls.WithStart(panicsAtStart),
		controls.WithRestartPolicy(controls.RestartPolicy{MaxRestarts: 2, InitialBackoff: time.Millisecond}))
	c.Start()

	awaitTrue(t, 2*time.Second, "three events", func() bool { return len(events.all()) == 3 })
	c.Stop()
	requireDone(t, c, 2*time.Second)

	require.Equal(t, []controls.EventKind{
		controls.EventRetrying, controls.EventRetrying, controls.EventUnableToStart,
	}, events.kinds())

	last := events.all()[2].Err
	require.ErrorIs(t, last, controls.ErrRestartsExhausted)
	require.ErrorIs(t, last, controls.ErrPanicked)
}

func TestAServiceThatPanicsOnceThenStartsIsRunning(t *testing.T) {
	t.Parallel()

	var (
		events eventLog
		runs   atomic.Int64
	)

	c := newQuietController(t, controls.WithOnEvent(events.record))
	c.Register("flaky", controls.WithStart(func(context.Context) error {
		if runs.Add(1) == 1 {
			panic("first time only")
		}

		return nil
	}), controls.WithRestartPolicy(controls.RestartPolicy{MaxRestarts: 2, InitialBackoff: time.Millisecond}))
	c.Start()

	awaitTrue(t, 2*time.Second, "the second run", func() bool { return runs.Load() == 2 })
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, controls.Running, c.GetState())

	c.Stop()
	requireDone(t, c, 2*time.Second)

	require.Equal(t, []controls.EventKind{controls.EventRetrying}, events.kinds())
}

// statusAt breaches a service whose Status fails failsFirst times and then
// panics, and reports how many Status calls the first breach took.
func statusAt(t *testing.T, failsFirst, threshold int, opts ...controls.ControllerOpt) (int64, controls.ServiceEvent) {
	t.Helper()

	var (
		events eventLog
		calls  atomic.Int64
		atStop atomic.Int64
	)

	c := newQuietController(t, append(opts, controls.WithOnEvent(events.record))...)
	c.Register("svc",
		controls.WithStatus(func() error {
			if calls.Add(1) <= int64(failsFirst) {
				return errors.New("unwell")
			}

			return panicsInStatus()
		}),
		controls.WithStop(func(context.Context) { atStop.CompareAndSwap(0, calls.Load()) }),
		controls.WithRestartPolicy(controls.RestartPolicy{
			MaxRestarts: 5, InitialBackoff: time.Hour,
			HealthFailureThreshold: threshold, HealthCheckInterval: time.Millisecond,
		}))
	c.Start()

	awaitTrue(t, 2*time.Second, "a breach", func() bool { return len(events.all()) == 1 })
	c.Stop()
	requireDone(t, c, 2*time.Second)

	return atStop.Load(), events.all()[0]
}

// A panicking polled Status breaches at once, whatever the threshold and the
// failures before it (spec 0009 D9).
func TestAPolledStatusPanicBreachesAtOnce(t *testing.T) {
	t.Parallel()

	for _, failsFirst := range []int{0, 2} {
		calls, ev := statusAt(t, failsFirst, 5)

		require.Equal(t, int64(failsFirst+1), calls, "breached on the first panic")
		require.Equal(t, controls.EventRetrying, ev.Kind)
		require.ErrorIs(t, ev.Err, controls.ErrPanicked)
		require.Equal(t, "health check failed: probe panicked: status bug", ev.Err.Error())
		requireOrigin(t, ev.Err, "panicsInStatus")
		require.Contains(t, goerrors.StackOf(ev.Err).String(), "monitorHealth",
			"the outermost stack is the wrap's, which is why the promise is OriginStack")
	}
}

// With the opt-in a panic counts as one failure (spec 0009 D10).
func TestProbePanicsAsFailuresCountTowardsTheThreshold(t *testing.T) {
	t.Parallel()

	calls, ev := statusAt(t, 0, 3, controls.WithProbePanicsAsFailures())

	require.Equal(t, int64(3), calls)
	require.ErrorIs(t, ev.Err, controls.ErrPanicked)
}

// A health-driven exhaustion carries the panic that drove it (spec 0009 D12).
func TestAStatusPanicExhaustionCarriesThePanic(t *testing.T) {
	t.Parallel()

	var events eventLog

	c := newQuietController(t, controls.WithOnEvent(events.record))
	errs := c.Errors()

	c.Register("svc",
		controls.WithStatus(panicsInStatus),
		controls.WithRestartPolicy(controls.RestartPolicy{
			MaxRestarts: 1, InitialBackoff: time.Millisecond,
			HealthFailureThreshold: 1, HealthCheckInterval: time.Millisecond,
		}))
	c.Start()

	got := exhaustionOf(t, errs)

	c.Stop()
	requireDone(t, c, 2*time.Second)

	require.ErrorIs(t, got, controls.ErrPanicked)

	ev := events.all()
	require.Equal(t, []controls.EventKind{controls.EventRetrying, controls.EventFailed}, events.kinds())
	require.ErrorIs(t, ev[1].Err, controls.ErrRestartsExhausted)
	require.ErrorIs(t, ev[1].Err, controls.ErrPanicked)
	requireOrigin(t, ev[1].Err, "panicsInStatus")

	info, _ := c.GetServiceInfo("svc")
	require.ErrorIs(t, info.Error, controls.ErrRestartsExhausted)
	require.ErrorIs(t, info.Error, controls.ErrPanicked)
}

func TestAPanickingHealthCheckIsUnhealthy(t *testing.T) {
	t.Parallel()

	for _, interval := range []time.Duration{0, time.Millisecond} {
		c := newQuietController(t)
		require.NoError(t, c.RegisterHealthCheck(controls.HealthCheck{
			Name: "dep", Type: controls.CheckTypeReadiness, Interval: interval, Check: panicsInCheck,
		}))
		c.Start()

		awaitTrue(t, 2*time.Second, "the check to run", func() bool {
			return readinessError(c, "dep") != "check has not completed its first run"
		})

		require.Equal(t, "health check panicked: check bug", readinessError(c, "dep"))

		c.Stop()
		requireDone(t, c, 2*time.Second)
	}
}

// The recovered messages are what they were before the sentinel (spec 0009 D2).
func TestRecoveredMessagesAreUnchanged(t *testing.T) {
	t.Parallel()

	c := newQuietController(t)
	c.Register("plain", controls.WithStatus(panicsInProbe), controls.WithStop(panicsInStop))
	c.Register("witherr", controls.WithStopErr(panicsInStopErr))
	c.Start()

	require.Equal(t, "probe panicked: probe bug", readinessError(c, "plain"))

	c.Stop()
	requireDone(t, c, 2*time.Second)

	plain, _ := c.GetServiceInfo("plain")
	require.ErrorIs(t, plain.StopErr, controls.ErrPanicked)
	require.Equal(t, "controls: stop panicked: stop bug", plain.StopErr.Error())

	withErr, _ := c.GetServiceInfo("witherr")
	require.ErrorIs(t, withErr.StopErr, controls.ErrPanicked)
	require.Equal(t, `controls: stopping "witherr" panicked: stoperr bug`, withErr.StopErr.Error())
}

// The stack goes on the failure's line for a panic, and only for one (spec
// 0009 D8).
func TestAPanicsFailureLineCarriesItsStack(t *testing.T) {
	t.Parallel()

	c, logs := loggedController(t)
	c.Register("bad", controls.WithStart(panicsAtStart))
	c.Register("erring", controls.WithStart(failsWith(errors.New("boom"))))
	c.Start()

	awaitTrue(t, 2*time.Second, "both failures", func() bool {
		return logs.failuresLogged("bad") == 1 && logs.failuresLogged("erring") == 1
	})
	c.Stop()
	requireDone(t, c, 2*time.Second)

	stack, ok := logs.stackOn("bad")
	require.True(t, ok)
	require.Contains(t, stack, "panicsAtStart")

	_, ok = logs.stackOn("erring")
	require.False(t, ok)
}

// --- The panic callback ---

type reportCase struct {
	name    string
	setup   func(t *testing.T, c *controls.Controller)
	trigger func(c *controls.Controller)
	service string
	site    controls.PanicSite
	value   any
}

func breachThenPanicInStop(c *controls.Controller) {
	c.Register("breach",
		controls.WithStatus(func() error { return errors.New("unwell") }),
		controls.WithStop(panicsInStop),
		controls.WithRestartPolicy(controls.RestartPolicy{
			MaxRestarts: 1, InitialBackoff: time.Hour,
			HealthFailureThreshold: 1, HealthCheckInterval: time.Millisecond,
		}))
}

// Every recover site reports to the callback with its service and site, the
// raw value, and an error wrapping ErrPanicked (spec 0009 D11).
func TestEverySiteReportsToThePanicCallback(t *testing.T) {
	t.Parallel()

	policy := controls.WithRestartPolicy(controls.RestartPolicy{MaxRestarts: 1, InitialBackoff: time.Hour})
	none := func(*controls.Controller) {}

	cases := []reportCase{
		{"start, no policy", func(_ *testing.T, c *controls.Controller) {
			c.Register("svc", controls.WithStart(panicsAtStart))
		}, none, "svc", controls.SiteStart, "start bug"},
		{"start, policy", func(_ *testing.T, c *controls.Controller) {
			c.Register("svc", controls.WithStart(panicsAtStart), policy)
		}, none, "svc", controls.SiteStart, "start bug"},
		{"polled status", func(_ *testing.T, c *controls.Controller) {
			c.Register("svc", controls.WithStatus(panicsInStatus), controls.WithRestartPolicy(controls.RestartPolicy{
				MaxRestarts: 1, InitialBackoff: time.Hour, HealthFailureThreshold: 1, HealthCheckInterval: time.Millisecond,
			}))
		}, none, "svc", controls.SiteProbe, "status bug"},
		{"Status()", func(_ *testing.T, c *controls.Controller) {
			c.Register("svc", controls.WithStatus(panicsInProbe))
		}, func(c *controls.Controller) { c.Status() }, "svc", controls.SiteProbe, "probe bug"},
		{"Liveness()", func(_ *testing.T, c *controls.Controller) {
			c.Register("svc", controls.WithLiveness(panicsInProbe))
		}, func(c *controls.Controller) { c.Liveness() }, "svc", controls.SiteProbe, "probe bug"},
		{"Readiness()", func(_ *testing.T, c *controls.Controller) {
			c.Register("svc", controls.WithReadiness(panicsInProbe))
		}, func(c *controls.Controller) { c.Readiness() }, "svc", controls.SiteProbe, "probe bug"},
		{"stop at a breach", func(_ *testing.T, c *controls.Controller) {
			breachThenPanicInStop(c)
		}, none, "breach", controls.SiteStop, "stop bug"},
		{"StopErr at shutdown", func(_ *testing.T, c *controls.Controller) {
			c.Register("svc", controls.WithStopErr(panicsInStopErr))
		}, func(c *controls.Controller) { c.Stop() }, "svc", controls.SiteStop, "stoperr bug"},
		{"stop at shutdown", func(_ *testing.T, c *controls.Controller) {
			c.Register("svc", controls.WithStop(panicsInStop))
		}, func(c *controls.Controller) { c.Stop() }, "svc", controls.SiteStop, "stop bug"},
		{"sync health check", func(t *testing.T, c *controls.Controller) {
			require.NoError(t, c.RegisterHealthCheck(controls.HealthCheck{Name: "dep", Check: panicsInCheck}))
		}, func(c *controls.Controller) { c.Readiness() }, "dep", controls.SiteHealthCheck, "check bug"},
		{"async health check", func(t *testing.T, c *controls.Controller) {
			require.NoError(t, c.RegisterHealthCheck(controls.HealthCheck{
				Name: "dep", Interval: time.Hour, Check: panicsInCheck,
			}))
		}, none, "dep", controls.SiteHealthCheck, "check bug"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var panics panicLog

			c := newQuietController(t, controls.WithOnPanic(panics.record))
			tc.setup(t, c)
			c.Start()
			tc.trigger(c)

			awaitTrue(t, 2*time.Second, "a report", func() bool { return len(panics.all()) > 0 })
			c.Stop()
			requireDone(t, c, 2*time.Second)

			r := panics.all()[0]
			require.Equal(t, tc.service, r.Service)
			require.Equal(t, tc.site, r.Site)
			require.Equal(t, tc.value, r.Value)
			require.ErrorIs(t, r.Err, controls.ErrPanicked)
		})
	}
}

// The callback is beside the failure path, not instead of it.
func TestAReportedStartPanicIsStillAFailure(t *testing.T) {
	t.Parallel()

	var (
		events eventLog
		panics panicLog
	)

	c := newQuietController(t, controls.WithOnEvent(events.record), controls.WithOnPanic(panics.record))
	c.Register("svc", controls.WithStart(panicsAtStart))
	c.Start()

	awaitTrue(t, 2*time.Second, "an event and a report", func() bool {
		return len(events.all()) == 1 && len(panics.all()) == 1
	})
	c.Stop()
	requireDone(t, c, 2*time.Second)

	require.Same(t, events.all()[0].Err, panics.all()[0].Err)
}

func TestChildPanicsReportToTheSupervisorsCallback(t *testing.T) {
	t.Parallel()

	var panics panicLog

	s := controls.NewSupervisor(controls.WithOnChildPanic(panics.record))
	require.NoError(t, s.Attach(controls.Child{Name: "crasher", Start: panicsAtStart}))
	require.NoError(t, s.Attach(controls.Child{
		Name: "stopper", Start: blocksUntilCancelled(nil), Stop: panicsInStop,
	}))
	require.NoError(t, s.Start(t.Context()))

	awaitTrue(t, 2*time.Second, "the start report", func() bool { return len(panics.all()) == 1 })
	require.NoError(t, s.Detach(t.Context(), "stopper"))
	awaitTrue(t, 2*time.Second, "the stop report", func() bool { return len(panics.all()) == 2 })

	s.Stop(t.Context())

	got := panics.all()
	require.Equal(t, "crasher", got[0].Service)
	require.Equal(t, controls.SiteStart, got[0].Site)
	require.Equal(t, "stopper", got[1].Service)
	require.Equal(t, controls.SiteStop, got[1].Site)

	for _, r := range got {
		require.ErrorIs(t, r.Err, controls.ErrPanicked)
	}
}

// A child's panic is never valid either (spec 0009 D7).
func TestAChildPanicIsNeverValid(t *testing.T) {
	t.Parallel()

	failed := make(chan controls.Failure, 1)
	s := controls.NewSupervisor(controls.WithOnFailure(func(f controls.Failure) { failed <- f }))
	require.NoError(t, s.Attach(controls.Child{
		Name: "crasher", Start: panicsAtStart, ValidError: func(error) bool { return true },
	}))
	require.NoError(t, s.Start(t.Context()))

	select {
	case f := <-failed:
		require.True(t, f.Panicked)
		require.ErrorIs(t, f.Err, controls.ErrPanicked)
	case <-time.After(2 * time.Second):
		t.Fatal("a panicking child with a match-all ValidError did not fail")
	}

	s.Stop(t.Context())
}

// A slow callback holds up neither a report nor a breach.
func TestASlowPanicCallbackDelaysNothing(t *testing.T) {
	t.Parallel()

	hold := make(chan struct{})
	entered := make(chan struct{}, 1)
	stopped := make(chan struct{})

	c := newQuietController(t, controls.WithOnPanic(func(controls.PanicReport) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-hold
	}))
	c.Register("probed", controls.WithReadiness(panicsInProbe))
	c.Register("breach",
		controls.WithStatus(panicsInStatus),
		controls.WithStop(func(context.Context) {
			select {
			case <-stopped:
			default:
				close(stopped)
			}
		}),
		controls.WithRestartPolicy(controls.RestartPolicy{
			MaxRestarts: 1, InitialBackoff: time.Hour,
			HealthFailureThreshold: 1, HealthCheckInterval: time.Millisecond,
		}))
	c.Start()

	<-entered

	done := make(chan struct{})

	go func() {
		c.Readiness()
		close(done)
	}()

	for _, ch := range []chan struct{}{done, stopped} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("a blocked panic callback held something up")
		}
	}

	close(hold)
	c.Stop()
	requireDone(t, c, 2*time.Second)
}

// Behind a blocked callback, repeats at one site coalesce into the latest
// with a count; a second site and a health check sharing the service's name
// each keep their own report (spec 0009 D11).
func TestPanicReportsCoalescePerReporterAndSite(t *testing.T) {
	t.Parallel()

	var (
		panics panicLog
		n      atomic.Int64
	)

	hold := make(chan struct{})
	entered := make(chan struct{})
	startPanics := make(chan struct{})

	var once sync.Once

	c := newQuietController(t, controls.WithOnPanic(func(r controls.PanicReport) {
		once.Do(func() { close(entered) })
		<-hold
		panics.record(r)
	}))
	c.Register("x",
		controls.WithStart(func(context.Context) error { <-startPanics; panic("start bug") }),
		controls.WithReadiness(func() error { panic(fmt.Sprintf("bug %d", n.Add(1))) }))
	require.NoError(t, c.RegisterHealthCheck(controls.HealthCheck{
		Name: "x", Type: controls.CheckTypeLiveness, Check: panicsInCheck,
	}))
	c.Start()

	c.Readiness()
	<-entered

	for range 9 {
		c.Readiness()
	}

	close(startPanics) // a second site
	awaitTrue(t, 2*time.Second, "the start panic", func() bool { return c.GetState() == controls.UnableToStart })

	c.Liveness() // the check that shares the service's name

	close(hold)
	awaitTrue(t, 2*time.Second, "four reports", func() bool { return len(panics.all()) == 4 })
	time.Sleep(20 * time.Millisecond)

	got := panics.all()
	require.Len(t, got, 4)

	require.Equal(t, "bug 1", got[0].Value)
	require.Zero(t, got[0].Repeats)

	require.Equal(t, "bug 10", got[1].Value, "the newest panic stands for the merged ones")
	require.Equal(t, 8, got[1].Repeats)
	require.Contains(t, got[1].Err.Error(), "bug 10")

	require.Equal(t, controls.SiteStart, got[2].Site)
	require.Equal(t, controls.SiteHealthCheck, got[3].Site)
	require.Equal(t, "x", got[3].Service)

	c.Stop()
	requireDone(t, c, 2*time.Second)
}

// The panic forwarder is cut off and drained with the others: a report still
// queued at the deadline is abandoned and counted, never delivered after Done.
func TestUndeliveredPanicReportsAreAbandonedAtTheDeadline(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	hold := make(chan struct{})
	entered := make(chan struct{})

	var once sync.Once

	c, logs := loggedController(t,
		controls.WithShutdownTimeout(100*time.Millisecond),
		controls.WithOnPanic(func(controls.PanicReport) {
			calls.Add(1)
			once.Do(func() { close(entered) })
			<-hold
		}))
	c.Register("x", controls.WithReadiness(panicsInProbe))
	require.NoError(t, c.RegisterHealthCheck(controls.HealthCheck{
		Name: "dep", Type: controls.CheckTypeLiveness, Check: panicsInCheck,
	}))
	c.Start()

	c.Readiness()
	<-entered
	c.Liveness() // queued behind it, at a second site

	c.Stop()
	requireDone(t, c, 2*time.Second)

	close(hold)
	time.Sleep(50 * time.Millisecond)

	require.Equal(t, int64(1), calls.Load(), "nothing is delivered after Done")
	require.Equal(t, int64(1), logs.undelivered("WithOnPanic"))
}

// Without the recover a panicking start takes the process down, which no test
// in this binary could survive, so it runs in a child.
func TestAStartPanicDoesNotCrashTheProcess(t *testing.T) {
	if os.Getenv("CONTROLS_PANIC_CHILD") == "1" {
		c := controls.NewController(context.Background())
		c.Register("bad", controls.WithStart(panicsAtStart))
		c.Start()

		deadline := time.Now().Add(5 * time.Second)
		for c.GetState() != controls.UnableToStart && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}

		c.Stop()
		<-c.Done()
		fmt.Println("survived")

		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	//nolint:gosec // G204: the test binary re-executing itself with a fixed test name.
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAStartPanicDoesNotCrashTheProcess$", "-test.v")
	cmd.Env = append(os.Environ(), "CONTROLS_PANIC_CHILD=1")

	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "the child crashed:\n%s", out)
	require.Contains(t, string(out), "survived", "the child did not finish")
}

// Names may repeat, so reports merge per registration, never per name.
func TestSameNamedServicesKeepSeparateReports(t *testing.T) {
	t.Parallel()

	var panics panicLog

	hold := make(chan struct{})
	entered := make(chan struct{})

	var once sync.Once

	c := newQuietController(t, controls.WithOnPanic(func(r controls.PanicReport) {
		once.Do(func() { close(entered) })
		<-hold
		panics.record(r)
	}))
	c.Register("twin", controls.WithReadiness(panicsInProbe))
	c.Register("twin", controls.WithReadiness(panicsInProbe))
	c.Start()

	c.Readiness() // the first twin's report is claimed, the second's queued
	<-entered
	c.Readiness() // the first queues again; the second merges into its own

	close(hold)
	awaitTrue(t, 2*time.Second, "three reports", func() bool { return len(panics.all()) == 3 })
	time.Sleep(20 * time.Millisecond)

	got := panics.all()
	require.Len(t, got, 3)
	require.Equal(t, []int{0, 1, 0}, []int{got[0].Repeats, got[1].Repeats, got[2].Repeats})

	c.Stop()
	requireDone(t, c, 2*time.Second)
}

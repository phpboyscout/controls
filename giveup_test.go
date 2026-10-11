package controls_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.com/phpboyscout/go/controls"
)

// gatedHandler records like recordingHandler, holds the line named hold until
// release closes, and notes the controller's state on every failure line.
type gatedHandler struct {
	recordingHandler

	hold    string
	release chan struct{}

	ctrl   atomic.Pointer[controls.Controller]
	stMu   sync.Mutex
	states []controls.State
}

func (h *gatedHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == h.hold {
		<-h.release
	}

	if c := h.ctrl.Load(); c != nil && r.Level == slog.LevelError {
		kinded := false

		r.Attrs(func(a slog.Attr) bool {
			kinded = kinded || a.Key == "kind"

			return true
		})

		if kinded {
			h.stMu.Lock()
			h.states = append(h.states, c.GetState())
			h.stMu.Unlock()
		}
	}

	return h.recordingHandler.Handle(ctx, r)
}

func (h *gatedHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *gatedHandler) WithGroup(string) slog.Handler      { return h }

func (h *gatedHandler) failureStates() []controls.State {
	h.stMu.Lock()
	defer h.stMu.Unlock()

	return append([]controls.State(nil), h.states...)
}

func gatedController(t *testing.T, hold string, opts ...controls.ControllerOpt) (*controls.Controller, *gatedHandler) {
	t.Helper()

	h := &gatedHandler{hold: hold, release: make(chan struct{})}
	if hold == "" {
		close(h.release)
	}

	c := controls.NewController(context.Background(),
		append([]controls.ControllerOpt{controls.WithLogger(slog.New(h))}, opts...)...)
	h.ctrl.Store(c)

	return c, h
}

func completesNow(context.Context) error { return nil }

func blocksUntilDone(ctx context.Context) error {
	<-ctx.Done()

	return nil
}

func gated(gate <-chan struct{}, err error) controls.StartFunc {
	return func(context.Context) error {
		<-gate

		return err
	}
}

// requireStopsWith waits for Done and checks the cause and the service.
func requireStopsWith(t *testing.T, c *controls.Controller, cause controls.StopCause, service string) controls.Outcome {
	t.Helper()

	requireDone(t, c, 3*time.Second)

	o := requireOutcome(t, c)
	require.Equal(t, cause, o.Cause)
	require.Equal(t, service, o.Service)

	return o
}

// requireStillUp checks the controller has not begun to stop after a while.
func requireStillUp(t *testing.T, c *controls.Controller) {
	t.Helper()

	select {
	case <-c.Done():
		t.Fatal("the controller stopped")
	case <-time.After(100 * time.Millisecond):
	}

	require.NotEqual(t, controls.Stopping, c.GetState())
	require.NotEqual(t, controls.Stopped, c.GetState())
}

func watched(status controls.StatusFunc, stops *atomic.Int64) []controls.ServiceOption {
	return []controls.ServiceOption{
		controls.WithStart(completesNow),
		controls.WithStatus(status),
		controls.WithStop(func(context.Context) {
			if stops != nil {
				stops.Add(1)
			}
		}),
	}
}

var errDied = errors.New("serve loop died")

// --- Wrong-order pairs ---

// A failure classified after Stop but before the cancel is published, and
// Stop's cause stands. The hold on "Stopping Services", which precedes the
// cancel, is what places it there.
func TestAGiveUpAfterStopBeforeTheCancelKeepsStopsCause(t *testing.T) {
	t.Parallel()

	var events eventLog

	gate := make(chan struct{})
	c, logs := gatedController(t, "Stopping Services", controls.WithStopOnGiveUp(), controls.WithOnEvent(events.record))
	c.Register("svc", controls.WithStart(gated(gate, errors.New("boom"))))
	c.Start()
	c.Stop()

	close(gate)
	awaitTrue(t, 2*time.Second, "the failure line", func() bool { return logs.failuresLogged("svc") == 1 })
	close(logs.release)

	requireStopsWith(t, c, controls.CauseStop, "")
	require.Equal(t, []controls.EventKind{controls.EventUnableToStart}, events.kinds())
	require.Zero(t, logs.count("Stop called, but not in expected state, unable to continue"),
		"a give-up losing to an earlier trigger is silent")
}

func TestAFailureAfterTheCancelIsNotAGiveUp(t *testing.T) {
	t.Parallel()

	var events eventLog

	c := newQuietController(t, controls.WithStopOnGiveUp(), controls.WithOnEvent(events.record))
	c.Register("svc", controls.WithStart(func(ctx context.Context) error {
		<-ctx.Done()

		return errors.New("on the way out")
	}))
	c.Start()
	c.Stop()

	requireStopsWith(t, c, controls.CauseStop, "")
	require.Empty(t, events.all())
}

// A death the watch notices only once the cancel has come is shutdown.
func TestADeathNoticedAfterTheCancelIsNotAGiveUp(t *testing.T) {
	t.Parallel()

	var (
		events  eventLog
		started atomic.Pointer[context.Context]
	)

	polled := make(chan struct{})

	var once sync.Once

	c := newQuietController(t, controls.WithStopOnGiveUp(), controls.WithDeathWatch(time.Millisecond, 1),
		controls.WithOnEvent(events.record))
	c.Register("svc",
		controls.WithStart(func(ctx context.Context) error { started.Store(&ctx); return nil }),
		controls.WithStatus(func() error {
			once.Do(func() { close(polled) })
			<-(*started.Load()).Done()

			return errDied
		}))
	c.Start()
	<-polled
	c.Stop()

	requireStopsWith(t, c, controls.CauseStop, "")
	require.Empty(t, events.all())
}

func TestASignalAndACompletionRaceToOneCause(t *testing.T) {
	t.Parallel()

	for range 100 {
		sigs := make(chan os.Signal, 1)
		c := newQuietController(t)
		c.SetSignalsChannel(sigs)
		c.Register("job", controls.WithStart(completesNow), controls.WithRunToCompletion())
		c.Start()
		sigs <- os.Interrupt

		requireDone(t, c, 2*time.Second)

		o := requireOutcome(t, c)
		switch o.Cause {
		case controls.CauseCompleted:
			require.Equal(t, "job", o.Service)
			require.Nil(t, o.Signal)
		case controls.CauseSignal:
			require.Empty(t, o.Service)
			require.Equal(t, os.Interrupt, o.Signal)
		default:
			t.Fatalf("cause %q", o.Cause)
		}
	}
}

func TestTwoServicesGivingUpAtOnceRecordOne(t *testing.T) {
	t.Parallel()

	gate := make(chan struct{})
	errA, errB := errors.New("a failed"), errors.New("b failed")
	c := newQuietController(t, controls.WithStopOnGiveUp())
	c.Register("a", controls.WithStart(gated(gate, errA)))
	c.Register("b", controls.WithStart(gated(gate, errB)))
	c.Start()
	close(gate)

	requireDone(t, c, 2*time.Second)

	o := requireOutcome(t, c)
	require.Equal(t, controls.CauseGaveUp, o.Cause)

	switch o.Service {
	case "a":
		require.ErrorIs(t, o.Err, errA)
	case "b":
		require.ErrorIs(t, o.Err, errB)
	default:
		t.Fatalf("service %q", o.Service)
	}
}

func TestACompletingServiceCancelledByShutdownIsNotACompletion(t *testing.T) {
	t.Parallel()

	c := newQuietController(t)
	c.Register("job", controls.WithStart(blocksUntilDone), controls.WithRunToCompletion())
	c.Start()
	c.Stop()

	requireStopsWith(t, c, controls.CauseStop, "")
}

// Whether the return beats the message processor cannot be forced, so the
// outcome is held over many runs instead.
func TestAnImmediateCompletionStillStops(t *testing.T) {
	t.Parallel()

	for range 100 {
		c := newQuietController(t)
		c.Register("job", controls.WithStart(completesNow), controls.WithRunToCompletion())
		c.Start()

		o := requireStopsWith(t, c, controls.CauseCompleted, "job")
		require.NoError(t, o.Err)
		require.True(t, o.Complete(), "the triggering supervisor is never abandoned: %v", o.Unfinished)
	}
}

// --- Forwards ---

func TestWithoutAnOptInNothingStopsTheController(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	c := newQuietController(t, controls.WithDeathWatch(time.Millisecond, 1))
	c.Register("fails", controls.WithStart(failsWith(errors.New("boom"))))
	c.Register("exhausts", controls.WithStart(failsWith(errors.New("boom"))),
		controls.WithRestartPolicy(controls.RestartPolicy{MaxRestarts: 1, InitialBackoff: time.Millisecond}))
	c.Register("dead", watched(func() error { calls.Add(1); return errDied }, nil)...)
	c.Start()

	requireStillUp(t, c)
	require.Equal(t, controls.UnableToStart, c.GetState())
	require.Zero(t, calls.Load(), "no death watch without WithStopOnGiveUp")

	c.Stop()
	requireStopsWith(t, c, controls.CauseStop, "")
}

func TestADeadServerWithoutAnOptInStaysRunning(t *testing.T) {
	t.Parallel()

	c := newQuietController(t, controls.WithDeathWatch(time.Millisecond, 1))
	c.Register("dead", watched(func() error { return errDied }, nil)...)
	c.Start()

	requireStillUp(t, c)
	require.Equal(t, controls.Running, c.GetState())

	c.Stop()
	requireDone(t, c, 2*time.Second)
}

// Each way of giving up stops the controller, names the service, carries its
// error, and reaches consumers that keep up before Done (spec 0010 D2).
func TestEveryGiveUpPathStopsTheController(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")

	cases := []struct {
		name string
		opts []controls.ServiceOption
		kind controls.EventKind
		err  error
	}{
		{"no policy", []controls.ServiceOption{controls.WithStart(failsWith(errBoom))},
			controls.EventUnableToStart, errBoom},
		{"exhausted", []controls.ServiceOption{
			controls.WithStart(failsWith(errBoom)),
			controls.WithRestartPolicy(controls.RestartPolicy{MaxRestarts: 1, InitialBackoff: time.Millisecond}),
		}, controls.EventUnableToStart, controls.ErrRestartsExhausted},
		{"died after a clean start", watched(func() error { return errDied }, nil),
			controls.EventFailed, errDied},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var events eventLog

			c := newQuietController(t, controls.WithStopOnGiveUp(), controls.WithDeathWatch(time.Millisecond, 3),
				controls.WithOnEvent(events.record))
			errs := c.Errors()

			var channel []error

			drained := make(chan struct{})

			go func() {
				defer close(drained)

				for err := range errs {
					channel = append(channel, err)
				}
			}()

			c.Register("svc", tc.opts...)
			c.Start()

			o := requireStopsWith(t, c, controls.CauseGaveUp, "svc")
			require.ErrorIs(t, o.Err, tc.err)
			<-drained

			ev := events.all()
			require.NotEmpty(t, ev)
			last := ev[len(ev)-1]
			require.Equal(t, tc.kind, last.Kind)
			require.Same(t, o.Err, last.Err)
			require.NotEmpty(t, channel)
			require.Same(t, o.Err, channel[len(channel)-1])
		})
	}
}

// The event is published before the stop: its log line sees the state before
// Stopping (spec 0010 D2).
func TestTheTerminalEventIsPublishedBeforeTheStop(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		opts  []controls.ServiceOption
		state controls.State
	}{
		{"no policy", []controls.ServiceOption{controls.WithStart(failsWith(errors.New("boom")))}, controls.UnableToStart},
		{"death watch", watched(func() error { return errDied }, nil), controls.Running},
	} {
		c, logs := gatedController(t, "", controls.WithStopOnGiveUp(), controls.WithDeathWatch(time.Millisecond, 1))
		c.Register("svc", tc.opts...)
		c.Start()
		requireDone(t, c, 2*time.Second)

		require.Equal(t, []controls.State{tc.state}, logs.failureStates(), tc.name)
	}
}

func TestTheDeathWatchCountsConsecutiveFailures(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	flaky := func() error {
		if calls.Add(1)%3 == 0 {
			return nil
		}

		return errDied
	}

	c := newQuietController(t, controls.WithStopOnGiveUp(), controls.WithDeathWatch(time.Millisecond, 3))
	c.Register("flaky", watched(flaky, nil)...)
	c.Start()

	awaitTrue(t, 2*time.Second, "twenty polls", func() bool { return calls.Load() >= 20 })
	requireStillUp(t, c)

	c.Stop()
	requireStopsWith(t, c, controls.CauseStop, "")
}

// A watch breach stops the controller and never calls the service's stop:
// only shutdown does.
func TestTheDeathWatchStopsOnTheThresholdWithoutStoppingTheService(t *testing.T) {
	t.Parallel()

	var calls, stops atomic.Int64

	c := newQuietController(t, controls.WithStopOnGiveUp(), controls.WithDeathWatch(time.Millisecond, 3))
	c.Register("dead", watched(func() error { calls.Add(1); return errDied }, &stops)...)
	c.Start()

	o := requireStopsWith(t, c, controls.CauseGaveUp, "dead")
	require.Equal(t, int64(3), calls.Load())
	require.Equal(t, int64(1), stops.Load(), "the shutdown's stop, and only that")
	require.Equal(t, "health check failed: serve loop died", o.Err.Error())
	require.NotErrorIs(t, o.Err, controls.ErrRestartsExhausted)

	info, _ := c.GetServiceInfo("dead")
	require.Same(t, o.Err, info.Error)
}

func TestADeathWatchedPanicBreachesAtOnce(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		opts  []controls.ControllerOpt
		calls int64
	}{
		{nil, 1},
		{[]controls.ControllerOpt{controls.WithProbePanicsAsFailures()}, 3},
	} {
		var calls atomic.Int64

		c := newQuietController(t, append(tc.opts, controls.WithStopOnGiveUp(),
			controls.WithDeathWatch(time.Millisecond, 3))...)
		c.Register("svc", watched(func() error { calls.Add(1); return panicsInStatus() }, nil)...)
		c.Start()

		o := requireStopsWith(t, c, controls.CauseGaveUp, "svc")
		require.ErrorIs(t, o.Err, controls.ErrPanicked)
		require.Equal(t, tc.calls, calls.Load())
	}
}

func TestABlockingWatchedStatusIsAbandonedNotAHang(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	polled := make(chan struct{})

	var once sync.Once

	c := newQuietController(t, controls.WithStopOnGiveUp(), controls.WithDeathWatch(time.Millisecond, 1),
		controls.WithShutdownTimeout(100*time.Millisecond))
	c.Register("svc", watched(func() error {
		once.Do(func() { close(polled) })
		<-release

		return nil
	}, nil)...)
	c.Start()
	<-polled
	c.Stop()

	o := requireStopsWith(t, c, controls.CauseStop, "")
	require.Contains(t, o.Unfinished, controls.Unfinished{Service: "svc", Reason: controls.SupervisorAbandoned})
}

// A transport server returns nil once bound and reports a later serve failure
// from a sticky cell its Status reads.
func TestATransportShapedServerThatDiesStopsTheController(t *testing.T) {
	t.Parallel()

	var serveErr atomic.Pointer[error]

	die := make(chan struct{})
	c := newQuietController(t, controls.WithStopOnGiveUp(), controls.WithDeathWatch(5*time.Millisecond, 3))
	c.Register("http",
		controls.WithStart(func(context.Context) error {
			go func() {
				<-die
				serveErr.Store(&errDied)
			}()

			return nil
		}),
		controls.WithStatus(func() error {
			if p := serveErr.Load(); p != nil {
				return *p
			}

			return nil
		}))
	c.Start()

	requireStillUp(t, c)
	close(die)

	o := requireStopsWith(t, c, controls.CauseGaveUp, "http")
	require.ErrorIs(t, o.Err, errDied)
}

func TestWithDeathWatchSetsTheIntervalAndKeepsDefaultsForZero(t *testing.T) {
	t.Parallel()

	var fast atomic.Int64

	c := newQuietController(t, controls.WithStopOnGiveUp(), controls.WithDeathWatch(20*time.Millisecond, 1))
	c.Register("dead", watched(func() error { fast.Add(1); return errDied }, nil)...)
	c.Start()

	start := time.Now()
	requireStopsWith(t, c, controls.CauseGaveUp, "dead")
	require.Less(t, time.Since(start), time.Second)
	require.Equal(t, int64(1), fast.Load(), "a threshold of 1 stops on the first failed poll")

	var slow atomic.Int64

	d := newQuietController(t, controls.WithStopOnGiveUp(), controls.WithDeathWatch(0, 0))
	d.Register("dead", watched(func() error { slow.Add(1); return errDied }, nil)...)
	d.Start()

	requireStillUp(t, d)
	require.Zero(t, slow.Load(), "a zero interval is the 10s default, not a busy poll")

	d.Stop()
	requireDone(t, d, 2*time.Second)
}

// Declaring completion is the service's opt-in to giving up; a non-declared
// service beside it still stops nothing (spec 0010 D5).
func TestACompletingServiceThatFailsForGoodGivesUp(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	c := newQuietController(t)
	c.Register("job", controls.WithStart(failsWith(errBoom)), controls.WithRunToCompletion())
	c.Start()

	o := requireStopsWith(t, c, controls.CauseGaveUp, "job")
	require.ErrorIs(t, o.Err, errBoom)
}

func TestAnOrdinaryFailureBesideACompletingServiceStopsNothing(t *testing.T) {
	t.Parallel()

	c := newQuietController(t)
	c.Register("job", controls.WithStart(blocksUntilDone), controls.WithRunToCompletion())
	c.Register("fails", controls.WithStart(failsWith(errors.New("boom"))))
	c.Start()

	requireStillUp(t, c)

	c.Stop()
	requireStopsWith(t, c, controls.CauseStop, "")
}

// Only nil is completion; a live context.Canceled or valid error is a failure
// (spec 0010 D6).
func TestOnlyNilIsCompletion(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
		opts []controls.ControllerOpt
	}{
		{"context.Canceled", context.Canceled, nil},
		{"a valid error", errors.New("expected"),
			[]controls.ControllerOpt{controls.WithValidError(func(error) bool { return true })}},
	} {
		c := newQuietController(t, tc.opts...)
		c.Register("job", controls.WithStart(failsWith(tc.err)), controls.WithRunToCompletion())
		c.Start()

		o := requireStopsWith(t, c, controls.CauseGaveUp, "job")
		require.ErrorIs(t, o.Err, tc.err, tc.name)
	}
}

func TestACompletingServiceRestartsThenCompletes(t *testing.T) {
	t.Parallel()

	var (
		runs   atomic.Int64
		events eventLog
	)

	c := newQuietController(t, controls.WithOnEvent(events.record))
	c.Register("job",
		controls.WithStart(func(context.Context) error {
			if runs.Add(1) == 1 {
				return errors.New("first attempt")
			}

			return nil
		}),
		controls.WithStatus(func() error { return errDied }),
		controls.WithRestartPolicy(controls.RestartPolicy{
			MaxRestarts: 2, InitialBackoff: time.Millisecond,
			HealthFailureThreshold: 1, HealthCheckInterval: time.Millisecond,
		}),
		controls.WithRunToCompletion())
	c.Start()

	requireStopsWith(t, c, controls.CauseCompleted, "job")
	require.Equal(t, []controls.EventKind{controls.EventRetrying}, events.kinds(),
		"a completed service is not health-monitored")
}

func TestADeclaredServiceWithNoStartCompletesAtOnce(t *testing.T) {
	t.Parallel()

	c := newQuietController(t)
	c.Register("noop", controls.WithRunToCompletion())
	c.Start()
	requireStopsWith(t, c, controls.CauseCompleted, "noop")

	gate := make(chan struct{})
	d := newQuietController(t, controls.WithCompletion(controls.CompleteOnAll))
	d.Register("noop", controls.WithRunToCompletion())
	d.Register("job", controls.WithStart(gated(gate, nil)), controls.WithRunToCompletion())
	d.Start()

	requireStillUp(t, d)
	close(gate)
	requireStopsWith(t, d, controls.CauseCompleted, "job")
}

func TestCompleteOnAllWaitsForTheLast(t *testing.T) {
	t.Parallel()

	gates := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	c := newQuietController(t, controls.WithCompletion(controls.CompleteOnAll))

	for i, name := range []string{"a", "b", "c"} {
		c.Register(name, controls.WithStart(gated(gates[i], nil)), controls.WithRunToCompletion())
	}

	c.Register("server", controls.WithStart(completesNow))
	c.Start()

	close(gates[0])
	close(gates[1])
	requireStillUp(t, c)
	require.Equal(t, controls.Running, c.GetState())

	close(gates[2])
	requireStopsWith(t, c, controls.CauseCompleted, "c")
}

func TestCompleteOnAllGivesUpOnAFailure(t *testing.T) {
	t.Parallel()

	gate := make(chan struct{})
	c := newQuietController(t, controls.WithCompletion(controls.CompleteOnAll))
	c.Register("done", controls.WithStart(completesNow), controls.WithRunToCompletion())
	c.Register("broken", controls.WithStart(gated(gate, errors.New("boom"))), controls.WithRunToCompletion())
	c.Start()

	requireStillUp(t, c)
	close(gate)
	requireStopsWith(t, c, controls.CauseGaveUp, "broken")
}

func TestCompleteOnFirstAndTheDefaultStopOnTheFirst(t *testing.T) {
	t.Parallel()

	for _, opts := range [][]controls.ControllerOpt{nil, {controls.WithCompletion(controls.CompleteOnFirst)}} {
		c := newQuietController(t, opts...)
		c.Register("quick", controls.WithStart(completesNow), controls.WithRunToCompletion())
		c.Register("slow", controls.WithStart(blocksUntilDone), controls.WithRunToCompletion())
		c.Start()

		requireStopsWith(t, c, controls.CauseCompleted, "quick")
	}
}

func TestCompleteOnAllRacingCompletionsStopOnce(t *testing.T) {
	t.Parallel()

	for range 100 {
		gate := make(chan struct{})
		c := newQuietController(t, controls.WithCompletion(controls.CompleteOnAll))

		for _, name := range []string{"a", "b", "c"} {
			c.Register(name, controls.WithStart(gated(gate, nil)), controls.WithRunToCompletion())
		}

		c.Start()
		close(gate)

		requireDone(t, c, 2*time.Second)

		o := requireOutcome(t, c)
		require.Equal(t, controls.CauseCompleted, o.Cause)
		require.Contains(t, []string{"a", "b", "c"}, o.Service)
	}
}

func TestCompleteOnAllWithNothingDeclaredDoesNothing(t *testing.T) {
	t.Parallel()

	c := newQuietController(t, controls.WithCompletion(controls.CompleteOnAll))
	c.Register("server", controls.WithStart(completesNow))
	c.Start()

	requireStillUp(t, c)

	c.Stop()
	requireStopsWith(t, c, controls.CauseStop, "")
}

// A service registered after Start is never supervised, so it is not counted.
func TestALateDeclaredServiceIsNotCounted(t *testing.T) {
	t.Parallel()

	gate := make(chan struct{})
	c := newQuietController(t, controls.WithCompletion(controls.CompleteOnAll))
	c.Register("early", controls.WithStart(gated(gate, nil)), controls.WithRunToCompletion())
	c.Start()
	c.Register("late", controls.WithStart(blocksUntilDone), controls.WithRunToCompletion())
	close(gate)

	requireStopsWith(t, c, controls.CauseCompleted, "early")
}

func TestAnUnknownCompletionPolicyWarnsAndCompletesOnTheFirst(t *testing.T) {
	t.Parallel()

	c, logs := loggedController(t, controls.WithCompletion(controls.CompletionPolicy(7)))
	c.Register("quick", controls.WithStart(completesNow), controls.WithRunToCompletion())
	c.Register("slow", controls.WithStart(blocksUntilDone), controls.WithRunToCompletion())
	c.Start()

	requireStopsWith(t, c, controls.CauseCompleted, "quick")
	require.Equal(t, 1, logs.count("unknown completion policy; completing on the first"))
}

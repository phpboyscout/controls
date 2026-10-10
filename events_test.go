package controls_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.com/phpboyscout/go/controls"
)

// eventLog collects what a WithOnEvent callback was given.
type eventLog struct {
	mu     sync.Mutex
	events []controls.ServiceEvent
}

func (l *eventLog) record(ev controls.ServiceEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.events = append(l.events, ev)
}

func (l *eventLog) all() []controls.ServiceEvent {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]controls.ServiceEvent(nil), l.events...)
}

func (l *eventLog) kinds() []controls.EventKind {
	var kinds []controls.EventKind

	for _, ev := range l.all() {
		kinds = append(kinds, ev.Kind)
	}

	return kinds
}

// undelivered is the count on the drain's warning for consumer, or -1.
func (h *recordingHandler) undelivered(consumer string) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, r := range h.records {
		if r.Message != "events undelivered at the shutdown deadline" {
			continue
		}

		var (
			mine  bool
			count int64 = -1
		)

		r.Attrs(func(a slog.Attr) bool {
			mine = mine || (a.Key == "consumer" && a.Value.String() == consumer)
			if a.Key == "count" {
				count = a.Value.Int64()
			}

			return true
		})

		if mine {
			return count
		}
	}

	return -1
}

// failuresLogged counts the failures logged for service. A failure is logged
// after it is decided and before it is queued, so once it is counted here it
// reaches every consumer even if shutdown begins next (spec 0008 D6, D7).
func (h *recordingHandler) failuresLogged(service string) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	n := 0

	for _, r := range h.records {
		if r.Level != slog.LevelError {
			continue
		}

		named, kinded := false, false

		r.Attrs(func(a slog.Attr) bool {
			named = named || (a.Key == "service_name" && a.Value.String() == service)
			kinded = kinded || a.Key == "kind"

			return true
		})

		if named && kinded {
			n++
		}
	}

	return n
}

func failsWith(err error) controls.StartFunc {
	return func(context.Context) error { return err }
}

// gatedFailure fails, numbering each run, once gate is closed.
func gatedFailure(gate <-chan struct{}) controls.StartFunc {
	var runs atomic.Int64

	return func(context.Context) error {
		<-gate

		return fmt.Errorf("run %d", runs.Add(1))
	}
}

func TestEventsForAServiceThatNeverStarts(t *testing.T) {
	t.Parallel()

	var got eventLog

	c := newQuietController(t, controls.WithOnEvent(got.record))
	c.Register("svc", controls.WithStart(failsWith(errors.New("boom"))),
		controls.WithRestartPolicy(controls.RestartPolicy{MaxRestarts: 2, InitialBackoff: time.Millisecond}))
	c.Start()

	awaitTrue(t, 2*time.Second, "three events", func() bool { return len(got.all()) == 3 })
	c.Stop()
	requireDone(t, c, 2*time.Second)

	events := got.all()
	require.Equal(t, []controls.EventKind{controls.EventRetrying, controls.EventRetrying, controls.EventUnableToStart}, got.kinds())
	require.Equal(t, 1, events[0].Restarts)
	require.Equal(t, 2, events[1].Restarts)
	require.Equal(t, 2, events[2].Restarts)
	require.ErrorIs(t, events[2].Err, controls.ErrRestartsExhausted)

	for _, ev := range events {
		require.Equal(t, "svc", ev.Service)
	}
}

func TestANoPolicyFailureIsOneUnableToStart(t *testing.T) {
	t.Parallel()

	var got eventLog

	errBoom := errors.New("boom")

	c := newQuietController(t, controls.WithOnEvent(got.record))
	c.Register("svc", controls.WithStart(failsWith(errBoom)))
	c.Start()

	awaitTrue(t, 2*time.Second, "the event", func() bool { return len(got.all()) == 1 })
	c.Stop()
	requireDone(t, c, 2*time.Second)

	require.Equal(t, []controls.ServiceEvent{{Service: "svc", Kind: controls.EventUnableToStart, Err: errBoom}}, got.all())
}

// A health breach has no Start error, so its retry carries the health error,
// and an exhaustion after breaches carries the bare sentinel, message unchanged
// (spec 0006 D2).
func TestEventsAfterHealthBreaches(t *testing.T) {
	t.Parallel()

	var got eventLog

	c := newQuietController(t, controls.WithOnEvent(got.record))
	c.Register("svc",
		controls.WithStatus(func() error { return errors.New("unhealthy") }),
		controls.WithRestartPolicy(controls.RestartPolicy{
			MaxRestarts: 1, HealthFailureThreshold: 1,
			HealthCheckInterval: 5 * time.Millisecond, InitialBackoff: time.Millisecond,
		}))
	c.Start()

	awaitTrue(t, 2*time.Second, "two events", func() bool { return len(got.all()) == 2 })
	c.Stop()
	requireDone(t, c, 2*time.Second)

	events := got.all()
	require.Equal(t, []controls.EventKind{controls.EventRetrying, controls.EventFailed}, got.kinds())
	require.ErrorContains(t, events[0].Err, "health check failed")
	require.Equal(t, controls.ErrRestartsExhausted.Error(), events[1].Err.Error())
	require.Equal(t, 1, events[1].Restarts)
}

// A callback blocked on a first event leaves later ones queued, where retries
// of one service collapse to the newest and terminal events stay.
func TestRetriesMergeBehindASlowCallback(t *testing.T) {
	t.Parallel()

	var got eventLog

	held := make(chan struct{})
	release := make(chan struct{})
	gate := make(chan struct{})

	var once sync.Once

	logs := &recordingHandler{}

	c := controls.NewController(context.Background(), controls.WithLogger(slog.New(logs)),
		controls.WithOnEvent(func(ev controls.ServiceEvent) {
			got.record(ev)
			once.Do(func() { close(held); <-release })
		}))
	c.Register("blocker", controls.WithStart(failsWith(errors.New("first"))))
	c.Register("retrier", controls.WithStart(gatedFailure(gate)),
		controls.WithRestartPolicy(controls.RestartPolicy{MaxRestarts: 3, InitialBackoff: time.Millisecond}))
	c.Register("twin", controls.WithStart(gatedFailure(gate)),
		controls.WithRestartPolicy(controls.RestartPolicy{InitialBackoff: time.Hour}))
	c.Register("twin", controls.WithStart(gatedFailure(gate)),
		controls.WithRestartPolicy(controls.RestartPolicy{InitialBackoff: time.Hour}))
	c.Register("ender", controls.WithStart(gatedFailure(gate)))
	c.Start()

	<-held
	close(gate)

	awaitTrue(t, 2*time.Second, "every failure to be queued", func() bool {
		return logs.failuresLogged("retrier") == 4 && logs.failuresLogged("twin") == 2 &&
			logs.failuresLogged("ender") == 1
	})

	close(release)
	awaitTrue(t, 2*time.Second, "delivery", func() bool { return len(got.all()) == 6 })
	c.Stop()
	requireDone(t, c, 2*time.Second)

	var retrier []controls.ServiceEvent

	twins, enders := 0, 0

	for _, ev := range got.all()[1:] {
		switch ev.Service {
		case "retrier":
			retrier = append(retrier, ev)
		case "twin":
			twins++
		case "ender":
			enders++
		}
	}

	require.Len(t, retrier, 2, "three retries and an exhaustion")
	require.Equal(t, controls.EventRetrying, retrier[0].Kind)
	require.Equal(t, 3, retrier[0].Restarts)
	require.Equal(t, controls.EventUnableToStart, retrier[1].Kind)
	require.Equal(t, 2, twins, "two services sharing a name must not merge")
	require.Equal(t, 1, enders)
}

func TestAPanickingCallbackStillGetsTheNextEvent(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	second := make(chan struct{})

	c := newQuietController(t, controls.WithOnEvent(func(controls.ServiceEvent) {
		if calls.Add(1) == 1 {
			panic("callback bug")
		}

		close(second)
	}))
	c.Register("a", controls.WithStart(failsWith(errors.New("a"))))
	c.Register("b", controls.WithStart(failsWith(errors.New("b"))))
	c.Start()

	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("the event after a panic was not delivered")
	}

	c.Stop()
	requireDone(t, c, 2*time.Second)
}

// Outcome reads false from a callback during the run, and Stop from a callback
// completes the shutdown; a callback delivered during the drain sees the
// outcome written.
func TestACallbackCanStopTheController(t *testing.T) {
	t.Parallel()

	var (
		duringRun   atomic.Bool
		duringDrain atomic.Bool
		calls       atomic.Int64
	)

	var c *controls.Controller

	logs := &recordingHandler{}

	c = controls.NewController(context.Background(), controls.WithLogger(slog.New(logs)), controls.WithOnEvent(func(controls.ServiceEvent) {
		if calls.Add(1) == 1 {
			_, ok := c.Outcome()
			duringRun.Store(ok)

			for logs.failuresLogged("a")+logs.failuresLogged("b") < 2 {
				time.Sleep(time.Millisecond)
			}

			c.Stop()

			for !c.IsStopped() {
				time.Sleep(time.Millisecond)
			}

			return
		}

		_, ok := c.Outcome()
		duringDrain.Store(ok)
	}))
	c.Register("a", controls.WithStart(failsWith(errors.New("a"))))
	c.Register("b", controls.WithStart(failsWith(errors.New("b"))))
	c.Start()

	requireDone(t, c, 2*time.Second)

	require.Equal(t, controls.CauseStop, requireOutcome(t, c).Cause)
	require.False(t, duringRun.Load())
	require.Equal(t, int64(2), calls.Load())
	require.True(t, duringDrain.Load())
}

// The callback's Stop wins the transition while the processor is held between
// receiving a direct Stop and acting on it, so its hand-off has no receiver.
// The cancel releases it; without that it would wait on Done, and Done on its
// drain, for the whole budget (spec 0008 D1, R5).
func TestACallbacksSupersededStopDoesNotWaitOnItsOwnDrain(t *testing.T) {
	t.Parallel()

	hold := newHoldOn("received Stop message")
	fail := make(chan struct{})
	stopping := make(chan struct{})

	var c *controls.Controller

	c = controls.NewController(context.Background(), controls.WithLogger(slog.New(hold)),
		controls.WithShutdownTimeout(3*time.Second),
		controls.WithOnEvent(func(controls.ServiceEvent) {
			close(stopping)
			c.Stop()
		}))
	c.Register("svc", controls.WithStart(gatedFailure(fail)))
	c.Start()

	go func() { c.Messages() <- controls.Stop }()

	<-hold.reached
	close(fail)
	<-stopping
	time.Sleep(20 * time.Millisecond)

	started := time.Now()

	close(hold.release)
	requireDone(t, c, 2*time.Second)
	require.Less(t, time.Since(started), time.Second)
}

// A callback that does not return holds Done for the budget, not forever;
// IsStopped and Outcome are answered before, and other services carry on.
func TestABlockedCallbackHoldsDoneOnlyForTheBudget(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	release := make(chan struct{})
	held := make(chan struct{})
	logs := &recordingHandler{}

	c := controls.NewController(context.Background(), controls.WithLogger(slog.New(logs)),
		controls.WithShutdownTimeout(300*time.Millisecond),
		controls.WithOnEvent(func(controls.ServiceEvent) {
			if calls.Add(1) == 1 {
				close(held)
				<-release
			}
		}))
	c.Register("a", controls.WithStart(failsWith(errors.New("a"))))
	c.Register("b", controls.WithStart(failsWith(errors.New("b"))))
	c.Register("retrier", controls.WithStart(failsWith(errors.New("again"))),
		controls.WithRestartPolicy(controls.RestartPolicy{InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}))
	c.Start()

	<-held
	awaitTrue(t, 2*time.Second, "restarts to carry on behind the callback", func() bool {
		return findInfo(t, c, "retrier").RestartCount >= 5
	})

	c.Stop()
	awaitTrue(t, 2*time.Second, "Stopped", c.IsStopped)

	_, ok := c.Outcome()
	require.True(t, ok)

	select {
	case <-c.Done():
		t.Fatal("Done closed while events were still queued and budget remained")
	default:
	}

	requireDone(t, c, 2*time.Second)
	require.True(t, logs.warnMentioning("events undelivered", "WithOnEvent"))

	close(release)
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, int64(1), calls.Load(), "nothing is delivered after abandonment")
}

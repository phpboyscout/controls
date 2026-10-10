package controls_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.com/phpboyscout/go/controls"
)

// holdOn is a slog handler that blocks the goroutine logging msg until
// released, once, so a test can hold the shutdown sequence at a named point.
type holdOn struct {
	msg     string
	reached chan struct{}
	release chan struct{}
	once    *sync.Once
}

func newHoldOn(msg string) holdOn {
	return holdOn{msg: msg, reached: make(chan struct{}), release: make(chan struct{}), once: &sync.Once{}}
}

func (h holdOn) Enabled(context.Context, slog.Level) bool { return true }
func (h holdOn) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h holdOn) WithGroup(string) slog.Handler            { return h }

func (h holdOn) Handle(_ context.Context, r slog.Record) error {
	if r.Message != h.msg {
		return nil
	}

	held := false

	h.once.Do(func() { held = true })

	if held {
		close(h.reached)
		<-h.release
	}

	return nil
}

func requireDone(t *testing.T, c *controls.Controller, within time.Duration) {
	t.Helper()

	select {
	case <-c.Done():
	case <-time.After(within):
		t.Fatalf("Done did not close within %s", within)
	}
}

func requireOutcome(t *testing.T, c *controls.Controller) controls.Outcome {
	t.Helper()

	o, ok := c.Outcome()
	require.True(t, ok, "Outcome should be written once Done has closed")

	return o
}

func TestOutcomeBeforeStartIsNotWritten(t *testing.T) {
	t.Parallel()

	c := newQuietController(t)

	_, ok := c.Outcome()
	require.False(t, ok)

	select {
	case <-c.Done():
		t.Fatal("Done closed before Start: nothing has stopped")
	default:
	}
}

func TestStopBeforeStartWritesNothingAndLeavesItStartable(t *testing.T) {
	t.Parallel()

	c := newQuietController(t)
	c.Stop()

	_, ok := c.Outcome()
	require.False(t, ok)

	c.Start()
	c.Stop()
	requireDone(t, c, 2*time.Second)
	require.Equal(t, controls.CauseStop, requireOutcome(t, c).Cause)
}

// SetState is public (spec 0003 D7), so a shutdown can begin with no winning
// transition: the message handler records the cause itself.
func TestStopSentAfterSetStateStoppingRecordsCauseStop(t *testing.T) {
	t.Parallel()

	c := newQuietController(t)
	c.Start()
	c.SetState(controls.Stopping)
	c.Messages() <- controls.Stop

	requireDone(t, c, 2*time.Second)

	o := requireOutcome(t, c)
	require.Equal(t, controls.CauseStop, o.Cause)
	require.True(t, o.Complete())
}

func TestSetStateStoppedWritesNoOutcome(t *testing.T) {
	t.Parallel()

	c := newQuietController(t)
	c.Start()
	c.SetState(controls.Stopped)

	require.True(t, c.IsStopped())

	_, ok := c.Outcome()
	require.False(t, ok, "a state string is not a shutdown")

	c.SetState(controls.Stopping)
	c.Messages() <- controls.Stop
	requireDone(t, c, 2*time.Second)
}

func TestEachTriggerRecordsItsCause(t *testing.T) {
	t.Parallel()

	errCause := errors.New("parent gave up")

	tests := []struct {
		name    string
		opts    []controls.ControllerOpt
		parent  func() (context.Context, func())
		trigger func(c *controls.Controller, cancelParent func())
		want    controls.StopCause
		wantErr error
		wantSig os.Signal
	}{
		{
			name:    "Stop",
			trigger: func(c *controls.Controller, _ func()) { c.Stop() },
			want:    controls.CauseStop,
		},
		{
			name:    "direct send",
			trigger: func(c *controls.Controller, _ func()) { c.Messages() <- controls.Stop },
			want:    controls.CauseStop,
		},
		{
			name:    "signal",
			opts:    []controls.ControllerOpt{controls.WithSignals()},
			trigger: func(c *controls.Controller, _ func()) { c.Signals() <- syscall.SIGTERM },
			want:    controls.CauseSignal,
			wantSig: syscall.SIGTERM,
		},
		{
			name: "parent cancelled with a cause",
			parent: func() (context.Context, func()) {
				ctx, cancel := context.WithCancelCause(context.Background())

				return ctx, func() { cancel(errCause) }
			},
			trigger: func(_ *controls.Controller, cancelParent func()) { cancelParent() },
			want:    controls.CauseParent,
			wantErr: errCause,
		},
		{
			name: "parent deadline",
			parent: func() (context.Context, func()) {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)

				return ctx, cancel
			},
			trigger: func(*controls.Controller, func()) {},
			want:    controls.CauseParent,
			wantErr: context.DeadlineExceeded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			parent, cancelParent := context.Background(), func() {}
			if tt.parent != nil {
				parent, cancelParent = tt.parent()
			}

			t.Cleanup(cancelParent)

			c := controls.NewController(parent,
				append([]controls.ControllerOpt{controls.WithLogger(slog.New(slog.DiscardHandler))}, tt.opts...)...)
			c.Start()

			tt.trigger(c, cancelParent)
			requireDone(t, c, 2*time.Second)

			o := requireOutcome(t, c)
			require.Equal(t, tt.want, o.Cause)
			require.NotEqual(t, controls.CauseGaveUp, o.Cause)
			require.NotEqual(t, controls.CauseCompleted, o.Cause)
			require.Equal(t, tt.wantSig, o.Signal)

			if tt.wantErr == nil {
				require.NoError(t, o.Err)
			} else {
				require.ErrorIs(t, o.Err, tt.wantErr)
			}

			require.ErrorIs(t, context.Cause(c.GetContext()), controls.ErrShutdown, "spec 0001 D3 stands")
		})
	}
}

func TestRacingTriggersRecordExactlyOneCause(t *testing.T) {
	t.Parallel()

	for range 100 {
		parent, cancel := context.WithCancelCause(context.Background())

		c := controls.NewController(parent, controls.WithLogger(slog.New(slog.DiscardHandler)), controls.WithSignals())
		c.Start()

		var wg sync.WaitGroup

		wg.Go(c.Stop)
		wg.Go(func() { cancel(errors.New("parent")) })
		wg.Go(func() { c.Signals() <- syscall.SIGINT })
		wg.Wait()

		requireDone(t, c, 2*time.Second)
		require.Contains(t,
			[]controls.StopCause{controls.CauseStop, controls.CauseParent, controls.CauseSignal},
			requireOutcome(t, c).Cause)

		cancel(nil)
	}
}

// The budget runs from the trigger, not from when the message processor gets
// to it: held 300ms at "Stopping Services" with a 200ms budget and a stop that
// never returns, Done closes at about 300ms. Measured from the processor it
// would be about 500ms.
func TestTheShutdownDeadlineStartsAtTheTrigger(t *testing.T) {
	t.Parallel()

	hold := newHoldOn("Stopping Services")
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })

	c := controls.NewController(context.Background(),
		controls.WithLogger(slog.New(hold)), controls.WithShutdownTimeout(200*time.Millisecond))
	c.Register("hangs", controls.WithStop(func(context.Context) { <-stuck }))
	c.Start()

	go func() {
		<-hold.reached
		time.Sleep(300 * time.Millisecond)
		close(hold.release)
	}()

	triggered := time.Now()

	c.Stop()
	requireDone(t, c, 2*time.Second)

	elapsed := time.Since(triggered)
	require.Less(t, elapsed, 420*time.Millisecond, "the budget was measured from the processor, not the trigger")

	o := requireOutcome(t, c)
	require.Equal(t, []controls.Unfinished{{Service: "hangs", Reason: controls.StopUnbudgeted}}, o.Unfinished)
}

// Today a Readiness probe that blocks holds the services mutex, and the stop
// sequence waits for it before any deadline applies (spec 0008 D10).
func TestABlockedProbeDoesNotBlockShutdown(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	c := controls.NewController(context.Background(),
		controls.WithLogger(slog.New(slog.DiscardHandler)), controls.WithShutdownTimeout(200*time.Millisecond))

	var once sync.Once

	c.Register("probed", controls.WithReadiness(func() error {
		once.Do(func() { close(entered) })
		<-release

		return nil
	}))
	c.Start()

	go c.Readiness()

	<-entered
	c.Stop()
	requireDone(t, c, 2*time.Second)
}

func TestOutcomeIsACopy(t *testing.T) {
	t.Parallel()

	c := newQuietController(t, controls.WithShutdownTimeout(50*time.Millisecond))
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })

	c.Register("hangs", controls.WithStop(func(context.Context) { <-stuck }))
	c.Start()

	var polls sync.WaitGroup

	stopPolling := make(chan struct{})

	polls.Go(func() {
		for {
			select {
			case <-stopPolling:
				return
			default:
				c.Outcome()
			}
		}
	})

	c.Stop()
	requireDone(t, c, 2*time.Second)
	close(stopPolling)
	polls.Wait()

	first := requireOutcome(t, c)
	require.Len(t, first.Unfinished, 1)
	first.Unfinished[0].Service = "mutated"

	require.Equal(t, "hangs", requireOutcome(t, c).Unfinished[0].Service)
}

func TestDoneIsOneChannel(t *testing.T) {
	t.Parallel()

	c := newQuietController(t)
	done := c.Done()
	require.Equal(t, done, c.Done())

	c.Start()
	c.Stop()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the channel Done returned before Start never closed")
	}
}

// Wait counts a service until it starts cleanly or its supervisor exits, so a
// failed first Start whose retry ignores cancellation keeps Wait blocked while
// Done closes within the budget (spec 0008 D3).
func TestDoneClosesWhereWaitDoesNot(t *testing.T) {
	t.Parallel()

	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })

	var runs atomic.Int64

	c := newQuietController(t, controls.WithShutdownTimeout(100*time.Millisecond))
	c.Register("wedges", controls.WithStart(func(context.Context) error {
		if runs.Add(1) == 1 {
			return errors.New("first run fails")
		}

		<-stuck

		return nil
	}), controls.WithRestartPolicy(controls.RestartPolicy{InitialBackoff: time.Millisecond}))
	c.Start()

	awaitTrue(t, 2*time.Second, "the retry to start", func() bool { return runs.Load() >= 2 })

	c.Stop()
	requireDone(t, c, 2*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	require.ErrorIs(t, c.WaitContext(ctx), context.DeadlineExceeded)
	require.Equal(t,
		[]controls.Unfinished{{Service: "wedges", Reason: controls.SupervisorAbandoned}},
		requireOutcome(t, c).Unfinished)
}

package controls_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.com/phpboyscout/go/controls"
)

// Stops run in reverse registration order: the last-registered is stopped
// first and finishes, the middle one hangs past the budget, and the
// first-registered's turn comes after the budget has gone.
func TestTheStopRecordSaysWhichStepsDidNotFinish(t *testing.T) {
	t.Parallel()

	errLate := errors.New("returned after being abandoned")
	release := make(chan struct{})
	returned := make(chan struct{})

	c := newQuietController(t, controls.WithShutdownTimeout(100*time.Millisecond))
	c.Register("first", controls.WithStop(func(context.Context) {}))
	c.Register("middle", controls.WithStopErr(func(context.Context) error {
		defer close(returned)
		<-release

		return errLate
	}))
	c.Register("last", controls.WithStop(func(context.Context) {}))
	c.Start()
	c.Stop()
	requireDone(t, c, 2*time.Second)

	require.Equal(t, []controls.Unfinished{
		{Service: "middle", Reason: controls.StopAbandoned},
		{Service: "first", Reason: controls.StopUnbudgeted},
	}, requireOutcome(t, c).Unfinished)

	close(release)
	<-returned

	info := findInfo(t, c, "middle")
	require.ErrorIs(t, info.StopErr, controls.ErrStopAbandoned, "a late result must not overwrite the record")
	require.ErrorIs(t, findInfo(t, c, "first").StopErr, controls.ErrStopAbandoned)
	require.NoError(t, findInfo(t, c, "last").StopErr)
}

func TestAFailedStopIsUnfinished(t *testing.T) {
	t.Parallel()

	errRelease := errors.New("listener still bound")

	tests := []struct {
		name string
		opt  controls.ServiceOption
		want error
	}{
		{"StopErrFunc returns an error", controls.WithStopErr(func(context.Context) error { return errRelease }), errRelease},
		{"StopErrFunc panics", controls.WithStopErr(func(context.Context) error { panic("boom") }), nil},
		{"WithStop panics", controls.WithStop(func(context.Context) { panic("boom") }), nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := newQuietController(t)
			c.Register("svc", tt.opt)
			c.Start()
			c.Stop()
			requireDone(t, c, 2*time.Second)

			unfinished := requireOutcome(t, c).Unfinished
			require.Len(t, unfinished, 1)
			require.Equal(t, controls.StopFailed, unfinished[0].Reason)
			require.Error(t, unfinished[0].Err)
			require.Equal(t, unfinished[0].Err, findInfo(t, c, "svc").StopErr)

			if tt.want != nil {
				require.ErrorIs(t, unfinished[0].Err, tt.want)
			}
		})
	}
}

func TestACleanShutdownStopClearsAnEarlierStopErr(t *testing.T) {
	t.Parallel()

	var stops, probes atomic.Int64

	c := newQuietController(t)
	c.Register("svc",
		controls.WithStopErr(func(context.Context) error {
			if stops.Add(1) == 1 {
				return errors.New("health-triggered stop failed")
			}

			return nil
		}),
		controls.WithStatus(func() error {
			if probes.Add(1) == 1 {
				return errors.New("unhealthy")
			}

			return nil
		}),
		controls.WithRestartPolicy(controls.RestartPolicy{
			HealthFailureThreshold: 1, HealthCheckInterval: 5 * time.Millisecond, InitialBackoff: time.Millisecond,
		}),
	)
	c.Start()

	awaitTrue(t, 2*time.Second, "the health-triggered stop to record its error", func() bool {
		return findInfo(t, c, "svc").StopErr != nil
	})

	c.Stop()
	requireDone(t, c, 2*time.Second)

	require.NoError(t, findInfo(t, c, "svc").StopErr)
	require.True(t, requireOutcome(t, c).Complete())
}

// A breach noticed after the cancel is shutdown, not a failure (spec 0008 D6),
// and once shutdown has begun only the shutdown writes StopErr (D4). This
// covers the post-cancel window only.
func TestAHealthBreachAfterTheCancelIsNotReported(t *testing.T) {
	t.Parallel()

	var (
		stops  atomic.Int64
		events atomic.Int64
	)

	inStatus := make(chan struct{})
	failStatus := make(chan struct{})
	shutdownStopped := make(chan struct{})

	var once sync.Once

	c := newQuietController(t, controls.WithOnEvent(func(controls.ServiceEvent) { events.Add(1) }))
	errs := c.Errors()

	c.Register("svc",
		controls.WithStopErr(func(context.Context) error {
			n := stops.Add(1)
			if n == 1 {
				close(shutdownStopped)
			}

			return fmt.Errorf("stop %d", n)
		}),
		controls.WithStatus(func() error {
			once.Do(func() { close(inStatus) })
			<-failStatus

			return errors.New("unhealthy")
		}),
		controls.WithRestartPolicy(controls.RestartPolicy{
			HealthFailureThreshold: 1, HealthCheckInterval: 5 * time.Millisecond, InitialBackoff: time.Millisecond,
		}),
	)
	c.Start()

	<-inStatus

	go c.Stop()

	<-shutdownStopped
	close(failStatus)

	requireDone(t, c, 2*time.Second)

	for err := range errs {
		t.Errorf("nothing should be reported after the cancel, got %v", err)
	}

	require.Zero(t, events.Load())
	require.EqualError(t, findInfo(t, c, "svc").StopErr, "stop 1")
}

func TestAHungServiceIsTwoUnfinishedSteps(t *testing.T) {
	t.Parallel()

	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })

	c := newQuietController(t, controls.WithShutdownTimeout(100*time.Millisecond))
	c.Register("hung",
		controls.WithStart(func(context.Context) error { <-stuck; return nil }),
		controls.WithStop(func(context.Context) { <-stuck }),
	)
	c.Start()
	c.Stop()
	requireDone(t, c, 2*time.Second)

	require.Equal(t, []controls.Unfinished{
		{Service: "hung", Reason: controls.StopAbandoned},
		{Service: "hung", Reason: controls.SupervisorAbandoned},
	}, requireOutcome(t, c).Unfinished)
}

// Two services may share a name; the record is per step, so the one that hung
// is not confused with the one that did not.
func TestTheStopRecordIsPerStepNotPerName(t *testing.T) {
	t.Parallel()

	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })

	c := newQuietController(t, controls.WithShutdownTimeout(100*time.Millisecond))
	// Stopped last, so the other twin's stop is not left without budget.
	c.Register("twin", controls.WithStop(func(context.Context) { <-stuck }))
	c.Register("twin", controls.WithStop(func(context.Context) {}))
	c.Start()
	c.Stop()
	requireDone(t, c, 2*time.Second)

	require.Equal(t,
		[]controls.Unfinished{{Service: "twin", Reason: controls.StopAbandoned}},
		requireOutcome(t, c).Unfinished)
}

func TestACleanShutdownIsComplete(t *testing.T) {
	t.Parallel()

	c := newQuietController(t)
	c.Register("a", controls.WithStopErr(func(context.Context) error { return nil }))
	c.Register("b", controls.WithStop(func(context.Context) {}))
	c.Start()
	c.Stop()
	requireDone(t, c, 2*time.Second)

	require.True(t, requireOutcome(t, c).Complete())
}

package controls_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.com/phpboyscout/go/controls"
)

// errorTap is a slog handler that forwards the "error" attribute of every
// record to a channel. The controller's own handler competes with a test for
// Errors(), so whichever of the two wins, the test sees the error here.
type errorTap struct {
	out chan<- error
}

func (h errorTap) Enabled(context.Context, slog.Level) bool { return true }
func (h errorTap) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h errorTap) WithGroup(string) slog.Handler            { return h }

func (h errorTap) Handle(_ context.Context, r slog.Record) error {
	r.Attrs(func(a slog.Attr) bool {
		if err, ok := a.Value.Any().(error); ok && a.Key == "error" {
			h.out <- err
		}

		return true
	})

	return nil
}

// tappedController returns a controller whose forwarded errors all reach errs,
// whether the test or the controller's handler received them first.
func tappedController(errs chan error) *controls.Controller {
	c := controls.NewController(context.Background(), controls.WithLogger(slog.New(errorTap{out: errs})))
	c.SetErrorsChannel(errs)

	return c
}

// exhaustionOf drains errs until one carries ErrRestartsExhausted, or gives up.
func exhaustionOf(t *testing.T, errs <-chan error) error {
	t.Helper()

	deadline := time.After(5 * time.Second)

	for {
		select {
		case err := <-errs:
			if errors.Is(err, controls.ErrRestartsExhausted) {
				return err
			}
		case <-deadline:
			t.Fatal("no exhaustion error arrived on Errors()")
		}
	}
}

func TestExhaustionAfterFailedRunsCarriesTheSentinelAndTheLastError(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	errs := make(chan error, 16)

	c := tappedController(errs)
	c.Register("svc",
		controls.WithStart(func(context.Context) error { return errBoom }),
		controls.WithRestartPolicy(controls.RestartPolicy{MaxRestarts: 1, InitialBackoff: time.Millisecond}),
	)
	c.Start()

	got := exhaustionOf(t, errs)

	c.Stop()
	c.Wait()

	require.ErrorIs(t, got, controls.ErrRestartsExhausted)
	require.ErrorIs(t, got, errBoom, "the last error must stay reachable beside the sentinel")
	require.Equal(t, "max restarts exceeded: boom", got.Error(), "the message must not change")

	info, ok := c.GetServiceInfo("svc")
	require.True(t, ok)
	require.ErrorIs(t, info.Error, controls.ErrRestartsExhausted)
	require.ErrorIs(t, info.Error, errBoom)
	require.Equal(t, "max restarts exceeded: boom", info.Error.Error())
}

func TestHealthDrivenExhaustionIsTheSentinelAlone(t *testing.T) {
	t.Parallel()

	errs := make(chan error, 16)

	c := tappedController(errs)
	c.Register("svc",
		controls.WithStart(func(context.Context) error { return nil }),
		controls.WithStatus(func() error { return errors.New("unwell") }),
		controls.WithRestartPolicy(controls.RestartPolicy{
			MaxRestarts:            1,
			InitialBackoff:         time.Millisecond,
			HealthFailureThreshold: 1,
			HealthCheckInterval:    5 * time.Millisecond,
		}),
	)
	c.Start()

	got := exhaustionOf(t, errs)

	c.Stop()
	c.Wait()

	require.ErrorIs(t, got, controls.ErrRestartsExhausted)
	require.Equal(t, "max restarts exceeded", got.Error(), "with no last error the sentinel stands alone")
}

func TestAFailureWithNoPolicyIsNotExhaustion(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	errs := make(chan error, 16)

	c := tappedController(errs)
	c.Register("svc", controls.WithStart(func(context.Context) error { return errBoom }))
	c.Start()

	select {
	case err := <-errs:
		require.ErrorIs(t, err, errBoom)
		require.NotErrorIs(t, err, controls.ErrRestartsExhausted, "one failure with no retries is a failure, not exhaustion")
	case <-time.After(5 * time.Second):
		t.Fatal("no error arrived")
	}

	c.Stop()
	c.Wait()
}

func TestAChildsFailureCarriesTheSentinelAndItsLastError(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	failures := make(chan controls.Failure, 4)

	sup := controls.NewSupervisor(controls.WithOnFailure(func(f controls.Failure) { failures <- f }))
	require.NoError(t, sup.Start(context.Background()))

	require.NoError(t, sup.Attach(controls.Child{
		Name:          "worker",
		Start:         func(context.Context) error { return errBoom },
		RestartPolicy: &controls.RestartPolicy{MaxRestarts: 1, InitialBackoff: time.Millisecond},
	}))
	require.NoError(t, sup.Attach(controls.Child{
		Name:  "crasher",
		Start: func(context.Context) error { panic("bug") },
	}))

	byName := map[string]controls.Failure{}

	for len(byName) < 2 {
		select {
		case f := <-failures:
			byName[f.Name] = f
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 2 failures arrived", len(byName))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	sup.Stop(ctx)

	worker := byName["worker"]
	require.ErrorIs(t, worker.Err, controls.ErrRestartsExhausted)
	require.ErrorIs(t, worker.Err, errBoom)
	require.Equal(t, `controls: child "worker" exhausted its restart policy: boom`, worker.Err.Error())
	require.Equal(t, 1, worker.Restarts)

	crasher := byName["crasher"]
	require.True(t, crasher.Panicked)
	require.ErrorIs(t, crasher.Err, controls.ErrRestartsExhausted)
	require.Contains(t, crasher.Err.Error(), `controls: child "crasher" exhausted its restart policy: controls: child "crasher" panicked: bug`)
}

package controls_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/phpboyscout/go/controls"
)

// causeProbe registers a service that records context.Cause as observed by its
// StartFunc at the moment the controller tears it down. This is the seam every
// assertion in this file hangs off: a service sees only its context, so the
// cause it observes IS the published contract.
type causeProbe struct {
	observed chan error
}

func newCauseProbe() *causeProbe {
	return &causeProbe{observed: make(chan error, 1)}
}

func (p *causeProbe) register(t *testing.T, c *controls.Controller) {
	t.Helper()

	c.Register("probe",
		controls.WithStart(func(ctx context.Context) error {
			<-ctx.Done()
			p.observed <- context.Cause(ctx)

			return ctx.Err()
		}),
	)
}

func (p *causeProbe) cause(t *testing.T) error {
	t.Helper()

	select {
	case err := <-p.observed:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("service never observed its context being cancelled")

		return nil
	}
}

// TestCause_ErrShutdown_OnDirectStop is the baseline: a direct Stop() already
// reports ErrShutdown today. It guards against the fix regressing the case that
// currently works.
func TestCause_ErrShutdown_OnDirectStop(t *testing.T) {
	t.Parallel()

	c := controls.NewController(t.Context())

	probe := newCauseProbe()
	probe.register(t, c)

	c.Start()
	c.Stop()
	c.Wait()

	assert.ErrorIs(t, probe.cause(t), controls.ErrShutdown,
		"a direct Stop() must report ErrShutdown as the context cause")
}

// TestCause_ErrShutdown_OnParentCancel reproduces the defect. The how-to
// documents context.Cause(ctx) == ErrShutdown as "the reliable signal that this
// controller initiated the stop", but when the PARENT context is cancelled the
// child inherits context.Canceled and the controller's own cancel(ErrShutdown)
// is a no-op — first cancel wins.
//
// Expected to FAIL before the fix.
func TestCause_ErrShutdown_OnParentCancel(t *testing.T) {
	t.Parallel()

	parent, cancel := context.WithCancel(t.Context())
	c := controls.NewController(parent)

	probe := newCauseProbe()
	probe.register(t, c)

	c.Start()
	cancel() // upstream cancellation drives the shutdown
	c.Wait()

	assert.ErrorIs(t, probe.cause(t), controls.ErrShutdown,
		"a parent cancellation must still report ErrShutdown: the controller "+
			"is the thing stopping the service, whatever triggered it")
}

// TestCause_ErrShutdown_OnParentDeadline covers OQ1: an expired parent deadline
// must produce an ORDERLY teardown reporting ErrShutdown, not a context that is
// already dead on arrival.
//
// Expected to FAIL before the fix.
func TestCause_ErrShutdown_OnParentDeadline(t *testing.T) {
	t.Parallel()

	parent, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	c := controls.NewController(parent)

	probe := newCauseProbe()
	probe.register(t, c)

	c.Start()
	c.Wait()

	assert.ErrorIs(t, probe.cause(t), controls.ErrShutdown,
		"an expired parent deadline must drive a graceful stop reporting ErrShutdown")
}

// TestParentDeadline_StillStopsServices guards the behaviour severing must not
// lose: the parent's deadline must continue to stop the services, even though
// the controller no longer inherits cancellation from it.
func TestParentDeadline_StillStopsServices(t *testing.T) {
	t.Parallel()

	parent, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	c := controls.NewController(parent)

	stopped := make(chan struct{})
	c.Register("svc",
		controls.WithStart(func(ctx context.Context) error {
			<-ctx.Done()

			return ctx.Err()
		}),
		controls.WithStop(func(_ context.Context) { close(stopped) }),
	)

	c.Start()
	c.Wait()

	select {
	case <-stopped:
	default:
		t.Fatal("an expired parent deadline must still run the service's StopFunc")
	}

	assert.True(t, c.IsStopped(), "controller must reach Stopped")
}

// TestSignals_OffByDefault is D2: NewController must not register a
// process-global signal handler. Signal disposition belongs to the outermost
// layer, which for a gtb-based tool is the root command.
//
// Expected to FAIL before the fix.
func TestSignals_OffByDefault(t *testing.T) {
	t.Parallel()

	c := controls.NewController(t.Context())

	assert.Nil(t, c.Signals(),
		"NewController must not install a signal handler by default")
}

// TestWithSignals_OptsIn is the other half of D2: a standalone main with no CLI
// framework above it can still ask the controller to own signals.
//
// Expected to FAIL before the fix (WithSignals does not yet exist).
func TestWithSignals_OptsIn(t *testing.T) {
	t.Parallel()

	c := controls.NewController(t.Context(), controls.WithSignals())

	require.NotNil(t, c.Signals(),
		"WithSignals must give the controller a signal channel")

	// The channel must be buffered: signal.Notify never blocks, so an unbuffered
	// channel would drop a signal arriving before the handler goroutine is ready.
	assert.Positive(t, cap(c.Signals()),
		"the signal channel must be buffered so no signal is dropped")
}

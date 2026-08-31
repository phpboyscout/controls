package controls_test

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.com/phpboyscout/go/controls"
)

// 0004 OQ1: a stop can report that it failed to release.
//
// Today StopFunc returns nothing, so a Controller stopping a service has no way
// to learn that resources are still held. The result is recorded rather than
// acted on: knowing is the prerequisite, and gating a restart on it is a
// behaviour change for every consumer and its own decision.
func TestWithStopErrRecordsTheResult(t *testing.T) {
	t.Parallel()

	boom := errors.New("could not release the listener")

	c := controls.NewController(context.Background(),
		controls.WithLogger(slog.New(slog.DiscardHandler)))

	c.Register("thing",
		controls.WithStart(func(context.Context) error { return nil }),
		controls.WithStopErr(func(context.Context) error { return boom }),
	)

	c.Start()

	stopController(t, c)

	info := findInfo(t, c, "thing")

	if !errors.Is(info.StopErr, boom) {
		t.Errorf("StopErr is %v, want the stop's own error", info.StopErr)
	}
}

// A stop that releases everything records nil, so a caller can tell "released"
// from "did not say".
func TestWithStopErrRecordsSuccessAsNil(t *testing.T) {
	t.Parallel()

	var ran atomic.Bool

	c := controls.NewController(context.Background(),
		controls.WithLogger(slog.New(slog.DiscardHandler)))

	c.Register("thing",
		controls.WithStart(func(context.Context) error { return nil }),
		controls.WithStopErr(func(context.Context) error { ran.Store(true); return nil }),
	)

	c.Start()

	stopController(t, c)

	if !ran.Load() {
		t.Fatal("the stop function never ran")
	}

	if info := findInfo(t, c, "thing"); info.StopErr != nil {
		t.Errorf("StopErr is %v, want nil for a stop that released everything", info.StopErr)
	}
}

// The whole claim of additive-ness rests on this: WithStop keeps working
// unchanged, so no consumer migrates. Asserted directly rather than assumed.
func TestWithStopStillWorksUnchanged(t *testing.T) {
	t.Parallel()

	var stopped atomic.Bool

	c := controls.NewController(context.Background(),
		controls.WithLogger(slog.New(slog.DiscardHandler)))

	c.Register("legacy",
		controls.WithStart(func(context.Context) error { return nil }),
		controls.WithStop(func(context.Context) { stopped.Store(true) }),
	)

	c.Start()

	stopController(t, c)

	if !stopped.Load() {
		t.Error("a WithStop function did not run")
	}

	if info := findInfo(t, c, "legacy"); info.StopErr != nil {
		t.Errorf("StopErr is %v; a StopFunc cannot fail and must record nil", info.StopErr)
	}
}

// A panicking stop is contained and reported rather than taking the process
// with it. The shutdown path already recovered; the health-restart path called
// Stop directly and did not, which is an inconsistency this closes.
func TestAPanickingStopIsRecordedNotFatal(t *testing.T) {
	t.Parallel()

	c := controls.NewController(context.Background(),
		controls.WithLogger(slog.New(slog.DiscardHandler)))

	c.Register("panics",
		controls.WithStart(func(context.Context) error { return nil }),
		controls.WithStopErr(func(context.Context) error { panic("a defect in stop") }),
	)

	c.Start()

	stopController(t, c)

	// Reaching here at all is half the assertion: an escaped panic would have
	// ended the process.
	if info := findInfo(t, c, "panics"); info.StopErr == nil {
		t.Error("a panicking stop recorded no error; it must be reported rather than swallowed")
	}
}

// stopController drives a controller through a clean shutdown and waits for it.
func stopController(t *testing.T, c *controls.Controller) {
	t.Helper()

	c.Stop()

	require.Eventually(t, func() bool {
		return c.GetState() == controls.Stopped
	}, 5*time.Second, time.Millisecond)

	c.Wait()
}

func findInfo(t *testing.T, c *controls.Controller, name string) controls.ServiceInfo {
	t.Helper()

	info, ok := c.GetServiceInfo(name)
	if !ok {
		t.Fatalf("no ServiceInfo for %q", name)
	}

	return info
}

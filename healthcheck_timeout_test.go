package controls_test

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/phpboyscout/go/controls"
)

// newDiscardController builds a controller with signals disabled and a discard
// logger, plus cleanup that stops and waits. release is closed before Stop so a
// deliberately context-ignoring check is unwedged first and Wait cannot hang.
func newDiscardController(t *testing.T, release chan struct{}) *controls.Controller {
	t.Helper()

	c := controls.NewController(context.Background(),
		controls.WithLogger(slog.New(slog.DiscardHandler)),
	)
	// Registered first -> runs last: stop/wait happens after release is closed.
	t.Cleanup(func() {
		c.Stop()
		c.Wait()
	})
	// Registered second -> runs first: unwedge any context-ignoring check before
	// the stop sequence so the ticker goroutine can drain.
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	return c
}

// TestSyncCheck_ContextIgnoringCheckTimesOut proves F1 on the sync path: a check
// that ignores its context must not hang the inline Readiness request. The
// Timeout must be enforced with a timeout CheckResult.
func TestSyncCheck_ContextIgnoringCheckTimesOut(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})

	c := newDiscardController(t, release)

	require.NoError(t, c.RegisterHealthCheck(controls.HealthCheck{
		Name: "wedged-sync",
		Check: func(_ context.Context) controls.CheckResult {
			<-release // ignores its context entirely

			return controls.CheckResult{Status: controls.CheckHealthy}
		},
		Timeout: 100 * time.Millisecond,
		Type:    controls.CheckTypeReadiness,
	}))

	c.Start()

	done := make(chan controls.HealthReport, 1)
	go func() { done <- c.Readiness() }()

	select {
	case report := <-done:
		found := false

		for _, s := range report.Services {
			if s.Name == "wedged-sync" {
				found = true

				assert.Equal(t, "ERROR", s.Status)
			}
		}

		assert.True(t, found, "wedged sync check must appear in the report")
		assert.False(t, report.OverallHealthy, "a timed-out check must fail readiness")
	case <-time.After(2 * time.Second):
		t.Fatal("Readiness hung on a context-ignoring sync check; Timeout not enforced")
	}
}

// TestAsyncCheck_WedgedRefreshFlipsReadiness proves F1 on the async path: once
// the ticker goroutine's refresh wedges on a context-ignoring check, readiness
// must not keep serving the last cached healthy result indefinitely.
func TestAsyncCheck_WedgedRefreshFlipsReadiness(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})

	var calls atomic.Int64

	c := newDiscardController(t, release)

	require.NoError(t, c.RegisterHealthCheck(controls.HealthCheck{
		Name: "wedged-async",
		Check: func(_ context.Context) controls.CheckResult {
			if calls.Add(1) == 1 {
				return controls.CheckResult{Status: controls.CheckHealthy}
			}

			<-release // second and later runs ignore their context and block

			return controls.CheckResult{Status: controls.CheckHealthy}
		},
		Interval: 50 * time.Millisecond,
		Timeout:  100 * time.Millisecond,
		Type:     controls.CheckTypeReadiness,
	}))

	c.Start()

	// First async run caches a healthy result.
	require.Eventually(t, func() bool {
		return calls.Load() >= 1
	}, 2*time.Second, 5*time.Millisecond)

	require.True(t, c.Readiness().OverallHealthy, "readiness healthy right after first async run")

	// The wedged refresh must flip readiness to not-ready — either because the
	// per-run Timeout records a timeout result, or because the cache goes stale.
	require.Eventually(t, func() bool {
		return !c.Readiness().OverallHealthy
	}, 3*time.Second, 20*time.Millisecond, "wedged async refresh must flip readiness to not-ready")
}

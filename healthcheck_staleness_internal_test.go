package controls

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// findStatus returns the ServiceStatus for name and whether it was present.
func findStatus(r HealthReport, name string) (ServiceStatus, bool) {
	for _, s := range r.Services {
		if s.Name == name {
			return s, true
		}
	}

	return ServiceStatus{}, false
}

// TestStaleAsyncCache_FailsReadinessAndSurfacesInStatus proves F1's staleness
// bound: an async cached result older than the staleness window must fail
// readiness (fail-closed) and be surfaced as an error in Status().
func TestStaleAsyncCache_FailsReadinessAndSurfacesInStatus(t *testing.T) {
	t.Parallel()

	c := NewController(context.Background())

	// A healthy result stamped well beyond 3x the interval ago.
	stale := &CheckResult{
		Status:    CheckHealthy,
		Timestamp: time.Now().Add(-1 * time.Second),
	}

	entry := &healthCheckEntry{
		check: HealthCheck{
			Name:     "db",
			Interval: 50 * time.Millisecond,
			Type:     CheckTypeBoth,
		},
	}
	entry.lastResult.Store(stale)
	c.healthChecks["db"] = entry

	readiness := c.Readiness()
	assert.False(t, readiness.OverallHealthy, "stale async cache must fail readiness")

	rs, ok := findStatus(readiness, "db")
	assert.True(t, ok)
	assert.Equal(t, "ERROR", rs.Status, "stale check must be reported ERROR in readiness")

	status := c.Status()

	ss, ok := findStatus(status, "db")
	assert.True(t, ok)
	assert.Equal(t, "ERROR", ss.Status, "staleness must be surfaced in Status() output")
}

// TestFreshAsyncCache_PassesReadiness is the staleness control: a recent cached
// result within the window is not treated as stale.
func TestFreshAsyncCache_PassesReadiness(t *testing.T) {
	t.Parallel()

	c := NewController(context.Background())

	fresh := &CheckResult{
		Status:    CheckHealthy,
		Timestamp: time.Now(),
	}

	entry := &healthCheckEntry{
		check: HealthCheck{
			Name:     "cache",
			Interval: 50 * time.Millisecond,
			Type:     CheckTypeReadiness,
		},
	}
	entry.lastResult.Store(fresh)
	c.healthChecks["cache"] = entry

	assert.True(t, c.Readiness().OverallHealthy, "fresh async cache must pass readiness")
}

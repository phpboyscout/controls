package controls

import (
	"context"
	"sync/atomic"
	"time"
)

const defaultCheckTimeout = 5 * time.Second

// stalenessIntervalMultiple bounds how old an async cached result may be before
// it is treated as stale: older than this multiple of the check's Interval and
// the cache is no longer trusted (D11).
const stalenessIntervalMultiple = 3

// healthCheckEntry wraps a HealthCheck with its cached result and cancellation.
type healthCheckEntry struct {
	check      HealthCheck
	lastResult atomic.Pointer[CheckResult]
	cancel     context.CancelFunc
}

// runCheck executes the health check with the configured timeout and stores the
// result. The check runs in its own goroutine and runCheck selects on the
// timeout context: a check that ignores its deadline can therefore never block
// the caller — the inline Status()/Readiness() request on the sync path, or the
// ticker goroutine on the async path. On expiry a timeout CheckResult is
// recorded and the abandoned check goroutine is left to finish on its own,
// matching the StopFunc-abandonment policy (D11). The done channel is buffered
// so that late send never blocks the abandoned goroutine.
func (e *healthCheckEntry) runCheck(parentCtx context.Context) {
	timeout := e.check.Timeout
	if timeout == 0 {
		timeout = defaultCheckTimeout
	}

	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	done := make(chan CheckResult, 1)

	go func() {
		done <- e.check.Check(ctx)
	}()

	var result CheckResult

	select {
	case result = <-done:
	case <-ctx.Done():
		result = CheckResult{
			Status:  CheckUnhealthy,
			Message: "health check timed out",
		}
	}

	result.Timestamp = time.Now()
	e.lastResult.Store(&result)
}

// stale reports whether an async cached result is older than
// stalenessIntervalMultiple x Interval. Sync checks (Interval == 0) run inline on
// every request and are never stale; a nil result is handled by the fail-closed
// path in toServiceStatus, not here.
func (e *healthCheckEntry) stale(r *CheckResult, now time.Time) bool {
	if e.check.Interval <= 0 || r == nil {
		return false
	}

	return now.Sub(r.Timestamp) > time.Duration(stalenessIntervalMultiple)*e.check.Interval
}

// result returns the latest check result. For sync checks, it runs the check
// inline. For async checks, it returns the cached result.
func (e *healthCheckEntry) result(parentCtx context.Context) *CheckResult {
	if e.check.Interval > 0 {
		return e.lastResult.Load()
	}

	// Sync check — run inline
	e.runCheck(parentCtx)

	return e.lastResult.Load()
}

// toServiceStatus converts a CheckResult to a ServiceStatus for inclusion in a
// HealthReport. A nil result means an async check has not yet produced a cached
// value. When failClosed is true (readiness gating) this is reported as not-ready;
// otherwise it defaults to OK.
func toServiceStatus(name string, r *CheckResult, failClosed bool) (ServiceStatus, bool) {
	if r == nil {
		if failClosed {
			return ServiceStatus{
				Name:   name,
				Status: "ERROR",
				Error:  "check has not completed its first run",
			}, false
		}

		return ServiceStatus{
			Name:   name,
			Status: "OK",
		}, true
	}

	s := ServiceStatus{Name: name}

	switch r.Status {
	case CheckHealthy:
		s.Status = "OK"
	case CheckDegraded:
		s.Status = "DEGRADED"
		s.Error = r.Message
	case CheckUnhealthy:
		s.Status = "ERROR"
		s.Error = r.Message
	}

	healthy := r.Status != CheckUnhealthy

	return s, healthy
}

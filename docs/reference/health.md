# Health checks and reports reference

Health reaches the reports from two places: probes attached to a service
(`WithStatus`, `WithLiveness`, `WithReadiness` — see
[Services](services.md#statusfunc-and-probefunc-func-error)) and standalone
`HealthCheck` values registered with `RegisterHealthCheck`. This page is the
field-by-field reference for the standalone checks and for the report values all
of them produce.

## HealthCheck fields

```go
type HealthCheck struct {
	Name     string
	Check    func(ctx context.Context) CheckResult
	Timeout  time.Duration
	Interval time.Duration
	Type     CheckType
}
```

| Field | Required | Zero value means | What it controls |
|---|---|---|---|
| `Name` | yes | — | The `name` in the report entry, and the key for `GetCheckResult`. Must be unique **among health checks**; `RegisterHealthCheck` returns `duplicate health check name: "<name>"` otherwise. |
| `Check` | yes | **crashes the process** — a nil `Check` is called and panics on its first run | The probe. The `ctx` it receives already carries `Timeout`. |
| `Timeout` | no | use the default (5s) | Bounds a single run of `Check`. |
| `Interval` | no | synchronous — run inline on every report that includes it | Non-zero makes the check asynchronous: a background goroutine polls it and reports read the cached result. |
| `Type` | no | `CheckTypeReadiness` | Which reports the check appears in. |

Registration rules, both enforced by the returned `error`:

- `cannot register health check after start` — checks must be registered while
  the controller is still in the `Unknown` state.
- `duplicate health check name: "<name>"` — only checked against other health
  checks. **A check may silently share a name with a registered service**, in
  which case the report contains two entries with the same `name` and consumers
  keying on it will collide.

## Sync and async checks

| | Sync (`Interval == 0`) | Async (`Interval > 0`) |
|---|---|---|
| When `Check` runs | inline, every time a report including it is built | immediately at `Start`, then every `Interval` |
| What reports read | the result of that run | the last cached result |
| Cost of a report | one live probe per request | none |
| `GetCheckResult` before any report | `false` | `true` once the first run has completed |
| Staleness | not applicable | a cached result older than **3 × `Interval`** is reported `ERROR` with `cached health result is stale` |

An async check's first run starts as soon as the controller starts — the window
in which it has no result is the duration of that first run, not a whole
interval. During that window `Readiness()` reports it as not-ready and
`Status()`/`Liveness()` treat it as OK.

Every run, sync or async, is executed in its own goroutine and raced against the
timeout. A `Check` that ignores its context cannot hang a report or wedge the
poller: at expiry the run records `CheckResult{Status: CheckUnhealthy, Message:
"health check timed out"}` and the abandoned goroutine is left to finish alone.

**A panic inside `Check` is not recovered and crashes the process.** Service
probes are wrapped; standalone checks are not. Recover inside your own `Check`
if the probe can panic.

## CheckResult

```go
type CheckResult struct {
	Status    CheckStatus
	Message   string
	Timestamp time.Time
}
```

| Field | Notes |
|---|---|
| `Status` | One of `CheckHealthy`, `CheckDegraded`, `CheckUnhealthy`. The zero value is `CheckHealthy`, so an empty `CheckResult{}` reports OK. |
| `Message` | Free text. Surfaces as the `error` field of the report entry for `DEGRADED` and `ERROR`; ignored for `OK`. |
| `Timestamp` | **Set by the controller, not by you.** Whatever your `Check` puts here is overwritten with the time the run completed — the value the staleness bound is measured against. |

## CheckStatus in the report

| `CheckStatus` | `ServiceStatus.Status` | `OverallHealthy` |
|---|---|---|
| `CheckHealthy` | `"OK"` | unchanged |
| `CheckDegraded` | `"DEGRADED"`, with `Message` in `error` | unchanged — degraded still passes every gate, including readiness |
| `CheckUnhealthy` | `"ERROR"`, with `Message` in `error` | `false` |

## CheckType routing

| `CheckType` | `Liveness()` | `Readiness()` | `Status()` |
|---|:---:|:---:|:---:|
| `CheckTypeReadiness` (zero value, so the default) | | ✓ | ✓ |
| `CheckTypeLiveness` | ✓ | | ✓ |
| `CheckTypeBoth` | ✓ | ✓ | ✓ |

## HealthReport and ServiceStatus

```go
type HealthReport struct {
	OverallHealthy bool            `json:"overall_healthy"`
	Services       []ServiceStatus `json:"services"`
}

type ServiceStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}
```

Marshalled, a report looks like this:

```json
{
  "overall_healthy": false,
  "services": [
    { "name": "http-api", "status": "OK" },
    { "name": "cache", "status": "DEGRADED", "error": "warming" },
    { "name": "database", "status": "ERROR", "error": "dial tcp: connection refused" }
  ]
}
```

- `status` is one of `"OK"`, `"DEGRADED"`, `"ERROR"`.
- `error` is omitted when empty, and carries the probe's error text or the check
  result's `Message`.
- Service entries come first, in registration order, followed by health-check
  entries in Go map order — **the order of check entries is not stable between
  calls.** Match on `name`, not on position.
- `overall_healthy` is `false` if any entry is `ERROR`, and unaffected by
  `DEGRADED`.

## What each report actually asserts

A report is the aggregate of the probes you supplied. It is **not** evidence that
a service's goroutine is running. A service with no probe always reports `"OK"`
— including one whose `StartFunc` has already returned an error, and one
registered after `Start` that was never started at all. If you need the report to
reflect that a service is alive, give it a probe that checks something real.

Calling a report method **after** shutdown has completed re-runs every
synchronous check against the cancelled controller context, so each one records
`"ERROR"` with `health check timed out`. Read the reports while the controller is
running.

## Related

- [Add health checks](../how-to/health-checks.md) — the task-oriented recipe,
  including wiring a report onto an HTTP endpoint.
- [Health, liveness & readiness](../explanation/health-model.md) — why readiness
  fails closed and liveness does not.
- [Defaults and timings](defaults.md) — check timeout, interval and staleness
  bound alongside every other default.

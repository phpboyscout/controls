# Add health checks

The controller aggregates health from two sources, **per-service probes**
attached at registration and **standalone health checks** registered
separately, into three reports: `Status()`, `Liveness()`, and `Readiness()`.
This guide shows how to attach both and how to expose the reports.

> **Transport-agnostic by design.** This module does not open a port or register
> HTTP or gRPC handlers. `Status()`, `Liveness()` and `Readiness()` return plain
> `HealthReport` values; you serialise them onto whatever endpoint your process
> already serves. See [Expose the reports](#expose-the-reports) below.

## Per-service probes

Attach probes to a service with `WithStatus`, `WithLiveness`, and
`WithReadiness`. Each is a `func() error`: return `nil` for healthy, non-nil
for unhealthy.

```go
c.Register("http-api",
	controls.WithStart(serveHTTP),
	controls.WithStop(stopHTTP),
	controls.WithLiveness(func() error {
		// Is the process alive and not deadlocked? (should it be restarted?)
		return nil
	}),
	controls.WithReadiness(func() error {
		// Can it accept traffic right now? (are dependencies reachable?)
		return db.Ping()
	}),
)
```

How each probe contributes:

| Option | Feeds `Status()` | Feeds `Liveness()` | Feeds `Readiness()` |
|---|---|---|---|
| `WithStatus` | yes | as a fallback when no `WithLiveness` | as a fallback when no `WithReadiness` |
| `WithLiveness` | no | yes | no |
| `WithReadiness` | no | no | yes |

`WithStatus` is the general-purpose health signal used for aggregation and for
the restart supervisor's health monitoring (see
[Configure restart policy](restart-policy.md)). For Kubernetes-style probes,
prefer the dedicated `WithLiveness` and `WithReadiness`; the liveness and
readiness reports fall back to `Status` only when the specific probe is absent.

> **Probes must not block.** Each probe runs inline when the report is built. A
> panic in a probe *on this path* is recovered and reported as an error rather
> than crashing the process, but a slow probe stalls the whole report. Keep them
> fast, and apply your own timeout inside the probe if it does I/O.
>
> That recovery is narrower than it looks: a panic inside a standalone
> `HealthCheck.Check`, or inside a `WithStatus` probe being polled by the restart
> supervisor, is **not** recovered and takes the process down. Recover inside
> your own function if the work can panic.

## Standalone health checks

For dependencies that are not tied to a service lifecycle (a database, a cache,
a third-party API), register a standalone `HealthCheck`. It must be registered
before `Start`, and its name must be unique among health checks. Uniqueness is
*not* checked against service names: a check called `database` alongside a
service called `database` is accepted, and the report then carries two entries
with the same `name`. Pick names that do not collide.

```go
err := c.RegisterHealthCheck(controls.HealthCheck{
	Name:    "database",
	Type:    controls.CheckTypeReadiness,
	Timeout: 2 * time.Second,
	Check: func(ctx context.Context) controls.CheckResult {
		if err := db.PingContext(ctx); err != nil {
			return controls.CheckResult{Status: controls.CheckUnhealthy, Message: err.Error()}
		}
		return controls.CheckResult{Status: controls.CheckHealthy}
	},
})
if err != nil {
	log.Fatal(err) // duplicate name, or registered after Start
}
```

A check returns a three-state `CheckResult`:

| `CheckStatus` | Reported as | `OverallHealthy` |
|---|---|---|
| `CheckHealthy` | `"OK"` | `true` |
| `CheckDegraded` | `"DEGRADED"` (with `Message`) | `true` |
| `CheckUnhealthy` | `"ERROR"` (with `Message`) | `false` |

`Type` controls which reports the check appears in:

| `CheckType` | Appears in |
|---|---|
| `CheckTypeReadiness` (default) | `Readiness()` and `Status()` |
| `CheckTypeLiveness` | `Liveness()` and `Status()` |
| `CheckTypeBoth` | all three |

### Sync vs. async checks

The `Interval` field decides *when* the `Check` function runs:

- **Sync (`Interval == 0`, the default).** The check runs **inline** every time a
  report that includes it is built. Simple, always current, but it costs a real
  probe on every health request, which is fine for cheap, local checks.
- **Async (`Interval > 0`).** A background goroutine runs the check on that
  interval and **caches** the latest `CheckResult`; reports read the cached
  value. Use this for expensive or rate-limited dependencies so a burst of health
  requests cannot hammer them.

```go
_ = c.RegisterHealthCheck(controls.HealthCheck{
	Name:     "billing-api",
	Type:     controls.CheckTypeReadiness,
	Interval: 15 * time.Second, // async: polled in the background
	Timeout:  3 * time.Second,  // bounds each individual run
	Check:    probeBillingAPI,
})
```

`Timeout` bounds a single run of `Check` (default 5s); the `ctx` passed to your
function already has it applied.

> **Readiness fails closed before the first async run.** An async check has no
> cached result until its first run finishes; that run starts as soon as the
> controller does, so the window is usually brief. For **readiness**, a traffic
> gate, a check with no result yet is reported as not-ready (`"ERROR"`,
> `OverallHealthy: false`) rather than defaulting to OK. This closes the startup
> window where traffic could be admitted before the check has actually run. The
> same uninitialised result counts as OK for `Status()` and `Liveness()`, which
> are not traffic gates. See [Health, liveness and readiness](../explanation/health-model.md).

### Read a cached check result

`GetCheckResult` returns the latest result for a named check. The `false` return
means the check is unknown, or it has not produced a result yet: for an async
check, before its first run finishes; for a **sync** check, until a report that
includes it has been built, since a sync check only runs as part of a report.

```go
if r, ok := c.GetCheckResult("billing-api"); ok {
	fmt.Printf("billing: %v @ %s\n", r.Status, r.Timestamp.Format(time.RFC3339))
}
```

## Expose the reports

Each report method returns a JSON-tagged `HealthReport`. Wiring it onto an HTTP
endpoint is a handful of lines:

```go
func readyHandler(c *controls.Controller) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		report := c.Readiness()

		code := http.StatusOK
		if !report.OverallHealthy {
			code = http.StatusServiceUnavailable
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(report)
	}
}
```

Use `c.Liveness()` for a `/livez`-style probe and `c.Status()` for a full
diagnostic dump. The same pattern applies to a gRPC health service or any other
transport. The module only produces the report; you choose how to surface it.

## Related

- [Health, liveness and readiness](../explanation/health-model.md): the model
  behind the three reports and the fail-closed rule.
- [Health checks and reports reference](../reference/health.md): every field and
  default, the report JSON shape, and what happens to each value when it is wrong.
- [Configure restart policy](restart-policy.md): how `WithStatus` drives
  health-based restarts.

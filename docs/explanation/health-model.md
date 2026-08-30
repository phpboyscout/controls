# Health, liveness & readiness

The controller exposes three aggregate reports — `Status()`, `Liveness()`, and
`Readiness()`. They look similar but answer different questions, and the
difference matters when you wire them to an orchestrator. This page explains the
model.

## Two questions, not one

Liveness and readiness are distinct probes that a naive "is it healthy?" check
conflates:

- **Liveness** asks *"is this process still alive and making progress?"* A
  failing liveness probe means the process is wedged and should be **restarted**.
- **Readiness** asks *"can this process accept traffic right now?"* A failing
  readiness probe means the process is up but temporarily unable to serve — a
  dependency is down, a cache is warming — and should be **taken out of the load
  balancer**, but *not* restarted.

Restarting a process that is merely *not ready* (say, waiting on a slow database)
throws away warm state and can cascade an outage. Keeping traffic on a process
that is *not live* serves errors. Separating the two lets an orchestrator do the
right thing for each.

`Status()` is the third, broader view: a full diagnostic report that includes
every service and every check regardless of type. It is the natural body for a
human-facing `/status` dump.

## How the reports aggregate

Each report walks two sources and merges them into one `HealthReport`
(`OverallHealthy bool`, the controller's lifecycle `State`, and a
`ServiceStatus` per entry):

1. **Per-service probes.** For each registered service, the report calls the
   relevant probe:
    - `Liveness()` calls `WithLiveness`, falling back to `WithStatus` if no
      liveness probe was set.
    - `Readiness()` calls `WithReadiness`, falling back to `WithStatus`.
    - `Status()` calls `WithStatus` only.
2. **Standalone checks** whose `CheckType` matches the report (see below).

Any probe or check that reports unhealthy flips `OverallHealthy` to `false`.

A panic in a **service probe** — `WithStatus`, `WithLiveness`, `WithReadiness` —
called while a report is built is recovered and surfaces as a
`probe panicked: <value>` error entry rather than crashing the report. That
recovery does not extend to a standalone `HealthCheck.Check`, nor to a
`WithStatus` probe polled by the restart supervisor; a panic in either of those
crashes the process. See
[What controls does not do](limitations.md#it-does-not-recover-panics-in-your-start-callbacks-or-standalone-checks).

## Check types decide where a check appears

A standalone `HealthCheck` carries a `CheckType` that routes it to the right
report:

| `CheckType` | `Liveness()` | `Readiness()` | `Status()` |
|---|:---:|:---:|:---:|
| `CheckTypeReadiness` (default) | | ✓ | ✓ |
| `CheckTypeLiveness` | ✓ | | ✓ |
| `CheckTypeBoth` | ✓ | ✓ | ✓ |

`Status()` always includes every check — it is not a gate, just a view.

## Sync vs. async checks

A check's `Interval` decides how it is evaluated:

- **Sync (`Interval == 0`).** Run inline each time a report including it is built.
  The result is always current, at the cost of a live probe per request.
- **Async (`Interval > 0`).** A background goroutine polls the check on the
  interval and caches the latest `CheckResult`; reports read the cache. This
  decouples an expensive dependency probe from health-request volume.

Both paths bound a single run with the check's `Timeout` (default 5s). The
timeout is **enforced even if the check ignores its context**: the run executes
in its own goroutine and is raced against the deadline, so a check that never
returns cannot hang the inline sync request or wedge the async ticker. On expiry
the run records a timeout `CheckResult` (`"ERROR"`) and the abandoned goroutine
is left to finish on its own.

For async checks there is also a **staleness bound**: if the cached result is
older than three times the check's `Interval`, the refresh loop is assumed to
have stalled and the cache is no longer trusted. A stale entry is reported as
`"ERROR"` in every report — it fails readiness closed and is surfaced in
`Status()` — rather than serving a last-known-healthy result indefinitely.

## Readiness is true only while the controller is Running

The first rule is not about probes at all. `Readiness()` reports
`OverallHealthy: false` in every state except `Running`, whatever the probes say.
`Liveness()` and `Status()` are not gated this way.

The case it exists for is shutdown. Services stop in reverse registration order,
so a transport server registered early is stopped **last**, and without this rule
it keeps answering HTTP 200 for the whole drain while every service it depends on
is torn down beneath it. With the default 5s shutdown timeout that is up to five
seconds of traffic arriving at a listener whose dependencies are already gone.

Liveness is deliberately left alone. A liveness probe failing during a graceful
shutdown invites the orchestrator to kill a process that is shutting down
correctly, destroying the in-flight requests the drain existed to protect.

The same rule covers `NeverStarted` (nothing is serving yet) and `UnableToStart`
(a registered service has proven it will never start, so the process cannot do
its job). In the latter case nothing is stopped and the error still reaches the
error channel: the controller reports unready and leaves the decision to whoever
is watching. See wiki spec
[0003](https://gitlab.com/phpboyscout/go/controls/-/wikis/specs/0003-the-lifecycle-state-should-reach-the-health-reports).

## Why readiness also fails closed on an unrun check

An async check has **no cached result** until its first run completes. That run
begins as soon as the controller starts — the poller does not wait out an
interval first — so the window is short, but it is real, and a slow first probe
widens it. What should a report say about a check that has not produced a result
yet?

The answer depends on the report's purpose:

- For **readiness** — a traffic gate — an unknown result is treated as
  **not-ready** (`"ERROR"`, `OverallHealthy: false`). This *fails closed*: until
  the check has actually run and proven the dependency healthy, the process is
  kept out of the load balancer. Without this rule there would be a brief startup
  window where traffic is admitted on the *assumption* of health.
- For **liveness** and **status** — which are not traffic gates — the same
  unknown result is treated as OK. Reporting "not alive" for a check that simply
  has not run yet would invite a spurious restart.

This asymmetry is deliberate: readiness is conservative because the cost of a
false "ready" is served errors, while liveness is lenient because the cost of a
false "not alive" is a needless restart. The mechanics of this rule in the
supervisor are covered in
[Concurrency & shutdown correctness](concurrency.md).

## The three-state result

Standalone checks return a `CheckResult` with one of three statuses, which maps
into the report as:

| `CheckStatus` | `ServiceStatus.Status` | Contributes unhealthy? |
|---|---|---|
| `CheckHealthy` | `"OK"` | no |
| `CheckDegraded` | `"DEGRADED"` | no — still serving, with a message |
| `CheckUnhealthy` | `"ERROR"` | yes |

`CheckDegraded` lets a check flag "needs attention" (a near-full disk, a slow
upstream) without failing the gate — it carries its `Message` through but leaves
`OverallHealthy` true.

## What a report does not tell you

A report is the aggregate of the probes you supplied, and nothing more. The
controller does not inspect whether a service's goroutine is still running, so a
service with no probe always reports `"OK"` — even after its `StartFunc` has
returned an error, and even if it was registered after `Start` and therefore
never ran. If the entry needs to mean "this service is working", give the service
a probe that checks something real.

## Related

- [Add health checks](../how-to/health-checks.md) — the practical recipe.
- [Health checks and reports reference](../reference/health.md) — every field,
  default and report value.
- [The restart supervisor](restart-supervisor.md) — how `WithStatus` health drives
  restarts.

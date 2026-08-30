# Services and restart policy reference

A service is a name plus a set of callbacks, registered with `Register` and
described with `ServiceOption`s. This page lists the options, the contract each
callback must honour, and every `RestartPolicy` field with its default.

## Register

```go
func (c *Controller) Register(id string, opts ...ServiceOption)
```

`Register` has no error return. Three consequences are worth knowing before you
call it:

- **Names are not checked for uniqueness.** Registering two services under the
  same name is accepted. Both are started and stopped, both appear in every
  health report as separate entries with the same `name`, and
  `GetServiceInfo(name)` returns only the most recently registered one's info.
- **Registration after `Start` is not supervised.** The service is appended and
  logged as `WARN "Register called after Start; service will not be supervised"`,
  but its `StartFunc` is never called, its `StopFunc` never runs, and no restart
  policy applies. It still appears in `Status()`, `Liveness()` and `Readiness()`
  — as `"OK"` if it has no probe. Treat that warning as a bug in your wiring.
- **Both lifecycle callbacks are optional.** A service with no `WithStart` gets a
  no-op that returns `nil`; one with no `WithStop` gets a no-op stop. Neither can
  be nil at run time, so neither can panic on a nil call.

## Service options

| Option | Type it takes | Omitted | Notes |
|---|---|---|---|
| `WithStart` | `func(context.Context) error` | no-op returning `nil` | Called once per run by the supervisor. The context is the controller's, cancelled at shutdown with cause `ErrShutdown`. |
| `WithStop` | `func(context.Context)` | no-op | Called during shutdown in reverse registration order, and before each health-triggered restart. The context carries the shutdown deadline. |
| `WithStatus` | `func() error` | none | General health signal: feeds `Status()`, is the fallback for `Liveness()`/`Readiness()`, and is the probe the restart supervisor polls. |
| `WithLiveness` | `func() error` | falls back to `WithStatus` | Feeds `Liveness()` only. |
| `WithReadiness` | `func() error` | falls back to `WithStatus` | Feeds `Readiness()` only. |
| `WithRestartPolicy` | `RestartPolicy` (by value) | no restarts at all | The policy is copied at registration; later mutation of your struct has no effect. |
| `WithRestartResetInterval` | `time.Duration` | `DefaultRestartResetInterval` (30s) | Sets `RestartPolicy.RestartResetInterval`, creating a zero-valued policy first if the service has none — so this option alone **enables restarts, with every other field at its default, including unlimited `MaxRestarts`**. Order relative to `WithRestartPolicy` does not matter. |

## What each callback must honour

### StartFunc — `func(ctx context.Context) error`

| Return | Classified as | Restarted? | Forwarded on the error channel? |
|---|---|---|---|
| `nil` | clean start — the service is serving in the background | never | no |
| any error while `ctx` is already cancelled, or `context.Canceled` | cancelled | never | no |
| an error matching the `WithValidError` predicate | cancelled | never | no |
| any other non-nil error | failure | yes, if a `RestartPolicy` is attached | yes |

**It must return after its context is cancelled.** A `StartFunc` that ignores
cancellation pins its supervisor goroutine; the shutdown sequence abandons it at
the deadline and names it in a `WARN`, but a bare `Wait()` then blocks forever.
Use `WaitContext` when a service wraps third-party code that may not honour
cancellation.

**A panic inside a `StartFunc` is not recovered and crashes the process.**
Unlike stop callbacks and health probes, start callbacks have no recovery
wrapper. Recover inside your own function if the work can panic.

### StopFunc — `func(ctx context.Context)`

- The context is derived from `context.Background()` with the shutdown timeout —
  not from the already-cancelled controller context — so `http.Server.Shutdown`
  and friends can still drain in-flight work.
- **It may be called more than once**, and must be idempotent. Shutdown calls it
  once; a health-threshold restart calls it again before every restart of that
  service.
- **It is not called between error-triggered restarts.** When a `StartFunc`
  returns a genuine error and the policy restarts the service, `StopFunc` does
  not run in between; only a health-threshold restart stops the service first.
- A panic inside a `StopFunc` **is** recovered, and the shutdown sequence
  continues with the next service.
- Ignoring the context does not hang shutdown: the callback is abandoned at the
  deadline and left to finish on its own. The work it was doing is not completed.

### StatusFunc and ProbeFunc — `func() error`

- Return `nil` for healthy, any error for unhealthy. The error text is copied
  into the report entry.
- Probes run **inline** while a report is built, so a slow probe delays the
  report and every caller waiting on it. Apply your own timeout inside a probe
  that does I/O.
- A panic in a probe called from `Status()`, `Liveness()` or `Readiness()` is
  recovered and reported as `probe panicked: <value>`.
- **A panic in a `WithStatus` probe polled by the restart supervisor is not
  recovered and crashes the process.** The recovery wrapper is applied on the
  report path only.

## RestartPolicy fields

```go
type RestartPolicy struct {
	MaxRestarts            int
	InitialBackoff         time.Duration
	MaxBackoff             time.Duration
	HealthFailureThreshold int
	HealthCheckInterval    time.Duration
	RestartResetInterval   time.Duration
}
```

| Field | Zero value means | Default applied | What it controls |
|---|---|---|---|
| `MaxRestarts` | unlimited restarts | — | The cap on **consecutive** failures. `MaxRestarts: 2` allows two restarts, so the service runs at most three times before the supervisor gives up. |
| `InitialBackoff` | use the default | 1s | The wait before the first restart. |
| `MaxBackoff` | use the default | 30s | The ceiling the wait doubles towards. The multiplier is fixed at 2.0 and there is no jitter. |
| `HealthFailureThreshold` | health monitoring off | — | Consecutive `WithStatus` failures that trigger a restart. Requires a `WithStatus` probe: with no probe, health monitoring stays off however high you set this. |
| `HealthCheckInterval` | use the default | 10s | How often the supervisor polls `WithStatus` while health monitoring is on. |
| `RestartResetInterval` | use the default | 30s (`DefaultRestartResetInterval`) | How long a run must last before the consecutive-failure counter **and** the backoff reset. |

Attaching a policy is what enables restarts at all. Without one, a `StartFunc`
error is recorded in `ServiceInfo`, forwarded on the error channel, and the
service is left stopped.

## What the supervisor does when restarts run out

When the consecutive-failure counter reaches `MaxRestarts`, the supervisor stops
supervising that service and records an error:

- from an error-driven restart loop, `max restarts exceeded: <last start error>`
  (the last error is wrapped, so `errors.Is` against it still matches);
- from a health-driven restart loop, `max restarts exceeded` on its own, because
  the health failure was recorded on `ServiceInfo.Error` rather than returned.

That error is stored on `ServiceInfo.Error` and forwarded on the error channel.
**The controller keeps running.** A service exhausting its restarts — or failing
with no policy at all — never shuts the process down; the controller stops only
on `Stop()`, a signal it owns, or completion of the parent context.

**What it does change is readiness, but only if the service never started.** A
service that has never started cleanly and has exhausted its policy will never
start, so the controller moves to `UnableToStart` and stops reporting ready. With
no policy at all that is the first genuine error, since there are no retries. A
service that *did* start cleanly and failed later is an ordinary failure however
many restarts it then used up, and so is one that fails once and recovers on a
restart: both conditions are required. Nothing is stopped either way. See
[the health model](../explanation/health-model.md).

## ServiceInfo

```go
type ServiceInfo struct {
	Name         string
	RestartCount int
	LastStarted  time.Time
	LastStopped  time.Time
	Error        error
}
```

`GetServiceInfo` returns a copy, so reading it never blocks the supervisor.

| Field | Meaning |
|---|---|
| `Name` | The id passed to `Register`. |
| `RestartCount` | **Consecutive** failures so far, not lifetime restarts. Reset to zero when a run lasts at least `RestartResetInterval`. |
| `LastStarted` | When the most recent run began. |
| `LastStopped` | When the most recent run returned. Set on every return, including a clean start that is still serving in the background — it is not evidence the service has stopped. |
| `Error` | The most recent classified error, or the `max restarts exceeded` wrapper once the supervisor gave up. `nil` after a clean run. |

## Ordering guarantees

- **Startup is concurrent.** One supervisor goroutine is launched per service in
  registration order; the controller does not wait for a `StartFunc` to return
  or a probe to pass before launching the next. There is no dependency ordering
  or readiness gate between services — gate inside your own `StartFunc` if you
  need one.
- **Shutdown is sequential and reversed.** Stop callbacks run one at a time in
  reverse registration order: register `database` then `http-api`, and `http-api`
  is stopped first.

## Related

- [Configure restart policy](../how-to/restart-policy.md) — the task-oriented recipe.
- [The restart supervisor](../explanation/restart-supervisor.md) — why a clean
  start is never restarted.
- [Defaults and timings](defaults.md) — the same defaults alongside every other one.

# Defaults and timings

Every default value in `controls`, what changes it, and what a zero value does.
Three of them are exported constants you can reference in your own code; the
rest are internal values applied when you leave a field at zero.

## Controller defaults

| Setting | Default | Changed by | A zero or missing value means |
|---|---|---|---|
| Shutdown timeout | `DefaultShutdownTimeout` = **5s** | `WithShutdownTimeout(d)` | Passing `0` is **not** "use the default": it is an already-expired budget, so stop callbacks are abandoned immediately. Omit the option to get 5s. |
| Logger | `slog.New(slog.DiscardHandler)`, silent | `WithLogger(l)` | Passing `nil` panics inside `NewController`. |
| OS signal handling | none | `WithSignals()` | Without it the signal channel stays `nil` and no `signal.Notify` registration is made. |
| Signal channel buffer | 1 slot | not configurable | — |
| Message and error channels | unbuffered | `SetMessageChannel` / `SetErrorsChannel`, before `Start` only | A replaced error channel is read only by you; the controller logs nothing from it. |
| Valid-error predicate | none: every non-nil `StartFunc` error is a failure | `WithValidError(fn)` | — |
| Lifecycle state at construction | `NeverStarted` | — | Registration is only honoured in this state. A `Controller` built without `NewController` holds the zero value, which `GetState` reports as `Unknown`. |

## Service defaults

| Setting | Default | Changed by |
|---|---|---|
| `StartFunc` | no-op returning `nil` (a clean start) | `WithStart` |
| `StopFunc` | no-op | `WithStop`, or `WithStopErr` for a stop that reports how it ended |
| Health probes | none, so the service always reports `"OK"` | `WithStatus` / `WithLiveness` / `WithReadiness` |
| Restart behaviour | **no restarts** | `WithRestartPolicy` (or `WithRestartResetInterval`, which creates a policy) |

## Restart policy defaults

Each field's default applies when you leave it at zero, so
`RestartPolicy{MaxRestarts: 5}` gets the 1s/30s/30s timings below.

| Field | Default when zero | Notes |
|---|---|---|
| `MaxRestarts` | unlimited | `N` allows `N` restarts, i.e. `N + 1` runs. |
| `InitialBackoff` | **1s** | First wait before a restart. |
| `MaxBackoff` | **30s** | Ceiling for the doubling wait. |
| Backoff multiplier | **2.0** | Fixed; not a field, and there is no jitter. |
| `HealthCheckInterval` | **10s** | Poll interval for `WithStatus` while health monitoring is on. |
| `HealthFailureThreshold` | **0, health monitoring off** | Also stays off with no `WithStatus` probe, whatever the value. |
| `RestartResetInterval` | `DefaultRestartResetInterval` = **30s** | A run lasting at least this long resets both the consecutive-failure counter and the backoff. |

Backoff sequence with the defaults, for a service that fails immediately every
time: 1s, 2s, 4s, 8s, 16s, 30s, 30s, … Because each run is far shorter than the
30s reset window, none of those failures ages out, so `MaxRestarts` is reached.

## Health-check defaults

| Setting | Default when zero | Notes |
|---|---|---|
| `HealthCheck.Timeout` | **5s** | Bounds one run of `Check`; on expiry the run records `"health check timed out"`, and a late answer is not accepted. A run whose caller context was already cancelled records `"health check cancelled: the controller is shutting down"` instead, without invoking `Check`. |
| `HealthCheck.Interval` | **0, synchronous** | Runs inline on every report that includes the check. |
| `HealthCheck.Type` | `CheckTypeReadiness` | Appears in `Readiness()` and `Status()`. |
| Async staleness bound | **3 × `Interval`** | Fixed; an older cached result is reported `"ERROR"` with `cached health result is stale`. |
| `CheckResult.Status` | `CheckHealthy` | An empty `CheckResult{}` reports `"OK"`. |

## Supervisor defaults

| Setting | Default | Changed by | A zero or missing value means |
|---|---|---|---|
| `Failures()` channel | not created | calling `Failures()` | A consumer that never calls it never has a queue filling behind it. Created on first call, bounded at `DefaultFailureBufferSize` = **16**, and never closed. |
| Failure callback queue | unbounded, ordered | `WithOnFailure(fn)` | Without the option no queue exists and no goroutine runs. |
| `Child.RestartPolicy` | `nil`, run once, outcome final | the field | `nil` is **never restart**. A non-nil policy with `MaxRestarts <= 0` is **unlimited**, the opposite reading. |
| `Child.ValidError` | `nil`, no error is exempt | the field | The child equivalent of a controller's `WithValidError`, per child rather than per supervisor. |
| `Child.Stop` | no-op | the field | Cancelling the context passed to `Start` is the primary mechanism; `Stop` is the extra one. |
| `Stop` / `Detach` budget | the context you pass | — | `context.Background()` is an unbounded budget. A child that ignores cancellation then holds the call open until it returns. |

## Generational defaults

The full contract is on the [Generational](generational.md) page.

| Setting | Default | Changed by | Notes |
|---|---|---|---|
| `ReleaseAttempt` | **250ms** | the field | The budget each call to `Release` gets; the disposer retries on that interval until `Release` returns `nil`. Zero or negative selects the default. |
| `Probe` | `nil`, always healthy while a generation is live | the field | `Healthy` still returns `ErrNoGeneration` with no live generation. |

## Which defaults are exported

| Constant | Value |
|---|---|
| `controls.DefaultShutdownTimeout` | `5 * time.Second` |
| `controls.DefaultRestartResetInterval` | `30 * time.Second` |
| `controls.DefaultFailureBufferSize` | `16` |

The 1s initial backoff, 30s max backoff, 10s health interval, 5s check timeout,
2.0 multiplier, 3× staleness multiple and 250ms release attempt are internal. You
cannot reference them by name, and they are not covered by the module's
compatibility promise the way an exported constant is. Set the corresponding
field explicitly if your process depends on a particular value.

## Related

- [Controller](controller.md): the options that change the controller defaults.
- [Services and restart policy](services.md): the fields that change the rest.

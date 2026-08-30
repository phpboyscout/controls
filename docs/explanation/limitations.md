# What controls does not do

`controls` supervises service lifecycles and nothing else. Everything below is
either deliberately absent or a known boundary of the design — worth reading
before you plan around a capability that is not there. Where there is a way to
get the effect anyway, it is named.

## It does not serve health endpoints

The module produces `HealthReport` values. It opens no port, registers no
HTTP or gRPC handler, and speaks no orchestrator's health protocol. Wiring a
report onto `/readyz`, a gRPC health service or anything else is your code — a
handful of lines, shown in [Add health
checks](../how-to/health-checks.md#expose-the-reports). This is what keeps the
dependency graph to one external module, and it is why the same supervisor works
under go-tool-base's transports and under a hand-rolled `net/http` server.

## It does not order or gate service startup

Services start **concurrently**. The controller launches one supervisor
goroutine per service in registration order and does not wait for a `StartFunc`
to return, or a readiness probe to pass, before launching the next. There is no
dependency declaration, no `DependsOn`, and no barrier.

Only shutdown is ordered: stop callbacks run one at a time in reverse
registration order. If service B must not begin work until service A is up, gate
it inside B's own `StartFunc` — for example by waiting on a channel A closes.

## It does not stop the process when a service dies

A `StartFunc` that returns an error, or a service that exhausts `MaxRestarts`,
does not shut the controller down. The error is recorded on `ServiceInfo`,
forwarded on the error channel and logged; the controller stays `Running` and
every other service carries on. The controller stops only on `Stop()`, on a
signal it owns via `WithSignals`, or when the parent context completes.

If a failed service should take the process with it, watch for it yourself —
consume the error channel with `SetErrorsChannel` before `Start`, or poll
`GetServiceInfo` — and call `Stop()`.

## A health report is not proof a service is running

Reports aggregate the probes you supplied. A service with no probe always
reports `"OK"` — including one whose `StartFunc` returned an error minutes ago,
and one registered after `Start` that never ran at all. `controls` does not
inspect goroutine liveness. Give a service a probe that checks something real if
you want its report entry to mean anything.

## It does not recover panics in your start callbacks or standalone checks

Panic recovery is deliberately partial, and the boundaries are not symmetrical:

| Callback | Panic behaviour |
|---|---|
| `WithStop` | recovered; shutdown continues with the next service |
| `WithStatus` / `WithLiveness` / `WithReadiness`, called while a report is built | recovered; reported as `probe panicked: <value>` |
| `WithStart` | **not recovered — the process crashes** |
| `Child.Start`, run by a `Supervisor` | recovered; counted, and fed to the restart policy |
| `WithStatus`, polled by the restart supervisor | **not recovered — the process crashes** |
| `HealthCheck.Check` | **not recovered — the process crashes** |

Recover inside your own function wherever the work can panic.

## It cannot be restarted, and it cannot be reconfigured while running

A controller is single-use. The lifecycle runs `Unknown → Running → Stopping →
Stopped` once; calling `Start()` on a `Stopped` controller logs a warning and
does nothing. To run services again, construct a new controller.

Nor can it be reconfigured mid-flight. Services and health checks must be
registered before `Start`; there is no deregistration for either. A
[`Supervisor`](../how-to/supervise-dynamic-children.md) is the answer when the
set has to change: its children can be detached at any time, and attached at any
time before shutdown begins (`Attach` returns `ErrSupervisorStopped` once `Stop`
has started). Detaching is the deregistration a `Controller` does not have. The `SetX`
setters exist for construction — they mutate fields the control goroutines read
without synchronisation, so calling one on a running controller is a race, not a
supported reconfiguration path.

## Configurations that are silently inert

Each of these compiles, runs and does nothing:

- **`HealthFailureThreshold` without a `WithStatus` probe.** Health-driven
  restarts need both; with no probe, monitoring never starts however high the
  threshold.
- **A `RestartPolicy` on a service whose `StartFunc` returns `nil` and which has
  no `WithStatus` probe.** A clean start is not an exit, so there is nothing to
  restart on; the service simply runs until shutdown.
- **`WithShutdownTimeout(0)`.** Zero is not "use the default" — it is an
  already-expired budget, so stop callbacks are abandoned as soon as they are
  launched and may never run.
- **Two services registered under the same name.** Both run; both appear in
  reports under that name; `GetServiceInfo` can only return one of them.
- **A health check sharing a name with a service.** `RegisterHealthCheck` only
  checks for collisions among health checks, so this is accepted and produces two
  report entries with the same `name`.

## It will not own OS signals unless you ask, and it cannot share them

Signal disposition is process-global, and `signal.Notify` is additive — every
registered channel gets a copy. So the controller registers nothing by default,
and `WithSignals` is for the case where the controller genuinely is the outermost
layer.

There is no supported configuration in which two things own the signal. Under a
CLI framework that already turns signals into context cancellation — go-tool-base
does — passing `WithSignals` gives you two shutdown drivers racing on one
`Ctrl-C`. Let the outer layer own it and cancel the context you passed to
`NewController`.

## You cannot tell from the context why the controller stopped

Since v0.2.0 the controller owns its own cancellation, so `context.Cause(ctx)` is
`ErrShutdown` for every stop it drives — a direct `Stop()`, a parent
cancellation, an expired parent deadline, or a signal. That is the point: the
guarantee is unconditional. The cost is that the cause no longer distinguishes
those triggers. A service that needs to know watches the parent context itself.

## Wait can still hang on a start callback that ignores cancellation

The shutdown sequence is bounded, and it abandons a stuck supervisor at the
deadline while naming the service in a `WARN`. The bare `Wait()` is not bounded:
it promises every supervisor goroutine has unwound, which a `StartFunc` that
never returns after cancellation makes impossible. Use `WaitContext(ctx)` when a
service wraps third-party code you cannot make cancellable; it returns
`ctx.Err()` and leaks the stuck goroutine rather than blocking forever.

## There is no restart curve to tune, and no jitter

Backoff doubles from `InitialBackoff` to `MaxBackoff`. The multiplier is fixed at
2.0, there is no jitter, and there is no way to substitute a strategy of your
own. A fleet of instances restarting against the same downed dependency will
retry in step.

## There are no metrics, traces or lifecycle events

No counters, no OpenTelemetry and no event stream of restarts. The observable
surface is the `*slog.Logger` you inject, the error channel, `GetState()`,
`GetServiceInfo`, and the health reports.

The one exception is a `Supervisor`, which fires `WithOnFailure` and sends on
`Failures()` when a child reaches a terminal failure. That is a single
transition, not an event stream: nothing fires on a start, a restart or a clean
stop. Keeping instrumentation out is what the dependency-footprint
guard test enforces — instrument in the layer above, from those signals.

## It reads no configuration and ships no mocks

There are no environment variables, config files or flags: every setting is a
functional option or a struct field, listed in the [reference
tier](../reference/index.md). And no mocks are generated or published — write a
stub, or generate one for the [interface](../reference/interfaces.md) your code
depends on.

## Related

- [Reference](../reference/index.md) — the options, fields and defaults that do exist.
- [Architecture & the lifecycle state machine](architecture.md) — the design these
  boundaries follow from.
- [Concurrency & shutdown correctness](concurrency.md) — the guarantees that are
  made, and what each one covers.

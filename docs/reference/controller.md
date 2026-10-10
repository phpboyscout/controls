# Controller reference

`Controller` is the supervisor: it owns the registered services, the control
goroutines, the lifecycle state and the shutdown sequence. This page lists its
constructor, its options and every method, with the behaviour of each when it is
called at the wrong time.

## NewController: what it sets up

```go
func NewController(ctx context.Context, opts ...ControllerOpt) *Controller
```

`NewController` allocates the controller and applies the options. It starts no
goroutine, installs no OS signal handler and touches no process-global state, so
constructing a controller you never start is harmless.

The `ctx` you pass is **watched, not inherited**. The controller derives its own
context with `context.WithCancelCause(context.WithoutCancel(ctx))` and watches
`ctx.Done()` separately: completing your context, by cancel *or* by deadline,
triggers the ordinary shutdown sequence, so every service still observes
`ErrShutdown` as its context cause. Values on your context are preserved and
visible to services. See [Cause
determinism](../explanation/concurrency.md#cause-determinism).

State immediately after `NewController`, before any option is applied:

| Field | Value |
|---|---|
| Lifecycle state | `NeverStarted` |
| Logger | `slog.New(slog.DiscardHandler)`, so nothing is logged |
| Shutdown timeout | `DefaultShutdownTimeout` (5s) |
| Message channel | unbuffered `chan Message` |
| Error channel | unbuffered `chan error` |
| Signal channel | `nil`: no OS signal handling |
| Valid-error predicate | none: every non-nil error from a `StartFunc` is a failure |
| Registered services / health checks | none |

## Controller options

Each option is a `ControllerOpt` passed to `NewController`. There is no way to
apply one after construction that is safe once the controller is running; see
[Setters](#setters-and-when-they-are-safe-to-call).

| Option | Signature | Default when omitted | What it does, and what a bad value does |
|---|---|---|---|
| `WithLogger` | `WithLogger(l *slog.Logger)` | discard handler | Stores `l.With("component", "controller")`. Passing `nil` **panics with a nil pointer dereference inside `NewController`**. Omit the option instead if you want no logging. |
| `WithShutdownTimeout` | `WithShutdownTimeout(d time.Duration)` | `DefaultShutdownTimeout` (5s) | Bounds the **whole** stop phase, not each callback. A zero or negative `d` is not "use the default": it produces an already-expired budget, so stop callbacks are abandoned the instant they are launched and may never run at all. |
| `WithSignals` | `WithSignals()` | no signal handling | Creates a signal channel with a one-slot buffer. `signal.Notify` for `SIGINT`/`SIGTERM` is registered in `Start`, not at construction, and detached again at shutdown. Do not use it under a CLI framework that already turns signals into cancellation. |
| `WithOnEvent` | `WithOnEvent(fn func(ServiceEvent))` | none | Delivers every terminal service failure, and the latest of each run of retries, to `fn` on a goroutine of its own, in order. A retry waiting behind a slow `fn` is replaced by a newer one. `fn` may call `Stop`. A panic in it is recovered and logged. See [Events](#service-events). |
| `WithValidError` | `WithValidError(fn func(error) bool)` | none | A `StartFunc` error for which `fn` returns true is classified as a normal end of run: not restarted, not counted, not forwarded on the error channel. Applies to every registered service; there is no per-service variant. (A `Supervisor`'s `Child` has its own `ValidError` field.) |

## Lifecycle methods

| Method | Blocks? | Effect | Called at the wrong time |
|---|---|---|---|
| `Start()` | no | `NeverStarted → Running`, then launches one supervisor goroutine per registered service, the async health-check goroutines, the event forwarders it needs, and the control goroutines (message processor, parent watch, and the signal handler if `WithSignals` was passed). | Any state other than `NeverStarted`: logs `WARN "Start called, but controller has already started; ignoring"` and returns. A controller is **single-use**; after `Stopped` it cannot be started again. |
| `Stop()` | no | `Running → Stopping`, **or** `UnableToStart → Stopping`, recording `CauseStop` and the shutdown deadline, then sends `Stop` on the message channel. Returns before the shutdown sequence has finished. Safe to call from a `WithOnEvent` callback. | Any other state: logs `WARN "Stop called, but not in expected state, unable to continue"` and returns. Before any `Start` that means it is a no-op and the controller stays startable. Safe to call concurrently or repeatedly. |
| `Wait()` | yes, unbounded | Blocks until the shutdown sequence has completed, every async health check has exited, and every service has either started cleanly or had its supervisor goroutine exit. The sequence includes delivering queued events, so a slow consumer delays it by up to the shutdown budget. | Before `Start`, the wait group is empty and it returns immediately. It never returns while a service whose `Start` failed has a retry that ignores cancellation; use `Done` or `WaitContext` for that case. |
| `WaitContext(ctx)` | yes, bounded | Returns `nil` when what `Wait` waits for is done, or `ctx.Err()` when `ctx` completes first. | On the abandoned path the stuck supervisor goroutines and one internal helper goroutine are deliberately leaked. |
| `Done()` | n/a | Returns a channel closed when the shutdown sequence has finished, whatever triggered it, within the shutdown timeout of the trigger. The same channel every call. | Before `Start` it stays open: nothing has stopped. |
| `Outcome()` | no | Returns `(Outcome, true)` once the controller's own shutdown sequence has written it, which happens as it reaches `Stopped`. A copy each call. | `(Outcome{}, false)` before that, and after a `SetState(Stopped)` from outside, which runs no shutdown. |

`Wait` and `WaitContext` are methods on the concrete `*Controller` only; they are
on no interface, including `Controllable`.

## Registration methods

| Method | Returns | Rules |
|---|---|---|
| `Register(id string, opts ...ServiceOption)` | nothing | Must be called before `Start`. Names are **not** checked for uniqueness. Called after `Start`: logs `WARN "Register called after Start; service will not be supervised"`, and the service is added to the collection but never started, stopped or supervised, while still appearing in every health report. See [Services](services.md#register). |
| `RegisterHealthCheck(check HealthCheck) error` | `error` | Must be called before `Start`. Returns `cannot register health check after start` in any other state, and `duplicate health check name: "<name>"` when a check of that name is already registered. Uniqueness is checked **only against other health checks**, never against service names. |

## Health and introspection methods

| Method | Returns | Notes |
|---|---|---|
| `Status()` | `HealthReport` | Every service (via its `WithStatus` probe) and every health check regardless of `CheckType`. Not a gate: an async check that has not run yet counts as OK. |
| `Liveness()` | `HealthReport` | Every service (`WithLiveness`, falling back to `WithStatus`) plus checks of type `CheckTypeLiveness` or `CheckTypeBoth`. |
| `Readiness()` | `HealthReport` | Every service (`WithReadiness`, falling back to `WithStatus`) plus checks of type `CheckTypeReadiness` or `CheckTypeBoth`. **Ready only while `Running`**: in every other state `OverallHealthy` is false regardless of what the probes say. Also fails closed on an async check with no result yet, reported `ERROR`. |
| `GetServiceInfo(name string)` | `(ServiceInfo, bool)` | `false` when no service of that name was registered. When two services share a name, only the most recently registered one's info is stored. |
| `GetCheckResult(name string)` | `(CheckResult, bool)` | `false` when the name is unknown **or** the check has not yet produced a result, which for a synchronous check means until a report including it has been built. |
| `GetState()` | `State` | One of `NeverStarted`, `Running`, `UnableToStart`, `Stopping`, `Stopped`, or `Unknown` for a `Controller` built without `NewController`. |
| `IsRunning()`, `IsStopping()`, `IsStopped()` | `bool` | Equality tests against `Running`, `Stopping`, `Stopped`. There is no predicate for `NeverStarted`, `UnableToStart` or `Unknown`; compare `GetState()` directly. |
| `GetContext()` | `context.Context` | The controller's own context, the one services receive. It is cancelled with cause `ErrShutdown` during shutdown. |
| `GetLogger()` | `*slog.Logger` | The configured logger, or the discard logger. |
| `WaitGroup()` | `*sync.WaitGroup` | The wait group `Wait` blocks on. On no interface. |

All three reports are safe to call concurrently while the controller runs, and
stay responsive during shutdown, because the stop sequence does not hold the
services mutex.

## Channels

| Method | Channel | Default |
|---|---|---|
| `Messages()` | `chan Message` | unbuffered |
| `Errors()` | `chan error` | unbuffered |
| `Signals()` | `chan os.Signal` | `nil` unless `WithSignals` was passed; buffered with one slot when it was |

`Stop` is the only `Message` the controller defines and the only one the message
processor acts on. Sending `controls.Stop` directly on `Messages()` drives the
same shutdown sequence as calling `Stop()`.

The controller never reads the error channel. It logs each failure at `ERROR`
where it happens, with `service_name` and `kind`, and delivers the error to the
channel through a forwarder that is the channel's only sender, so a slow reader
never blocks a service.

- **Calling `Errors()` subscribes.** Failures before your first call are not
  delivered. Call it before `Start`, or use `WithOnEvent`, if you must see a
  service fail at boot.
- **Read the controller's own channel until it closes**, which it does before
  `Done` closes. A reader that stops early holds `Done`, and `Wait`, back by up to
  the shutdown budget whenever errors are queued for it.
- **A channel you install with `SetErrorsChannel`** before `Start` is subscribed
  from `Start`. It is yours, so the controller never closes it, and sends nothing
  on it after `Done`. Read it until `Done`, then take whatever is still buffered.
- **Never send on `Errors()`.** Nothing receives, so the send blocks until the
  channel closes, and then panics.

The channel carries the same failures as `WithOnEvent`, as bare errors.

## Service events

```go
type ServiceEvent struct {
	Service  string
	Kind     EventKind
	Err      error
	Restarts int
}
```

| `Kind` | When | `Err` |
|---|---|---|
| `EventRetrying` | A run failed and the restart policy will run it again, including after a health-threshold breach. | The run's error, or `health check failed: …` for a breach. |
| `EventFailed` | Terminal. The service started cleanly at least once, then failed and exhausted its policy. | Satisfies `errors.Is(err, ErrRestartsExhausted)`; after health breaches it is the sentinel alone. |
| `EventUnableToStart` | Terminal. The service never started cleanly and will not be tried again, including a service with no restart policy failing once. | As above, or the `StartFunc`'s error for a service with no policy. |

`Restarts` is the consecutive-restart count, which resets after a healthy run;
for a terminal event it is the count the service gave up at. A failure noticed
after shutdown has cancelled the services' context is not reported. Events queued
when shutdown begins are delivered within the remaining budget, and the number
left undelivered at the deadline is logged.

A `WithOnEvent` callback can run after the controller reaches `Stopped`, and one
still running at the deadline can outlive `Done`, because a callback cannot be
interrupted. A callback that calls `Wait` or blocks on `Done` holds up its own
drain until the budget expires.

## The shutdown outcome

```go
type Outcome struct {
	Cause      StopCause
	Err        error     // context.Cause of the parent, for CauseParent
	Signal     os.Signal // for CauseSignal
	Service    string    // for CauseGaveUp and CauseCompleted
	Unfinished []Unfinished
}

type Unfinished struct {
	Service string
	Reason  UnfinishedReason
	Err     error // the stop's error, for StopFailed
}
```

| `StopCause` | Trigger |
|---|---|
| `CauseStop` | `Stop()`, or `Stop` sent on `Messages()` |
| `CauseSignal` | the first signal on a channel `WithSignals` owns |
| `CauseParent` | the parent context's cancellation or deadline |
| `CauseGaveUp`, `CauseCompleted` | reserved: nothing produces them yet |

Exactly one cause is recorded however many triggers race, and the shutdown
deadline is recorded with it, so the budget runs from the trigger.

| `UnfinishedReason` | Meaning |
|---|---|
| `StopAbandoned` | The stop had budget left when called and had not returned when it expired. `StopErr` holds `ErrStopAbandoned`, and a later return does not overwrite it. |
| `StopUnbudgeted` | The budget had already expired when this stop's turn came. It is still called, and not awaited. `StopErr` holds `ErrStopAbandoned`. |
| `StopFailed` | The stop returned an error, or panicked, inside the budget. |
| `SupervisorAbandoned` | The service's supervisor goroutine had not exited by the deadline. |

`Complete()` is true when `Unfinished` is empty.

## Setters, and when they are safe to call

`SetLogger`, `SetShutdownTimeout`, `SetErrorsChannel`, `SetMessageChannel`,
`SetSignalsChannel`, `SetWaitGroup` and `SetState` mutate fields that the
control goroutines read after `Start`. They carry no internal synchronisation.

**Call them only during construction, before `Start`**, which is what the
`WithX` options do internally. Calling one on a running controller races the
running goroutines and is a programming error, not a supported reconfiguration
path.

`SetState` is a different case, and the earlier version of this page had it
wrong. It is not there for fakes: `StateAccessor` is meant to be consumed **and
implemented** outside this module, and a consumer driving a controller it owns
needs to say what state it is in. What is true is that it bypasses the
compare-and-set transitions, so calling it on a controller you did not construct
races the control goroutines, and since readiness is gated on the state it can
take a healthy process out of rotation. See wiki spec
[0003](https://gitlab.com/phpboyscout/go/controls/-/wikis/specs/0003-the-lifecycle-state-should-reach-the-health-reports)
D7.

`SetSignalsChannel` detaches any previous `signal.Notify` registration before
storing the new channel, so swapping the channel cannot leave an orphaned
registration receiving signals nobody reads.

## Package-level values

| Symbol | Value | Meaning |
|---|---|---|
| `ErrShutdown` | `errors.NewSentinel("controls.shutdown", "controller shutdown")` | The cause attached to the controller context for every stop the controller drives. Test for it with `errors.Is(context.Cause(ctx), controls.ErrShutdown)`. |
| `ErrRestartsExhausted` | `errors.NewSentinel("controls.restarts_exhausted", "max restarts exceeded")` | Inside the error a service leaves on `ServiceInfo.Error`, `Errors()` and its terminal `ServiceEvent`, and a child on `Failure.Err`, when it has used up its restart policy. `errors.Is` matches it and the last error beside it. |
| `ErrStopAbandoned` | `errors.NewSentinel("controls.stop_abandoned", "stop not awaited within the shutdown budget")` | On `ServiceInfo.StopErr` for a stop abandoned at the deadline, or not awaited because the budget had gone. |
| `DefaultShutdownTimeout` | `5 * time.Second` | Applied when `WithShutdownTimeout` is not passed. |
| `DefaultRestartResetInterval` | `30 * time.Second` | Applied when `RestartPolicy.RestartResetInterval` is zero. |
| `Stop` | `Message("stop")` | The only control message. |
| `CauseStop`, `CauseSignal`, `CauseParent`, `CauseGaveUp`, `CauseCompleted` | `StopCause` values | What started a shutdown; the last two are reserved. |
| `StopAbandoned`, `StopUnbudgeted`, `StopFailed`, `SupervisorAbandoned` | `UnfinishedReason` values | Why a shutdown step did not finish. |
| `EventRetrying`, `EventFailed`, `EventUnableToStart` | `EventKind` values | The kinds of `ServiceEvent`. |
| `NeverStarted`, `Running`, `UnableToStart`, `Stopping`, `Stopped` | `State` values | The lifecycle states, in order. |
| `Unknown` | `State` value | Not part of that sequence: the state could not be determined. |

The `Supervisor` sentinels and `DefaultFailureBufferSize` are on the
[Supervisor](supervisor.md#sentinel-errors) page. `Generational`'s four
sentinels are on the [Generational](generational.md#sentinel-errors) page.

## Related

- [Services and restart policy](services.md): what `Register` accepts.
- [Defaults and timings](defaults.md): every default in one table.
- [Handle graceful shutdown and signals](../how-to/graceful-shutdown.md): the
  shutdown sequence in order, and the signal-ownership rule.

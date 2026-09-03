# Architecture and the lifecycle state machine

This page explains how `controls` is put together: the `Controller` at the
centre, the `Services` collection it supervises, the control goroutines that
carry messages, errors, and signals, and the lifecycle state machine that ties
them together.

## The controller and the services collection

A `Controller` owns two things worth naming:

- a **`Services` collection**: the ordered list of registered services, plus a
  `sync.Map` of per-service `ServiceInfo` (restart count, last start and stop,
  last error, how the last stop ended); and
- a set of **channels and a context** shared by the control goroutines: a
  message channel, an error channel, an OS-signal channel, a cancellable
  context, and a wait group.

`Register` appends to the services collection (defaulting a missing `Start`/`Stop`
to a no-op). `RegisterHealthCheck` adds to a separate map of standalone checks.
Both must happen before `Start`.

## The lifecycle state machine

The controller moves through these states, held under a mutex and moved by
compare-and-set transitions:

```mermaid
stateDiagram-v2
    [*] --> NeverStarted : NewController
    NeverStarted --> NeverStarted : Stop (no-op)
    NeverStarted --> Running : Start (CAS)
    Running --> UnableToStart : a required service will never start
    Running --> Stopping : Stop / signal / ctx cancel
    UnableToStart --> Stopping : Stop / signal / ctx cancel
    Stopping --> Stopped : all WithStop complete
    Stopped --> [*] : Wait returns
```

- **`NeverStarted`**: constructed but not started. The only state in which
  registration is honoured. A `Stop` here is a no-op, so the controller stays
  startable.
- **`Running`**: `Start` has launched the supervisor and control goroutines.
  The only state in which the controller reports **ready**.
- **`UnableToStart`**: a registered service has failed without ever starting
  cleanly and has exhausted its restart policy, so it will never start. Nothing
  is stopped and the error still reaches the error channel; what changes is that
  readiness goes false, so an orchestrator routes no traffic to a process that
  cannot do its job.
- **`Stopping`**: a shutdown has been initiated; stop callbacks are running.
- **`Stopped`**: the shutdown sequence has completed.

**`Unknown` is not part of that sequence.** It means the state could not be
determined, and `GetState` returns it for a `Controller` built without
`NewController`: `State` is a string type, so such a controller holds the empty
string, which would otherwise be undetectable.

The transitions are **compare-and-set** operations. `Start` only proceeds if it
can move `NeverStarted → Running`; a second `Start` finds the state is no longer
`NeverStarted` and returns. `Stop` proceeds from `Running` **or**
`UnableToStart`, because a controller that cannot serve is still a live process
whose working services need stopping. This is what makes `Start` and `Stop`
idempotent (see [Concurrency and shutdown correctness](concurrency.md)). The one
write that bypasses the compare-and-set is the public `SetState`, which exists
for a consumer driving a controller it owns; the
[controller reference](../reference/controller.md#setters-and-when-they-are-safe-to-call)
says when that is safe.

The state is decided by wiki spec
[0003](https://gitlab.com/phpboyscout/go/controls/-/wikis/specs/0003-the-lifecycle-state-should-reach-the-health-reports),
which also records why readiness is gated on it and liveness is not.

## Start, Stop, and Wait

The three lifecycle methods relate like this:

- **`Start()`** transitions to `Running`, sizes the wait group to *services +
  1*, launches one supervisor goroutine per service, starts any async health
  checks (each adding one more count), and launches the control goroutines. Then
  it returns immediately. It does **not** block.
- **`Stop()`** transitions to `Stopping` and sends a `Stop` message onto the
  message channel, where the message processor picks it up and runs the shutdown
  sequence. It too returns without waiting.
- **`Wait()`** blocks on the wait group. The group only reaches zero after the
  shutdown handler has finished the whole sequence. The "+1" lifecycle count is
  released last, so `Wait()` returning means shutdown is genuinely complete.

## The control goroutines

`Start` launches the long-lived control goroutines (via an internal `controls()`
step) alongside the per-service supervisors. There are three when signals are
enabled and two otherwise, because the signal handler is only launched if
`WithSignals` supplied a channel:

| Goroutine | Watches | Job |
|---|---|---|
| **Signal handler** | the OS-signal channel | first `SIGINT`/`SIGTERM` triggers `Stop`; a second forces the handler to exit. Only launched when `WithSignals` supplied a channel |
| **Error and context handler** | the error channel and the **parent** context's `Done()` | logs forwarded service errors; triggers `Stop` when the context you passed to `NewController` is cancelled or its deadline expires |
| **Message processor** | the message channel | runs the shutdown sequence when it receives `Stop`, the only control message the package defines |

Each service runs under its own **supervisor** goroutine, which invokes
`WithStart`, classifies the outcome, applies the restart policy, and forwards
genuine errors on the error channel (see
[The restart supervisor](restart-supervisor.md)).

All of these goroutines share one exit condition: a `shutdownComplete` channel
that the shutdown handler closes once the sequence finishes. Watching it lets
each goroutine terminate cleanly rather than leak or spin, which is the subject
of [Concurrency and shutdown correctness](concurrency.md).

## The shutdown sequence

When the message processor receives `Stop`, the shutdown handler:

1. confirms or forces the `Stopping` state;
2. detaches OS-signal handling;
3. cancels the controller context with cause `ErrShutdown`;
4. cancels every async health check's context;
5. calls `Services.stop`, which runs each `WithStop` in reverse order under the
   shutdown-timeout context, abandoning any that overrun the deadline;
6. waits for the supervisor goroutines to exit within what remains of the
   budget, naming in a `WARN` any whose `WithStart` never returned;
7. sets the state to `Stopped`, closes `shutdownComplete`, and releases the
   lifecycle wait-group count.

## Health reporting

`Status()`, `Liveness()`, and `Readiness()` are read-side methods, independent of
the control loop. Each walks the services collection (calling the relevant probe)
and the matching standalone checks, and returns an aggregate `HealthReport`. They
are safe to call concurrently while the controller runs. The model behind the
three is described in [Health, liveness and readiness](health-model.md).

## Related

- [Health, liveness and readiness](health-model.md)
- [The restart supervisor](restart-supervisor.md)
- [Concurrency and shutdown correctness](concurrency.md)
- [What controls does not do](limitations.md)
- [Controller reference](../reference/controller.md): every method and its
  behaviour in each state.

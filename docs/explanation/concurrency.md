# Concurrency and shutdown correctness

A lifecycle supervisor is only useful if it is correct under concurrency: it must
not double-start services, leak goroutines, busy-spin a CPU, or deadlock on
shutdown. This page describes the properties the controller guarantees and the
mechanisms that enforce them. They are exercised under `-race`, and several of
them have a regression test named for the defect that produced them.

## Idempotent Start and Stop

`Start` and `Stop` are driven by **compare-and-set** state transitions taken
under a mutex:

- `Start` only proceeds if it can move the state `NeverStarted → Running`. A
  second (or concurrent) `Start` observes a non-`NeverStarted` state and returns
  without launching anything, so services are never double-started and the wait
  group is never double-counted.
- `Stop` proceeds from `Running` **or** `UnableToStart`, in both cases by CAS to
  `Stopping`. Duplicate `Stop` calls, or a `Stop` racing a signal-driven
  shutdown, collapse into a single shutdown sequence. `UnableToStart` is in that
  set deliberately: a controller that cannot serve is still a live process whose
  working services need stopping.

This makes both methods safe to call from multiple goroutines and safe to call
more than once, which is a real concern when a signal, a parent-context cancel,
and an explicit `Stop` can all arrive at once.

## Goroutine termination: no leak, no busy-spin

Every long-lived goroutine the controller starts (the signal handler, the parent
watch, the message processor, and each service supervisor) shares a single exit
condition: a `shutdownComplete` channel that the shutdown handler closes once the
sequence finishes, and that `Done()` returns. Each goroutine `select`s on it and
returns when it closes. Nothing is left blocked on a channel that will never
receive.

The parent watch needs one extra piece of care. It watches the parent's
`Done()`, and a closed `Done()` channel is *permanently* ready, so a loop that
kept selecting on it would fire on every iteration and spin the CPU. The watch
therefore selects once, on the parent or on `shutdownComplete`, and returns
after either.

A spin is CPU, not a goroutine count, and the first test written for this could
not see one: a handler that spins and then exits at shutdown leaves the goroutine
count exactly where it would be without the spin. The guard that holds now
measures process CPU across a shutdown held open by a slow `WithStop`. With the
defect reinstated it reports a whole core; without it, near zero.

## Bounded shutdown: what the bound covers

`Wait` blocks on a wait group sized to *services + async health checks + 1*. The
extra "+1" is the controller's own lifecycle count, released **last**, only
after the shutdown handler has run every stop callback, set the `Stopped` state
and delivered the queued failure events (below). So `Wait` returning is a hard
guarantee that shutdown finished. A service's count is released when it first
starts cleanly or when its supervisor exits, whichever is first, so `Wait` also
waits on a service whose `Start` failed and whose retry hangs.

`Done()` is the bounded way to wait. It closes at the end of the same sequence,
which runs under **one deadline recorded when shutdown is triggered**: `Stop`, a
signal or the parent context, not the moment the message processor gets to it.
So `<-c.Done()` returns within the shutdown timeout of the trigger, and
`Outcome()` then says what triggered it and which steps did not finish.

That guarantee holds even if a `WithStop` misbehaves. Each stop runs in its own
goroutine and is awaited against the shutdown-timeout deadline; a stop that
ignores its context is **abandoned** when the deadline elapses and the sequence
moves on. The abandoned goroutine is left to finish on its own, but it can no
longer hold up shutdown. So with context-respecting `WithStart` callbacks,
`Wait` returns within roughly the shutdown timeout regardless of a stuck stop.

The bound covers the **shutdown sequence**, not a `WithStart` that never
returns. A start callback that ignores cancellation keeps its per-service wait
group count held forever, and the bare `Wait`, which promises to see every
service unwind, blocks with it. That case is covered by `WaitContext` and the
post-stop supervisor wait, described in D10 below.

## D10: bounding the wait against context-ignoring StartFuncs

A `WithStart` that ignores `ctx.Done()`, canonically a wrapper around a
third-party blocking `Run()` with no cancellation support whose `WithStop`
cannot unblock it, never returns. Its supervisor goroutine never exits and the
wait group never drains. The shutdown sequence itself completes (services
stopped or abandoned on deadline, state `Stopped`), and then a bare `Wait`
hangs forever on a controller that reports itself stopped.

The wait group cannot be force-drained on the stuck supervisor's behalf:
calling `wg.Done()` for it would race a late-returning `Start` into a
double-decrement panic. The controller instead applies the same
abandon-at-deadline policy the stop path already uses for context-ignoring
`WithStop` callbacks, at two points:

- **Inside shutdown**, after the stop callbacks have run, the shutdown handler
  waits for every supervisor to exit against the *remaining* shutdown-timeout
  budget, so one deadline covers the whole bounded-shutdown contract. Any
  supervisor still running at the deadline is abandoned, and its service is
  named in a `WARN` record, turning a silent hang into a diagnosable message.
- **At the caller**, `WaitContext(ctx)` selects between the wait-group drain
  and `ctx.Done()`: `nil` on a clean drain, `ctx.Err()` when the wait is
  abandoned. `Wait` keeps its unbounded behaviour for callers with
  context-respecting services who want the guaranteed-cleanup semantics.

On either abandon path the stuck supervisor (and the small helper goroutine
watching the undrainable wait group) are **deliberately leaked**, the identical,
documented trade already accepted for abandoned stop callbacks. The
goroutine-leak guard tests account for this: the stuck-StartFunc tests bound
their deliberate leak by releasing the blocked start at test cleanup, so no
other test's goroutine baseline is skewed.

## D8: health-check setup happens-before the control goroutines

Shutdown can be triggered the instant the controller starts running. A signal
or a parent cancel can land while services are still initialising, and that
shutdown path reads each async health check's `CancelFunc` to cancel it.

If the control goroutines (which can drive that shutdown) were launched *before*
the async health checks recorded their `CancelFunc`s, a shutdown landing
mid-startup would read a `CancelFunc` that another goroutine is still writing,
which is a data race. `Start` therefore wires up services and async health
checks **before** it launches the control goroutines. The write of each
`CancelFunc` happens-before any goroutine that might read it, closing the race
by construction.

## D9: a failure is never sent from a supervisor

A supervisor that sees a service fail logs it, at `ERROR` with the service and
the kind of failure, then appends it to a queue for each consumer: the
`WithOnEvent` callback, and the error channel once something has subscribed by
calling `Errors()` (or installed a channel with `SetErrorsChannel`). Appending
never blocks, so nothing a consumer does or fails to do can hold a supervisor
back. Each queue has one **forwarder**, a goroutine that is the only thing
delivering to that consumer. A retry still waiting in a queue is replaced by a
newer retry of the same service, so a queue holds at most two events per
service; a terminal failure is never replaced or dropped.

Nothing in the controller reads the error channel. Before v0.8 it did, to log
what arrived, and a consumer reading the same channel received only a share of
the errors (issue 19).

At shutdown, once every supervisor has exited or been abandoned, the queues
refuse anything further, and no new subscription is accepted. The forwarders
then deliver what is left **within the remaining budget**, and `Done` closes
after that. A consumer that stops taking events therefore holds `Done` back by
up to the remaining budget, but no longer. At the deadline, delivery is
abandoned, and the undelivered count is logged. The error channel's forwarder
selects on that abandonment in its send, and shutdown waits for it to exit
before closing `Done`, so nothing is sent on the channel after `Done`. The
controller's own channel is closed by its forwarder as it exits, or at the
cutoff if it has none, so a reader ranging over it always ends. A callback
cannot be interrupted: one in progress at the deadline may outlive `Done`.

`Stop()` sends a `Stop` control message to the message processor after winning
the `Running → Stopping` CAS. If a `Stop` sent directly on the message channel
reached the processor first, nothing receives that send. `Stop()` therefore
selects on the send, on `shutdownComplete`, **and on the services' context**,
which only the shutdown sequence cancels, so it returns as soon as that
shutdown is under way. That matters for a `WithOnEvent` callback calling
`Stop()`, which would otherwise wait on `Done` while `Done` waits on the
callback.

## D11: the health-check timeout is raced, and stale async caches fail closed

A `HealthCheck.Check` carries a `Timeout`, but a check that ignores its context
would defeat it: run inline from `Status()` or `Readiness()`, a context-ignoring
check hangs every health request; run from the async ticker goroutine, it wedges
the refresh so the cache never updates and readiness serves the last *healthy*
result forever, a dead dependency reported healthy.

Each run is therefore executed in its own goroutine and raced against the
timeout context: whichever of the check result and `ctx.Done()` arrives first
wins. On expiry the run records a timeout `CheckResult` (`"ERROR"`) and returns;
the abandoned check goroutine is **left to finish on its own**, the same
abandon-at-deadline trade the stop path (D10) and the supervisor wait accept.
The hand-off channel is buffered so the abandoned goroutine's late send never
blocks.

One refinement came later, from a false message. The context handed to a check
is the controller's own, and shutdown cancels it, so during a shutdown both arms
of that `select` were ready at once and Go chose between them at random: a check
that returned in microseconds was recorded as "health check timed out" on
roughly half of runs. A check now reports
`health check cancelled: the controller is shutting down` when its caller's
context is already gone, and is not invoked at all if that is true on entry.
Still unhealthy, because a check with no result cannot vouch for a dependency,
but the diagnosis names the real cause, and every path is deterministic.

As a second line of defence, an async cached result older than **three times**
the check's `Interval` is treated as **stale**: the refresh loop is assumed to
have stalled, so the cache can no longer be trusted. A stale entry is reported
`"ERROR"` in every aggregation, failing readiness closed *and* surfacing in
`Status()`, rather than serving a stale healthy value indefinitely.

## D12: the stop sequence does not hold the services mutex

Shutting services down runs each `WithStop` in reverse registration order and
awaits it against the shutdown deadline, potentially the whole shutdown timeout.
`status()`, `liveness()`, and `readiness()` all take the same `services` mutex,
so holding it across the stop sequence would block every health and readiness
probe until shutdown finished, exactly when a load balancer most needs a prompt
not-ready answer.

The stop sequence therefore **snapshots the service slice under the lock and
releases it** before running any `WithStop`. Registration is already impossible
once the controller is `Stopping`, so the snapshot cannot go stale, and the
health probes stay responsive throughout shutdown.

The reverse holds too. `status()`, `liveness()` and `readiness()` copy the slice
under the lock and call each probe after releasing it, so a probe that blocks
cannot hold the stop sequence before it reaches its deadline, which a load
balancer polling `/readyz` would otherwise make likely. Two reports built at
once may therefore call the same probe concurrently, so a probe must be safe to
call concurrently with itself.

## Who owns the signal handler

**The outermost layer does, and by default that is not the controller.**
`signal.Notify` is additive: every registered channel receives a copy of every
signal. A library that registers a handler has not chosen a helpful default. It
has quietly become a co-owner of process-global state, and two owners means two
shutdown drivers running concurrently on one `Ctrl-C`.

That is not hypothetical. When a CLI framework above the controller also
translated signals into context cancellation, both fired, and which cancellation
landed first decided whether `context.Cause` reported `ErrShutdown` or the
parent's cause. A documented guarantee, resolved by goroutine scheduling.

So the controller does not register by default. It observes the context it was
given, and `WithSignals` is available for the standalone case where the
controller genuinely is outermost.

## Cause determinism

The controller derives its context with
`context.WithCancelCause(context.WithoutCancel(parent))` and watches
`parent.Done()` separately. The parent's completion is a **trigger** for the
normal shutdown sequence, not the cancellation itself.

The alternative, deriving directly from the parent, cannot give a dependable
cause. Once the parent cancels, the child is already cancelled with the parent's
cause, and the controller's own `cancel(ErrShutdown)` is a no-op, because the
first cancellation wins. Severing is what makes `ErrShutdown` unconditional.

`parent.Done()` closes on deadline expiry as well as cancellation, so an expired
deadline routes through the same path and produces an orderly teardown bounded by
the shutdown timeout, rather than handing every `WithStop` a context that is
already dead.

## Signal registration hygiene

When signals *are* enabled, registration is handled so it can neither be orphaned
nor swallow signals:

- `signal.Notify` is deferred to `Start` (in `startSignalHandler`), where it is
  registered **only if a signal channel survives** and **paired with the reader
  goroutine launched immediately below it**. Registering at construction would
  leave a controller that is constructed but never started with a handler and no
  reader, silently swallowing `SIGINT`/`SIGTERM` so the process ignores Ctrl-C.
  Deferring registration to `Start` means an unstarted controller keeps the
  signals at their default disposition.
- Without `WithSignals` the channel is `nil`, so `startSignalHandler` returns
  before registering anything.
- The registration is detached with `signal.Stop` when the signal channel is
  swapped out and again at shutdown, so a late signal never lands on a channel no
  one is reading.

The regression test for the first point re-executes the test binary as a child
that constructs a controller with signals enabled and never starts it, then
sends it `SIGINT` and expects it to die. An earlier version of that test forgot
`WithSignals` in the child, so it passed with the defect reinstated. A guard
nobody has seen fail is indistinguishable from one that cannot, which is why
each of these mechanisms was checked by putting the bug back.

## Related

- [Architecture and the lifecycle state machine](architecture.md): the
  goroutines and states these properties operate on.
- [Handle graceful shutdown and signals](../how-to/graceful-shutdown.md): the
  user-facing side of bounded shutdown.
- [The restart supervisor](restart-supervisor.md): the error-channel contract
  D9 supports.

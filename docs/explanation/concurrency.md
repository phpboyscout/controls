# Concurrency & shutdown correctness

A lifecycle supervisor is only useful if it is correct under concurrency: it must
not double-start services, leak goroutines, busy-spin a CPU, or deadlock on
shutdown. This page describes the properties the controller guarantees and the
mechanisms that enforce them. They are exercised under `-race` and are the reason
the module can be trusted as the backbone of a long-running process.

## Idempotent Start and Stop

`Start` and `Stop` are driven by **compare-and-set** state transitions taken
under a mutex:

- `Start` only proceeds if it can move the state `Unknown → Running`. A second
  (or concurrent) `Start` observes a non-`Unknown` state and returns without
  launching anything — so services are never double-started and the wait group
  is never double-counted.
- `Stop` only proceeds on `Running → Stopping`. Duplicate `Stop` calls, or a
  `Stop` racing a signal-driven shutdown, collapse into a single shutdown
  sequence.

This makes both methods safe to call from multiple goroutines and safe to call
more than once — a real concern when a signal, a parent-context cancel, and an
explicit `Stop` can all arrive at once.

## Goroutine termination — no leak, no busy-spin

Every long-lived goroutine the controller starts — the signal handler, the error
and context handler, the message processor, and each service supervisor — shares
a single exit condition: a `shutdownComplete` channel that the shutdown handler
closes once the sequence finishes. Each goroutine `select`s on it and returns
when it closes. Nothing is left blocked on a channel that will never receive.

The error-and-context handler needs one extra piece of care. It watches
`ctx.Done()`, but a closed `Done()` channel is *permanently* ready — a `select`
that keeps a `case <-ctx.Done()` would fire on every iteration and spin the CPU.
The handler defuses this by setting its local copy of the done channel to `nil`
after the first receipt, which disables that `select` case for good. The
goroutine then idles until `shutdownComplete` closes, draining any buffered
errors before it exits.

## Bounded shutdown — what the bound covers

`Wait` blocks on a wait group sized to *services + 1*. The extra "+1" is the
controller's own lifecycle count, released **last** — only after the shutdown
handler has run every stop callback and set the `Stopped` state. So `Wait`
returning is a hard guarantee that shutdown finished.

That guarantee holds even if a `WithStop` misbehaves. Each stop runs in its own
goroutine and is awaited against the shutdown-timeout deadline; a stop that
ignores its context is **abandoned** when the deadline elapses and the sequence
moves on. The abandoned goroutine is left to finish on its own, but it can no
longer hold up shutdown — so with context-respecting `WithStart` callbacks,
`Wait` returns within roughly the shutdown timeout regardless of a stuck stop.

The bound covers the **shutdown sequence**, not a `WithStart` that never
returns. A start callback that ignores cancellation keeps its per-service wait
group count held forever, and the bare `Wait` — which promises to see every
service unwind — blocks with it. That case is covered by `WaitContext` and the
post-stop supervisor wait, described in D10 below.

## D10 — bounding the wait against context-ignoring StartFuncs

A `WithStart` that ignores `ctx.Done()` — canonically a wrapper around a
third-party blocking `Run()` with no cancellation support, whose `WithStop`
cannot unblock it — never returns, so its supervisor goroutine never exits and
the wait group never drains. The shutdown sequence itself completes (services
stopped or abandoned on deadline, state `Stopped`), and then a bare `Wait`
hangs forever on a controller that reports itself stopped.

The wait group cannot be force-drained on the stuck supervisor's behalf:
calling `wg.Done()` for it would race a late-returning `Start` into a
double-decrement panic. The controller instead applies the same
abandon-at-deadline policy the stop path already uses for context-ignoring
`WithStop` callbacks, at two points:

- **Inside shutdown**, after the stop callbacks have run, the shutdown handler
  waits for every supervisor to exit against the *remaining* shutdown-timeout
  budget — one deadline covers the whole bounded-shutdown contract. Any
  supervisor still running at the deadline is abandoned, and its service is
  named in a WARN record — turning a silent hang into a diagnosable message.
- **At the caller**, `WaitContext(ctx)` selects between the wait-group drain
  and `ctx.Done()`: `nil` on a clean drain, `ctx.Err()` when the wait is
  abandoned. `Wait` keeps its unbounded behaviour for callers with
  context-respecting services who want the guaranteed-cleanup semantics.

On either abandon path the stuck supervisor (and the small helper goroutine
watching the undrainable wait group) are **deliberately leaked** — the
identical, documented tradeoff already accepted for abandoned stop callbacks.
The goroutine-leak guard tests account for this: the stuck-StartFunc tests
bound their deliberate leak by releasing the blocked start at test cleanup, so
no other test's goroutine baseline is skewed.

## D8 — startup ordering: health-check setup happens-before the control goroutines

Shutdown can be triggered the instant the controller starts running — a signal
or a parent cancel can land while services are still initialising. That shutdown
path reads each async health check's `CancelFunc` in order to cancel it.

If the control goroutines (which can drive that shutdown) were launched *before*
the async health checks recorded their `CancelFunc`s, a shutdown landing
mid-startup would read a `CancelFunc` that another goroutine is still writing —
a data race. `Start` therefore wires up services and async health checks
**before** it launches the control goroutines. The write of each `CancelFunc`
happens-before any goroutine that might read it, closing the race by
construction.

## D9 — error forwards are select-guarded on shutdown completion

A service supervisor forwards genuine errors on the error channel, whose only
receiver is the error-and-context handler. But that handler exits when
`shutdownComplete` closes. If a supervisor tried to forward an error *after* the
handler had gone, an unguarded send on an unbuffered channel would block the
supervisor forever.

Every forward is therefore a two-way `select`: send on the error channel, **or**
observe `shutdownComplete`. Once shutdown has completed there is no receiver, so
the `shutdownComplete` case wins and the send is abandoned. This makes every
error forward provably non-blocking, so a late error can never wedge a supervisor
goroutine during teardown.

`Stop()` is a second sender covered by the same guard. After winning the
`Running → Stopping` CAS it sends a `Stop` control message to the message
processor. But if the caller is descheduled after the CAS while a direct-channel
`Stop` drives the whole shutdown, the processor exits before the send lands —
and an unguarded send on the unbuffered message channel would block forever.
`Stop()` therefore selects between the message send and `shutdownComplete`, so a
`Stop` racing a completing shutdown returns promptly instead of hanging.

## D11 — health-check timeout is raced, and stale async caches fail closed

A `HealthCheck.Check` carries a `Timeout`, but a check that ignores its context
would defeat it: run inline from `Status()`/`Readiness()`, a context-ignoring
check hangs every health request; run from the async ticker goroutine, it wedges
the refresh so the cache never updates and readiness serves the last *healthy*
result forever — a dead dependency reported healthy.

Each run is therefore executed in its own goroutine and raced against the
timeout context: whichever of the check result and `ctx.Done()` arrives first
wins. On expiry the run records a timeout `CheckResult` (`"ERROR"`) and returns;
the abandoned check goroutine is **left to finish on its own**, the same
abandon-at-deadline tradeoff the stop path (D10) and the supervisor wait accept.
The hand-off channel is buffered so the abandoned goroutine's late send never
blocks.

As a second line of defence, an async cached result older than **three times**
the check's `Interval` is treated as **stale**: the refresh loop is assumed to
have stalled, so the cache can no longer be trusted. A stale entry is reported
`"ERROR"` in every aggregation — it fails readiness closed *and* is surfaced in
`Status()` — rather than serving a stale healthy value indefinitely.

## D12 — the stop sequence does not hold the services mutex

Shutting services down runs each `WithStop` in reverse registration order and
awaits it against the shutdown deadline — potentially the whole shutdown timeout.
`status()`, `liveness()`, and `readiness()` all take the same `services` mutex,
so holding it across the stop sequence would block every health/readiness probe
until shutdown finished — exactly when a load balancer most needs a prompt
not-ready answer.

The stop sequence therefore **snapshots the service slice under the lock and
releases it** before running any `WithStop`. Registration is already impossible
once the controller is `Stopping`, so the snapshot cannot go stale, and the
health probes stay responsive throughout shutdown.

## Signal registration hygiene

OS-signal registration is handled so it can neither be orphaned nor swallow
signals:

- `signal.Notify` is deferred to `Start` (in `startSignalHandler`), where it is
  registered **only if a signal channel survives** and **paired with the reader
  goroutine launched immediately below it**. Registering at construction would
  leave a controller that is constructed but never started with a handler and no
  reader — silently swallowing `SIGINT`/`SIGTERM` so the process ignores Ctrl-C.
  Deferring registration to `Start` means an unstarted controller keeps the
  signals at their default disposition.
- `WithoutSignals` sets the channel to `nil`, so `startSignalHandler` returns
  before registering anything.
- The registration is detached with `signal.Stop` when the signal channel is
  swapped out and again at shutdown, so a late signal never lands on a channel no
  one is reading.

## Related

- [Architecture & the lifecycle state machine](architecture.md) — the goroutines
  and states these properties operate on.
- [Handle graceful shutdown & signals](../how-to/graceful-shutdown.md) — the
  user-facing side of bounded shutdown.
- [The restart supervisor](restart-supervisor.md) — the error-channel contract
  D9 supports.

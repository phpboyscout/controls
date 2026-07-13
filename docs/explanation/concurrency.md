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

## Bounded shutdown — Wait can never hang

`Wait` blocks on a wait group sized to *services + 1*. The extra "+1" is the
controller's own lifecycle count, released **last** — only after the shutdown
handler has run every stop callback and set the `Stopped` state. So `Wait`
returning is a hard guarantee that shutdown finished.

Crucially, that guarantee holds even if a `WithStop` misbehaves. Each stop runs
in its own goroutine and is awaited against the shutdown-timeout deadline; a stop
that ignores its context is **abandoned** when the deadline elapses and the
sequence moves on. The abandoned goroutine is left to finish on its own, but it
can no longer hold up shutdown — so `Wait` returns within roughly the shutdown
timeout regardless of a stuck service.

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

## Signal registration hygiene

OS-signal registration is handled so it can neither be orphaned nor swallow
signals:

- `signal.Notify` is called **only after** all options are applied, and only if a
  signal channel survives — so `WithoutSignals` genuinely leaves `SIGINT`/`SIGTERM`
  with their default disposition rather than registering then discarding a handler.
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

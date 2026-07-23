# Handle graceful shutdown & signals

The controller drives a bounded, ordered shutdown: it cancels the context every
service shares, runs each `WithStop` in reverse registration order, and force-
abandons a stuck stop — or a supervisor whose `WithStart` never returns — at a
deadline so the shutdown sequence itself can never hang. This guide covers the
timeout, signal handling, bounding your wait with `WaitContext`, and how a
service distinguishes a controlled stop from an upstream cancellation.

## The shutdown sequence

Whether triggered by a signal, a direct `Stop()`, or parent-context
cancellation, shutdown always runs the same sequence:

1. Transition the controller to `Stopping`.
2. Detach OS-signal handling.
3. Cancel the controller context with the cause `ErrShutdown` — unblocking every
   `WithStart` that waits on `ctx.Done()`.
4. Run each `WithStop` in **reverse registration order**, bounded by the shutdown
   timeout.
5. Await supervisor exit with the *remaining* shutdown budget, logging a WARN
   naming any service whose `WithStart` failed to return (it is abandoned).
6. Transition to `Stopped` and release `Wait()`.

## Set the shutdown timeout

The timeout bounds the *whole* stop phase. It defaults to
`DefaultShutdownTimeout` (5s); override it with `WithShutdownTimeout`:

```go
c := controls.NewController(ctx,
	controls.WithShutdownTimeout(30*time.Second),
)
```

Each `WithStop` receives a context carrying this deadline. A well-behaved stop
respects it — for example, `http.Server.Shutdown(ctx)` drains in-flight requests
until the deadline, then returns.

> **The deadline context is fresh, not the cancelled controller context.** The
> `ctx` passed to `WithStop` is derived from `context.Background()` with the
> shutdown timeout — *not* from the already-cancelled controller context. That is
> deliberate: a context that was dead on arrival would make `http.Server.Shutdown`
> fail instantly instead of draining.

## Force-stop of context-ignoring stops

Each `WithStop` runs in its own goroutine and is awaited against the deadline. If
a stop function **ignores its context** and runs long, the controller abandons it
when the deadline elapses and moves on to the next service:

```go
controls.WithStop(func(ctx context.Context) {
	// BAD: ignores ctx, blocks for 10s regardless of the shutdown timeout.
	time.Sleep(10 * time.Second)
})
```

With a 5s timeout, the controller waits 5s, then abandons this goroutine (it is
left to finish on its own) and continues shutting down the remaining services.
This guarantees `Wait()` returns within roughly the shutdown timeout even when a
stop misbehaves — but the abandoned work is *not* cleanly completed. Always
honour the context:

```go
controls.WithStop(func(ctx context.Context) {
	select {
	case <-workDone:
	case <-ctx.Done(): // give up at the deadline
	}
})
```

## Bound your wait against a stuck start

The bare `Wait()` is unbounded: it returns only once every supervisor goroutine
has unwound, which requires every `WithStart` to return after cancellation. If a
start wraps third-party code that may ignore its context, use `WaitContext` so
your process cannot hang on a controller that already reports `Stopped`:

```go
c.Start()

ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

if err := c.WaitContext(ctx); err != nil {
	// The wait was abandoned: some StartFunc never returned. The shutdown
	// sequence itself completed; the stuck service was named in a WARN log.
}
```

On the abandon path the stuck supervisor goroutine is deliberately leaked — the
same tradeoff as an abandoned stop. See
[D10 in Concurrency & shutdown correctness](../explanation/concurrency.md).

## Signal handling

By default `NewController` registers handlers for `SIGINT` and `SIGTERM`. The
first signal initiates a graceful `Stop`. A **second** signal exits the signal
handler immediately, so a caller can escalate (for example to `os.Exit`) if a
shutdown is wedged.

### Disable signals in tests

Signal handling is unwanted in unit tests — you want to drive shutdown
explicitly. `WithoutSignals` leaves `SIGINT`/`SIGTERM` with their default
disposition (no orphaned registration):

```go
c := controls.NewController(ctx, controls.WithoutSignals())
c.Start()
// ... exercise the services ...
c.Stop()
c.Wait()
```

## Distinguish a controlled stop

When the controller shuts a service down, it cancels the context with a specific
cause: `controls.ErrShutdown`. A service can check the cause to tell a
*controlled* stop apart from an upstream cancellation (a failing parent context,
a deadline) and react differently:

```go
controls.WithStart(func(ctx context.Context) error {
	<-ctx.Done()

	if errors.Is(context.Cause(ctx), controls.ErrShutdown) {
		// Orderly shutdown — return the cause as an expected end-of-run.
		return ctx.Err()
	}

	// Cancelled by something upstream — treat as an abnormal exit if you wish.
	return context.Cause(ctx)
})
```

`context.Cause(ctx) == controls.ErrShutdown` is the reliable signal that *this
controller* initiated the stop.

## Related

- [Concurrency & shutdown correctness](../explanation/concurrency.md) — what
  the shutdown bound covers (and D10, the stuck-start abandon policy), why no
  goroutine leaks or busy-spins.
- [Architecture](../explanation/architecture.md) — the state machine and control
  goroutines behind the sequence above.

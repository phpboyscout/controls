# Handle graceful shutdown and signals

The controller drives a bounded, ordered shutdown: it cancels the context every
service shares, runs each `WithStop` in reverse registration order, and abandons
a stuck stop, or a supervisor whose `WithStart` never returns, at a deadline so
the shutdown sequence itself can never hang. This guide covers the timeout,
signal handling, bounding your wait with `WaitContext`, and how a service
distinguishes a controlled stop from an upstream cancellation.

## The shutdown sequence

Whether triggered by a signal, a direct `Stop()`, or parent-context
cancellation, shutdown always runs the same sequence:

1. Transition the controller to `Stopping`, from `Running` or from
   `UnableToStart`. Readiness is already false in both cases from this point on,
   so traffic stops being routed here before any service is stopped.
2. Detach OS-signal handling.
3. Cancel the controller context with the cause `ErrShutdown`, unblocking every
   `WithStart` that waits on `ctx.Done()`.
4. Run each `WithStop` in **reverse registration order**, bounded by the shutdown
   timeout.
5. Await supervisor exit with the *remaining* shutdown budget, logging a `WARN`
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
respects it. `http.Server.Shutdown(ctx)`, for example, drains in-flight requests
until the deadline, then returns.

> **Zero is not "use the default".** `WithShutdownTimeout(0)`, or any negative
> duration, creates a budget that has already expired, so every stop callback is
> abandoned the instant it is launched and may never run at all. Omit the option
> if you want the 5s default.

> **A `StopFunc` can run more than once, so make it idempotent.** Shutdown calls
> it once; a health-threshold restart calls it again before each restart of that
> service. An error-triggered restart does *not* call it; see
> [Configure restart policy](restart-policy.md).

> **The deadline context is fresh, not the cancelled controller context.** The
> `ctx` passed to `WithStop` is derived from `context.Background()` with the
> shutdown timeout, *not* from the already-cancelled controller context. That is
> deliberate: a context that was dead on arrival would make `http.Server.Shutdown`
> fail instantly instead of draining.

## Abandoning a stop that ignores its context

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
stop misbehaves, but the abandoned work is *not* cleanly completed. Always
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

On the abandon path the stuck supervisor goroutine is deliberately leaked, the
same trade as an abandoned stop. See
[D10 in Concurrency and shutdown correctness](../explanation/concurrency.md).

## Signal handling

**The controller installs no signal handler by default.** Signal disposition is
process-global state, so it belongs to whichever layer is outermost: usually
your `main`, or the CLI framework wrapping it. A library that registers its own
handler silently becomes a second owner of that global, and `signal.Notify` is
additive: every registered channel receives a copy, so two handlers means two
shutdowns racing.

So the controller observes rather than intercepts. Cancel the context you passed
to `NewController` and the services shut down gracefully; see
[Stopping from a parent context](#stopping-from-a-parent-context).

!!! warning "Using a CLI framework? Do not opt in."

    If something above you already turns signals into context cancellation
    (go-tool-base's root command does), passing `WithSignals` reintroduces
    exactly the double-handler race this default exists to prevent. Let the
    framework own the signal and cancel your context.

### Opting in from a standalone main

When the controller genuinely *is* the outermost layer, a daemon with no CLI
framework above it, ask for signals explicitly:

```go
c := controls.NewController(ctx, controls.WithSignals())
```

The first `SIGINT`/`SIGTERM` initiates a graceful `Stop`. A **second** signal
exits the signal handler immediately, so a caller can escalate (for example to
`os.Exit`) if a shutdown is wedged.

!!! warning "A second interrupt overrides your shutdown budget"

    Where the outermost layer force-exits on the second signal, as go-tool-base's
    root command does with `os.Exit(128+signum)`, the process dies immediately,
    mid-`WithStop` if necessary. Size `WithShutdownTimeout` for the *graceful*
    path; a user pressing `Ctrl-C` twice is deliberately overruling it.

### Tests need no option

Because signals are off by default, a test just constructs a controller and
drives shutdown explicitly:

```go
c := controls.NewController(ctx)
c.Start()
// ... exercise the services ...
c.Stop()
c.Wait()
```

### Stopping from a parent context

Cancelling the context you passed to `NewController` triggers the same graceful
sequence as `Stop()`. So does its deadline expiring:

```go
ctx, cancel := context.WithCancel(context.Background())
c := controls.NewController(ctx)
c.Start()

cancel()  // -> ordered, bounded shutdown, exactly as Stop() would
c.Wait()
```

The controller does not merely inherit that cancellation. It *reacts* to it,
running the full shutdown sequence. That is what makes `ErrShutdown` reliable
below, and it means an expired parent deadline gives your services an orderly
teardown bounded by `WithShutdownTimeout`, rather than a context that is already
dead when `WithStop` receives it.

## Recognise a controlled stop

When the controller shuts a service down it cancels the context with a specific
cause, `controls.ErrShutdown`, so a service can recognise an orderly teardown and
return an expected end-of-run rather than an error:

```go
controls.WithStart(func(ctx context.Context) error {
	<-ctx.Done()

	if errors.Is(context.Cause(ctx), controls.ErrShutdown) {
		// Orderly shutdown: an expected end-of-run, not a failure.
		return ctx.Err()
	}

	return context.Cause(ctx)
})
```

**`ErrShutdown` is the cause of every stop the controller drives**: a direct
`Stop()`, a parent cancellation, an expired parent deadline, or a signal when
you opted into `WithSignals`. One rule, no exceptions.

!!! note "This changed in v0.2.0, and it is a deliberate narrowing"

    The controller used to derive its context directly from yours, so a parent
    cancellation left the *parent's* cause on the service's context. That meant
    you could sometimes tell an upstream cancel from a controlled stop.
    "Sometimes" is the problem: which cause won was a race between the parent's
    cancellation and the controller's own, so the distinction was never
    dependable.

    The controller now owns its cancellation outright and treats your context's
    completion as a *trigger* for the normal shutdown sequence. You can no longer
    distinguish *why* the controller stopped you from the cause alone. If a
    service needs that, watch the parent context yourself. What you gain is that
    `ErrShutdown` finally means something unconditional.

## Related

- [Concurrency and shutdown correctness](../explanation/concurrency.md): what
  the shutdown bound covers (and D10, the stuck-start abandon policy), and why
  no goroutine leaks or busy-spins.
- [Architecture](../explanation/architecture.md): the state machine and control
  goroutines behind the sequence above.
- [Controller reference](../reference/controller.md): `Stop`, `Wait`,
  `WaitContext` and every option, including what each does when called in the
  wrong state.

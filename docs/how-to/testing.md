# Test services and mock the controller

Service lifecycle functions are ordinary functions and the controller is a
concrete type you can drive directly, so most testing needs no mocks at all.
When your own code depends on *a controller*, the `Controllable` interface (and
the narrower interfaces beside it) let you substitute a fake.

> **Signals are already off.** A controller installs no `SIGINT`/`SIGTERM`
> handler unless you ask for one, so a test needs no option to stay out of
> process-wide state — just never pass `WithSignals`. Drive shutdown explicitly
> with `Stop()`, or by cancelling the context you constructed it with.

## Test your Start/Stop/Status functions directly

`StartFunc`, `StopFunc`, and `StatusFunc` are plain functions — the simplest test
calls them without a controller at all:

```go
func TestStatusProbe(t *testing.T) {
	srv := newServer()

	status := statusFunc(srv) // returns controls.StatusFunc == func() error
	require.NoError(t, status())

	srv.forceUnhealthy()
	require.Error(t, status())
}
```

## Drive a real controller

For a lifecycle test, register against a real controller and drive it. This
exercises concurrent startup, health aggregation, and shutdown for real:

```go
func TestServiceLifecycle(t *testing.T) {
	ctx := context.Background()

	c := controls.NewController(ctx)

	var started, stopped atomic.Bool
	c.Register("worker",
		controls.WithStart(func(ctx context.Context) error {
			started.Store(true)
			<-ctx.Done() // serve until shutdown
			return ctx.Err()
		}),
		controls.WithStop(func(context.Context) { stopped.Store(true) }),
		controls.WithReadiness(func() error { return nil }),
	)

	c.Start()
	require.Eventually(t, started.Load, time.Second, time.Millisecond)

	// Health is read from the aggregated report — not a channel.
	require.True(t, c.Readiness().OverallHealthy)

	c.Stop()
	c.Wait() // blocks until the full shutdown sequence has finished
	require.True(t, stopped.Load())
}
```

Note that health is inspected with `Status()` / `Liveness()` / `Readiness()`,
which return a `HealthReport`. A `StatusFunc` returns `nil` when healthy and an
error otherwise; there is no health *channel* to read in application code.

## Mock the controller with the `Controllable` interface

Production code should hold the concrete `*controls.Controller`. But code that
merely *registers services against* a controller can depend on an interface and
be tested with a fake. The module ships no mocks, so generate one for the
interface you depend on (e.g. with [mockery](https://vektra.github.io/mockery/)),
or hand-write a small stub:

```go
// Your code depends on the interface, not the concrete controller.
func wireServices(c controls.Controllable, deps Deps) {
	c.Register("api", controls.WithStart(deps.serve), controls.WithStop(deps.drain))
}
```

```go
func TestWireServices(t *testing.T) {
	fake := NewMockControllable(t) // your generated mock
	fake.EXPECT().Register("api", mock.Anything, mock.Anything).Return()

	wireServices(fake, testDeps())
}
```

### Depend on the narrowest interface

`Controllable` is the full surface. Prefer a narrower interface when your code
only needs part of it — it makes the dependency (and the mock) smaller:

| Interface | Use when your code only needs to… |
|---|---|
| `Runner` | `Start`, `Stop`, `Register` services, and query `IsRunning`/`IsStopped`/`IsStopping` |
| `StateAccessor` | read/set the lifecycle `State`, or read the context or logger |
| `Configurable` | apply configuration (logger, shutdown timeout, wait group, channels) |
| `ChannelProvider` | access the message, error and signal channels |
| `HealthReporter` | read `Status()` / `Liveness()` / `Readiness()` reports, or `GetServiceInfo` |

> [!important]
> **`Wait` and `WaitContext` are on the concrete `*Controller`, not on any
> interface** — including `Controllable`. Code that has to block until shutdown
> completes therefore cannot take `Controllable` alone; accept
> `*controls.Controller`, or declare a one-method interface of your own:
>
> ```go
> type waiter interface{ Wait() }
> ```

Reserve the concrete `*controls.Controller` for production wiring; reach for the
interfaces at test seams.

## Related

- [Register & run services](register-services.md)
- [Add health checks](health-checks.md)
- [Handle graceful shutdown & signals](graceful-shutdown.md)

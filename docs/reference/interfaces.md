# Interfaces reference

`*Controller` is the only implementation in this module. The interfaces exist so
your code can depend on the part of it that it actually uses, and so a test can
substitute a fake. This page lists each interface's methods and, just as
usefully, the methods that are on none of them.

## The interfaces and their methods

| Interface | Methods |
|---|---|
| `Runner` | `Start()`, `Stop()`, `IsRunning() bool`, `IsStopped() bool`, `IsStopping() bool`, `Register(id string, opts ...ServiceOption)` |
| `HealthReporter` | `Status() HealthReport`, `Liveness() HealthReport`, `Readiness() HealthReport`, `GetServiceInfo(name string) (ServiceInfo, bool)` |
| `HealthCheckReporter` | everything in `HealthReporter`, plus `GetCheckResult(name string) (CheckResult, bool)` |
| `StateAccessor` | `GetState() State`, `SetState(State)`, `GetContext() context.Context`, `GetLogger() *slog.Logger` |
| `Configurable` | `SetErrorsChannel(chan error)`, `SetMessageChannel(chan Message)`, `SetSignalsChannel(chan os.Signal)`, `SetWaitGroup(*sync.WaitGroup)`, `SetShutdownTimeout(time.Duration)`, `SetLogger(*slog.Logger)` |
| `ChannelProvider` | `Messages() chan Message`, `Errors() chan error`, `Signals() chan os.Signal` |
| `Controllable` | `Runner` + `HealthReporter` + `StateAccessor` + `Configurable` + `ChannelProvider` |

`*Controller` satisfies all of them, asserted at compile time in the package.

## Methods that are on no interface

Depending on `Controllable` — the widest interface — still does not give you:

| Method | Why it matters |
|---|---|
| `Wait()` | Code that blocks until shutdown completes cannot take `Controllable`. Accept `*controls.Controller`, or declare `type waiter interface{ Wait() }` of your own. |
| `WaitContext(ctx) error` | Same, for the deadline-bounded wait. |
| `RegisterHealthCheck(HealthCheck) error` | Wiring code that registers standalone checks needs the concrete type. |
| `GetCheckResult(name)` | On `HealthCheckReporter`, which `Controllable` does **not** embed. Depend on `HealthCheckReporter` if you read cached check results. |
| `WaitGroup() *sync.WaitGroup` | `Configurable` has the setter but no getter. |

## Which interface to depend on

| Your code… | Depend on |
|---|---|
| registers services and drives start/stop | `Runner` |
| serves a health endpoint | `HealthReporter` |
| serves a health endpoint and reads named check results | `HealthCheckReporter` |
| reads the lifecycle state, context or logger | `StateAccessor` |
| configures a controller during construction | `Configurable` |
| consumes the message, error or signal channel | `ChannelProvider` |
| does several of the above | `Controllable`, or `*controls.Controller` |

Production wiring is usually simplest with the concrete `*controls.Controller`;
the interfaces earn their keep at test seams. **The module ships no mocks** —
generate one for the interface you depend on, or hand-write a stub. See [Test
services and mock the controller](../how-to/testing.md).

## Option types

| Type | Signature | Who can write one |
|---|---|---|
| `ControllerOpt` | `func(Configurable)` | Anyone, but a third-party option can only call the six `Configurable` setters. `WithValidError` reaches further through an unexported interface, so its behaviour cannot be reproduced outside the package. |
| `ServiceOption` | `func(*Service)` | Anyone. `Service` and all its fields are exported, so you can write your own service options — `func(s *controls.Service) { s.Name = ... }` is legal. |

## Function types

| Type | Signature | Used by |
|---|---|---|
| `StartFunc` | `func(context.Context) error` | `WithStart` |
| `StopFunc` | `func(context.Context)` | `WithStop` |
| `StatusFunc` | `func() error` | `WithStatus` |
| `ProbeFunc` | `func() error` | `WithLiveness`, `WithReadiness` |
| `ValidErrorFunc` | `func(error) bool` | `WithValidError` |

`StatusFunc` and `ProbeFunc` are separate named types with identical underlying
signatures; a plain `func() error` satisfies either.

## Related

- [Test services and mock the controller](../how-to/testing.md) — how to use
  these at a test seam.
- [Controller](controller.md) — what each method does.

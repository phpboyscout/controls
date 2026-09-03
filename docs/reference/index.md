# Reference

Every option, field, default and method in `controls`, with what it does, what it defaults to, and
what happens when it is set wrongly. This tier answers "what is this and what
value should it have"; the [how-to guides](../how-to/register-services.md)
answer "how do I do X", and [explanation](../explanation/architecture.md)
answers "why is it built this way".

## What is on each reference page

| Page | Covers |
|---|---|
| [Controller](controller.md) | `NewController`, every `ControllerOpt`, every controller method, and what each does when called at the wrong time |
| [Services and restart policy](services.md) | `Register`, every `ServiceOption`, the `StartFunc`, `StopFunc`, `StopErrFunc` and `StatusFunc` contracts, `RestartPolicy` fields, `ServiceInfo` |
| [Supervisor](supervisor.md) | `NewSupervisor`, `WithOnFailure`, every `Supervisor` method, the `Child` contract, `ChildState`, `Failure`, and what each call does out of order |
| [Generational](generational.md) | The `Build`, `Release` and `Probe` contracts, every method, the `Release` retry, the four sentinels, and what each call does out of order |
| [Health checks and reports](health.md) | `HealthCheck` fields, `CheckStatus`, `CheckType`, `CheckResult`, the `HealthReport` JSON shape, and what each report includes |
| [Defaults and timings](defaults.md) | Every default value in one table, the option or field that changes it, and what a zero value means |
| [Interfaces](interfaces.md) | `Runner`, `HealthReporter`, `HealthCheckReporter`, `StateAccessor`, `Configurable`, `ChannelProvider`, `Controllable`, and the methods that are on none of them |

## Where the generated API listing lives

This tier is written by hand and states behaviour the type signatures do not
show. The generated, always-current listing of every exported symbol, with the
runnable `Example` tests, is on
[pkg.go.dev](https://pkg.go.dev/gitlab.com/phpboyscout/go/controls). Use both:
pkg.go.dev for signatures, this tier for defaults and failure modes.

## What is deliberately not here

`controls` has no configuration file, no environment variables and no
command-line flags. It is a library, and every knob is a functional option or a
struct field, listed on the pages above. If you are looking for a config key,
you are looking for the framework or application that embeds this module.

For what the module does not do at all, see
[What controls does not do](../explanation/limitations.md).

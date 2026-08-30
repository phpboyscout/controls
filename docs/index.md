# controls

A small **service-lifecycle supervisor** for long-running Go processes —
concurrent startup, health probes, ordered (reverse-registration) shutdown, and self-healing restarts,
behind one `Controller`, as a light, framework-free library.

`controls` is the same supervisor behind [go-tool-base](https://gitlab.com/phpboyscout/go-tool-base)'s
service commands, extracted so any project can register its services (HTTP/gRPC
servers, background workers, schedulers) and let the `Controller` start them
concurrently, monitor their health, optionally take ownership of
`SIGINT`/`SIGTERM`, and drive a bounded graceful shutdown — **without** pulling
in the framework.

```go
import "gitlab.com/phpboyscout/go/controls"
```

## The light-footprint promise

The module graph is deliberately tiny. `go.mod` declares exactly one external
dependency, [cockroachdb/errors](https://github.com/cockroachdb/errors) — no
config framework, no TUI, no OpenTelemetry, no go-tool-base. A
`depfootprint_test.go` guard fails the build if any forbidden dependency ever
enters the graph.

The only logging seam is the standard library's `*slog.Logger`, injected via
`WithLogger`. Supply one, or supply none — it defaults to a discard handler, so
logging is never mandatory and never a dependency you must adopt.

## Design

- **Framework-free.** One external dependency (`cockroachdb/errors`); one
  logging seam (`*slog.Logger`). Everything else is functional options.
- **Correct concurrency.** Idempotent `Start`/`Stop`; goroutines that terminate
  on shutdown rather than leak or busy-spin; a restart supervisor that
  distinguishes clean start / cancellation / failure and never floods the error
  channel; force-stop at the shutdown deadline; readiness that fails closed
  until the first async health check has actually run. Race-clean under `-race`.
- **Transport-agnostic health.** `Status()` / `Liveness()` / `Readiness()`
  return plain `HealthReport` values. The module does **not** open ports or
  register HTTP/gRPC handlers — you wire the reports into whatever transport
  your process already speaks (see [Add health checks](how-to/health-checks.md)).

## Quick start

```go
package main

import (
	"context"

	"gitlab.com/phpboyscout/go/controls"
)

func main() {
	c := controls.NewController(context.Background())

	c.Register("api",
		controls.WithStart(func(ctx context.Context) error {
			// start serving; return when ctx is cancelled
			<-ctx.Done()
			return ctx.Err()
		}),
		controls.WithStop(func(ctx context.Context) {
			// graceful shutdown, bounded by ctx
		}),
	)

	c.Start() // launches the services and returns; installs no signal handler
	c.Wait()  // blocks until Stop (or the parent context) drains the shutdown sequence
}
```

Add `controls.WithSignals()` to `NewController` when this process is the
outermost layer and `Ctrl-C` should stop it — see [Handle graceful shutdown &
signals](how-to/graceful-shutdown.md#signal-handling).

## Key concepts

- **`Controller`** — the supervisor. `Register` services before `Start`; `Wait`
  blocks until the full shutdown sequence has completed and every supervisor
  goroutine has unwound (it requires context-respecting start callbacks —
  `WaitContext` is the deadline-bounded variant for services that may wrap
  cancellation-ignoring third-party code).
- **Health probes** — attach `WithStatus` / `WithLiveness` / `WithReadiness` to
  a service, or register standalone `HealthCheck`s (sync, or async with an
  `Interval`). `Status()` / `Liveness()` / `Readiness()` aggregate them into a
  `HealthReport`.
- **Restart policy** — `WithRestartPolicy` enables self-healing with exponential
  backoff, a `MaxRestarts` cap, a health-failure threshold, and a
  consecutive-failure counter that resets after a healthy window.
- **Options** — `WithLogger`, `WithShutdownTimeout`, `WithSignals` (standalone mains),
  and `WithValidError` (exempt expected terminal errors like
  `http.ErrServerClosed` from the restart count).

## Install

```sh
go get gitlab.com/phpboyscout/go/controls
```

## Where to go next

The documentation follows the [Diátaxis](https://diataxis.fr/) framework:

- **[Getting started](tutorials/getting-started.md)** — a learning-oriented
  walkthrough: register one service, start it, and shut down cleanly on Ctrl-C.
- **How-to guides** — task-oriented recipes:
    - [Register & run services](how-to/register-services.md)
    - [Add health checks](how-to/health-checks.md)
    - [Configure restart policy](how-to/restart-policy.md)
    - [Handle graceful shutdown & signals](how-to/graceful-shutdown.md)
    - [Supervise children that come and go](how-to/supervise-dynamic-children.md)
- **Explanation** — understanding-oriented background:
    - [Architecture & the lifecycle state machine](explanation/architecture.md)
    - [Health, liveness & readiness](explanation/health-model.md)
    - [The restart supervisor](explanation/restart-supervisor.md)
    - [Concurrency & shutdown correctness](explanation/concurrency.md)
    - [What controls does not do](explanation/limitations.md)
- **[Reference](reference/index.md)** — every option, field and default, with
  what happens when it is set wrongly:
    - [Controller](reference/controller.md)
    - [Services and restart policy](reference/services.md)
    - [Supervisor](reference/supervisor.md)
    - [Health checks and reports](reference/health.md)
    - [Defaults and timings](reference/defaults.md)
    - [Interfaces](reference/interfaces.md)

    The generated listing of every exported symbol, with runnable `Example`
    tests, is on
    [pkg.go.dev](https://pkg.go.dev/gitlab.com/phpboyscout/go/controls).

> **Using go-tool-base?** The framework wires this supervisor into its service
> commands and exposes the `HealthReport`s through its own `pkg/http` and
> `pkg/grpc` handlers. That glue lives in go-tool-base, not here; this module is
> transport- and framework-agnostic.

## Further reading

The blog carries a curated route through this subject: **[Building a command-line tool in Go](https://phpboyscout.uk/topics/building-a-cli-in-go/)** collects
everything written about it, ordered so you can start at the beginning rather
than newest-first.

!!! tip "Ask phpbotscout"

    ![phpbotscout](https://phpboyscout.uk/images/projects/logo-phpbotscout.png){ width="84" align=left style="border-radius:10px;margin-right:1rem" }

    He answers questions about the projects over on the Discord, citing the docs
    where they already cover it, and offering to raise an issue where they don't.
    Bring a bug, an idea, or a questionable engineering decision.

    [Join the Discord](https://discord.gg/mQzGbmGyzZ){ .md-button .md-button--primary }

<div align="center">

# controls

**Service-lifecycle supervisor for Go: concurrent startup, health probes, ordered (reverse-registration) shutdown, self-healing restarts**

[![Go Reference](https://pkg.go.dev/badge/gitlab.com/phpboyscout/go/controls.svg)](https://pkg.go.dev/gitlab.com/phpboyscout/go/controls)
[![Pipeline](https://gitlab.com/phpboyscout/go/controls/badges/main/pipeline.svg)](https://gitlab.com/phpboyscout/go/controls/-/pipelines)
[![Coverage](https://gitlab.com/phpboyscout/go/controls/badges/main/coverage.svg)](https://gitlab.com/phpboyscout/go/controls/-/graphs/main/charts)
[![phpboyscout Go toolkit](https://img.shields.io/badge/phpboyscout-Go%20toolkit-554488?logo=gitlab&logoColor=white)](https://go.phpboyscout.uk)

<em>Part of the <a href="https://go.phpboyscout.uk">phpboyscout Go toolkit</a>: small, framework-free Go modules extracted from <a href="https://gitlab.com/phpboyscout/go-tool-base">go-tool-base</a>. Docs: <a href="https://controls.go.phpboyscout.uk">controls.go.phpboyscout.uk</a></em>

</div>

---

`gitlab.com/phpboyscout/go/controls` is a small **service-lifecycle supervisor**
for long-running Go processes. Register your services (HTTP and gRPC servers,
background workers, schedulers) and the `Controller` starts them concurrently,
aggregates the health probes they supply, optionally takes ownership of
`SIGINT`/`SIGTERM`, and drives a bounded graceful shutdown in reverse
registration order, with an optional self-healing restart policy. The shutdown
bound covers stop callbacks *and* supervisor exit: a stop that ignores its
context, or a start that never returns, is abandoned at the deadline and named
in a `WARN` rather than left to wedge shutdown.

It is the same supervisor behind go-tool-base's service commands, extracted so
any project can adopt it **without** pulling in the framework.

## Design

- **Framework-free.** The only external dependency is
  [`go/errors`](https://gitlab.com/phpboyscout/go/errors), a sibling toolkit
  module. No config framework, no TUI, no OpenTelemetry, no go-tool-base, and
  `depfootprint_test.go` fails the build if any of them reaches the module graph.
- **Stdlib seams.** The only logging seam is a `*slog.Logger`; omit it and the
  controller logs to a discard handler. Everything else is functional options.
- **Correct concurrency.** Idempotent `Start`/`Stop`; a restart policy that
  distinguishes a clean start from a cancellation from a failure and never floods
  the error channel; a stop callback that overruns the shutdown deadline is
  abandoned rather than waited for; readiness fails closed until the first async
  health check has run. Race-clean under `-race`.

## Install

```bash
go get gitlab.com/phpboyscout/go/controls
```

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

No OS signal handler is installed unless you pass `controls.WithSignals()`.
Signal disposition is process-global, so it belongs to the outermost layer, and
under a CLI framework that already turns signals into context cancellation that
layer is not the controller.

## Key concepts

- **`Controller`** is the supervisor. `Register` services before `Start`; `Wait`
  blocks until the full shutdown sequence completes and every supervisor
  goroutine has unwound. `Wait` requires start callbacks that return when their
  context is cancelled; `WaitContext` is the deadline-bounded variant for a
  service that wraps third-party code which may not.
- **Health probes.** Attach `WithStatus`, `WithLiveness` or `WithReadiness` to a
  service, or register standalone `HealthCheck`s (sync, or async with an
  `Interval`). `Status()`, `Liveness()` and `Readiness()` return aggregate
  `HealthReport` values for wiring into whatever transport your process speaks.
  The module opens no port of its own.
- **Restart policy.** `WithRestartPolicy` enables self-healing with exponential
  backoff, a `MaxRestarts` cap on consecutive failures, a health-failure
  threshold, and a counter that resets after a healthy window.
- **`Supervisor`** manages a set of children that attach and detach while the
  process runs, under the same `RestartPolicy` rules. A `Controller` manages a
  fixed set with ordered shutdown; a `Supervisor` manages a changing one with
  concurrent shutdown, and a process usually wants both.
- **`Generational`** owns one generation of a resource at a time, for a service
  that holds something single-use (a listener, a server, a supervisor) and has
  to survive a restart.
- **Options.** `WithLogger`, `WithShutdownTimeout`, `WithSignals` for a
  standalone main, and `WithValidError` to exempt an expected terminal error such
  as `http.ErrServerClosed` from the restart count.

## The lint

`lint/` is a nested module holding `singleuse`, an advisory analyzer that names
the line where a `StartFunc` captures a server, listener or supervisor a
restart cannot reuse. It keeps `x/tools` out of this module's graph.

```bash
go run gitlab.com/phpboyscout/go/controls/lint/cmd/singleuse@latest ./...
```

## Documentation

Full guides and design notes: **[controls.go.phpboyscout.uk](https://controls.go.phpboyscout.uk)**.
API reference: **[pkg.go.dev](https://pkg.go.dev/gitlab.com/phpboyscout/go/controls)**.

## License

See [LICENSE](LICENSE).

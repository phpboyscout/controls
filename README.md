<div align="center">

# controls

**Service-lifecycle supervisor for Go — concurrent startup, health probes, ordered (reverse-registration) shutdown, self-healing restarts**

[![Go Reference](https://pkg.go.dev/badge/gitlab.com/phpboyscout/go/controls.svg)](https://pkg.go.dev/gitlab.com/phpboyscout/go/controls)
[![Pipeline](https://gitlab.com/phpboyscout/go/controls/badges/main/pipeline.svg)](https://gitlab.com/phpboyscout/go/controls/-/pipelines)
[![Coverage](https://gitlab.com/phpboyscout/go/controls/badges/main/coverage.svg)](https://gitlab.com/phpboyscout/go/controls/-/graphs/main/charts)
[![phpboyscout Go toolkit](https://img.shields.io/badge/phpboyscout-Go%20toolkit-554488?logo=gitlab&logoColor=white)](https://go.phpboyscout.uk)

<em>Part of the <a href="https://go.phpboyscout.uk">phpboyscout Go toolkit</a> &mdash; small, framework-free Go modules extracted from <a href="https://gitlab.com/phpboyscout/go-tool-base">go-tool-base</a>. Docs: <a href="https://controls.go.phpboyscout.uk">controls.go.phpboyscout.uk</a></em>

</div>

---

`gitlab.com/phpboyscout/go/controls` — a small **service-lifecycle supervisor**
for long-running Go processes. Register your services (HTTP/gRPC servers,
background workers, schedulers), and the `Controller` orchestrates their startup
ordering, monitors their health (liveness/readiness), forwards OS signals, and
drives a bounded graceful shutdown in reverse registration order — with an
optional self-healing restart policy. The shutdown bound covers stop callbacks
*and* supervisor exit: a context-ignoring stop — or a start that never returns —
is abandoned (and named in a WARN) at the deadline rather than wedging shutdown.

It is the same supervisor behind go-tool-base's service commands, extracted so
any project can adopt it **without** pulling in the framework.

## Design

- **Framework-free.** The only external dependency is `cockroachdb/errors`; the
  only logging seam is a nil-safe `*slog.Logger`. No config framework, no TUI, no
  OpenTelemetry, no go-tool-base. A `depfootprint_test.go` guard enforces it.
- **Stdlib seams.** Bring a `*slog.Logger` (or none — it defaults to a discard
  handler). Everything else is functional options.
- **Correct concurrency.** Idempotent `Start`/`Stop`, a restart policy that
  distinguishes clean-exit / cancellation / failure and never floods the error
  channel, force-stop on shutdown timeout, and readiness that fails closed until
  the first async health check completes. Race-clean under `-race`.

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

	c.Start() // registers SIGINT/SIGTERM handlers by default
	c.Wait()  // blocks until a signal (or Stop) drains the shutdown sequence
}
```

## Key concepts

- **`Controller`** — the supervisor. `Register` services before `Start`; `Wait`
  blocks until the full shutdown sequence completes and every supervisor
  goroutine has unwound (it requires context-respecting start callbacks —
  `WaitContext` is the deadline-bounded variant for services that may wrap
  cancellation-ignoring third-party code).
- **Health probes** — attach `WithStatus` / `WithLiveness` / `WithReadiness` to a
  service, or register standalone `HealthCheck`s (sync or async with an
  `Interval`). `Status()` / `Liveness()` / `Readiness()` return aggregate
  `HealthReport`s for wiring into transport health endpoints.
- **Restart policy** — `WithRestartPolicy` enables self-healing with exponential
  backoff, a `MaxRestarts` cap, a health-failure threshold, and a consecutive-
  failure counter that resets after a healthy window.
- **Options** — `WithLogger`, `WithShutdownTimeout`, `WithoutSignals` (tests),
  `WithValidError` (exempt expected terminal errors like `http.ErrServerClosed`
  from the restart count).

## Documentation

Full guides and design notes: **[controls.go.phpboyscout.uk](https://controls.go.phpboyscout.uk)**.
API reference: **[pkg.go.dev](https://pkg.go.dev/gitlab.com/phpboyscout/go/controls)**.

## License

See [LICENSE](LICENSE).

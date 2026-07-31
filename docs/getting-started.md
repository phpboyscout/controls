# Getting started

This tutorial walks you through your first supervised process, end to end: you
will register one background worker, start it under a `Controller`, and watch it
shut down cleanly when you press Ctrl-C.

You will do everything in one self-contained Go program so you can see each
moving part. By the end you will understand what `Register`, `Start`, and — most
importantly — `Wait` actually guarantee.

## Prerequisites

- Go 1.26 or newer.
- A new module to experiment in:

```sh
mkdir controls-tutorial && cd controls-tutorial
go mod init example.test/controls-tutorial
go get gitlab.com/phpboyscout/go/controls
```

## Step 1 — Create a controller

A `Controller` is constructed from a parent `context.Context` and zero or more
options. The context is the root of every service's lifetime: when it is
cancelled — by a signal, by `Stop`, or by the parent — every service's `ctx`
unblocks.

```go
ctx := context.Background()
c := controls.NewController(ctx, controls.WithSignals())
```

`WithSignals` asks the controller to handle `SIGINT`/`SIGTERM` itself, which is
what we want here: this program is a standalone daemon, so the controller is the
outermost layer and Ctrl-C should trigger a graceful shutdown.

It is opt-in because signal disposition is process-global. If you are building on
a CLI framework that already turns signals into context cancellation — go-tool-base
does — leave it off and let the framework cancel the context you pass in; the
controller shuts down just the same. See
[Handle graceful shutdown & signals](how-to/graceful-shutdown.md).

With no other options the controller logs to a discard handler.

## Step 2 — Register a service

A *service* is a name plus a set of lifecycle callbacks supplied as functional
options. The two that matter most are:

- **`WithStart`** — a `func(ctx context.Context) error`. It receives a context
  that is cancelled when the controller shuts down.
- **`WithStop`** — a `func(ctx context.Context)`. It runs during shutdown, with
  a context bounded by the shutdown timeout.

Here is a worker that ticks once a second until its context is cancelled:

```go
c.Register("ticker",
	controls.WithStart(func(ctx context.Context) error {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				// The controller is shutting down. Return the cause so the
				// supervisor sees a clean, expected end-of-run.
				return ctx.Err()
			case t := <-ticker.C:
				fmt.Println("tick", t.Format(time.TimeOnly))
			}
		}
	}),
	controls.WithStop(func(ctx context.Context) {
		fmt.Println("ticker stopping")
	}),
)
```

> **Register before Start.** Registration must happen before `Start`. Once
> `Start` runs, the controller has snapshotted its service set and launched the
> supervisor goroutines, so a later `Register` is never started or stopped — it
> only logs a warning. Treat that warning as a bug to fix.

## Step 3 — Start, then wait

```go
c.Start() // launches every registered service, returns immediately
c.Wait()  // blocks until the whole shutdown sequence has finished
```

`Start` is non-blocking: it launches a supervisor goroutine per service and the
control goroutines, then returns. `Wait` is where your `main` parks.

## Step 4 — Shut down with Ctrl-C

Because we passed `WithSignals`, the controller is listening for `SIGINT` and
`SIGTERM`. Pressing Ctrl-C:

1. delivers `SIGINT`, which the signal handler turns into a `Stop`;
2. cancels the controller context, so the ticker's `<-ctx.Done()` fires and its
   `WithStart` returns;
3. runs each `WithStop` in reverse registration order, bounded by the shutdown
   timeout;
4. releases `Wait`.

## The complete program

```go
package main

import (
	"context"
	"fmt"
	"time"

	"gitlab.com/phpboyscout/go/controls"
)

func main() {
	c := controls.NewController(context.Background(), controls.WithSignals())

	c.Register("ticker",
		controls.WithStart(func(ctx context.Context) error {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case t := <-ticker.C:
					fmt.Println("tick", t.Format(time.TimeOnly))
				}
			}
		}),
		controls.WithStop(func(ctx context.Context) {
			fmt.Println("ticker stopping")
		}),
	)

	c.Start()
	c.Wait()

	fmt.Println("shutdown complete")
}
```

Run it, let it tick a few times, then press Ctrl-C:

```sh
go run .
```

## What `Wait()` guarantees

`Wait` does not simply block until your `Start` functions return — it blocks
until the **entire shutdown sequence** has finished. Internally the controller
adds one wait-group count per service *plus one for its own lifecycle*, and that
extra count is only released after it has:

1. cancelled the controller context,
2. run every `WithStop` (or abandoned a stuck one at the deadline),
3. transitioned to the `Stopped` state.

So when `Wait` returns, you know shutdown is genuinely complete — every stop
callback has been given its chance to run and no supervised goroutine is still
draining. That makes `c.Wait()` the correct and only thing your `main` needs to
block on.

## Success criterion

The program prints a `tick` line every second. On Ctrl-C it prints
`ticker stopping`, then `shutdown complete`, and exits promptly. If it hangs on
shutdown, a `WithStop` is ignoring its context — see
[Handle graceful shutdown & signals](how-to/graceful-shutdown.md).

## Next steps

- Register several services and control their ordering:
  [Register & run services](how-to/register-services.md).
- Report health to an HTTP or gRPC endpoint:
  [Add health checks](how-to/health-checks.md).
- Make a service self-healing: [Configure restart policy](how-to/restart-policy.md).
- Understand the lifecycle state machine:
  [Architecture](explanation/architecture.md).

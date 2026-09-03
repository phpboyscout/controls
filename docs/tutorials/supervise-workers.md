# Supervise workers that come and go

In this tutorial you'll take a process that already runs under a `Controller`
and give it a set of workers that changes while it runs: a session per tenant,
attached as tenants arrive and detached as they leave. One tenant will fail,
and you'll watch the process stay ready anyway, which is the whole point of the
type you're about to meet. Allow about fifteen minutes.

It builds on [Getting started](getting-started.md). If you haven't done that
one, do it first: this page assumes you know what `Register`, `Start` and
`Wait` do, and doesn't explain them again.

## Prerequisites

- Go 1.27 or newer.
- The module from the getting-started tutorial, or a fresh one:

```sh
mkdir controls-tutorial && cd controls-tutorial
go mod init example.test/controls-tutorial
go get gitlab.com/phpboyscout/go/controls
```

## Start with a controller that owns the signal

Same opening as before. This is a standalone program, so the controller is the
outermost layer and `Ctrl-C` should stop it:

```go
c := controls.NewController(context.Background(), controls.WithSignals())
```

## Register a supervisor as a service

A `Supervisor` runs children that attach and detach at any time. It has a
lifecycle of its own, so it registers with the controller like any other
service. `Start`, `Stop` and `Readiness` already have the right signatures:

```go
sup := controls.NewSupervisor(
	controls.WithOnFailure(func(f controls.Failure) {
		fmt.Println("FAILED:", f.Name, "->", f.Err)
	}),
)

c.Register("tenants",
	controls.WithStart(sup.Start),
	controls.WithStop(sup.Stop),
	controls.WithReadiness(sup.Readiness),
)
_ = c.RegisterHealthCheck(sup.HealthCheck("tenant-sessions"))
```

Two things to notice. `WithOnFailure` is how you'll hear about a child that has
given up; you'll see it fire later. And the health check is registered under a
different name from the service, because `RegisterHealthCheck` only checks
names against other health checks, so a collision with `"tenants"` would be
accepted and reported twice.

Registering the supervisor makes the *supervisor* a requirement for the process.
Its children are not, and that difference is what you'll be watching for.

## Give each tenant a session

A child is a `StartFunc`, like a service's. This one connects, waits until it's
cancelled, and disconnects. The `broken` flag is for later:

```go
func session(name string, broken bool) controls.StartFunc {
	return func(ctx context.Context) error {
		if broken {
			return errors.New("no such tenant")
		}

		fmt.Println(name, "connected")
		<-ctx.Done()
		fmt.Println(name, "disconnected")

		return ctx.Err()
	}
}
```

## Attach a tenant every two seconds

Something has to decide when tenants arrive. Here it's a ticker inside an
ordinary registered service, which is a fine place for it. Attaching after the
supervisor has started is exactly what a `Supervisor` is for; a `Controller`
would log a warning and ignore you.

```go
c.Register("dispatcher",
	controls.WithStart(func(ctx context.Context) error {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for n := 1; ; n++ {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				name := fmt.Sprintf("tenant-%d", n)
				if err := sup.Attach(controls.Child{Name: name, Start: session(name, false)}); err != nil {
					fmt.Println("attach", name, "refused:", err)
				}
			}
		}
	}),
)
```

The dispatcher is registered *after* the supervisor on purpose. Shutdown runs in
reverse registration order, so the dispatcher stops first and stops attaching
before the supervisor is told to stop. Register them the other way round and a
tenant could arrive at a supervisor that's already shutting down. It'd be
refused with `ErrSupervisorStopped` rather than lost, but there's no reason to
find out.

## Watch them come and go

`sup.Health()` returns a map of every attached child and its state. Maps aren't
ordered, so sort the names before printing:

```go
func states(sup *controls.Supervisor) string {
	h := sup.Health()

	names := make([]string, 0, len(h))
	for n := range h {
		names = append(names, n)
	}

	sort.Strings(names)

	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, n+"="+string(h[n].State))
	}

	return strings.Join(parts, " ")
}
```

Then print a line after every attach, inside the ticker case. Alongside the
states, print whether the process as a whole is ready, and what the supervisor's
own health check says. `Readiness()` builds the report, which is what runs that
check, so read the check's result after it:

```go
ready := c.Readiness().OverallHealthy

check, _ := c.GetCheckResult("tenant-sessions")
verdict := check.Message
if verdict == "" {
	verdict = "ok"
}

fmt.Println("health:", states(sup), "| process ready:", ready, "| check:", verdict)
```

Run what you have so far and let it go for six seconds or so:

```sh
go run .
```

```text
health: tenant-1=pending | process ready: true | check: ok
tenant-1 connected
tenant-2 connected
health: tenant-1=running tenant-2=running | process ready: true | check: ok
```

The first line may say `pending` or `running` depending on which of your
goroutines got there first: the supervisor launches a child on its own
goroutine the moment it's attached, and the health line is printed on yours.
Both are honest answers a few microseconds apart.

## Let a tenant fail

Change the attach so the third tenant is broken:

```go
Start: session(name, n == 3),
```

Its session returns an error straight away. The child has no `RestartPolicy`,
so it runs once and its outcome is final, and the supervisor reports it through
the callback you registered at the start:

```text
health: tenant-2=running tenant-3=failed | process ready: true | check: 1 of 2 supervised children have failed
FAILED: tenant-3 -> controls: child "tenant-3" exhausted its restart policy: no such tenant
```

Look at the middle of that first line. A tenant has failed and the process
still reports **ready**. A registered *service* failing its probe would have
flipped that to `false` for the whole process; a supervised *child* never does.
What you get instead is the health check saying `DEGRADED` with a count, which
an operator can see and a readiness probe ignores, and the `Failure` handed to
your callback to act on. Whether one dead tenant matters is your call, and the
supervisor doesn't make it for you. If you want the reasoning, it's on
[Registered means required](../explanation/supervision.md).

The `FAILED:` line may print before or after the health line. The callback runs
on the supervisor's own goroutine, so it races your ticker.

## Detach the ones that leave

Tenants leave as well as arrive. From the third tick on, detach the tenant that
attached two ticks ago, with a budget, so a session that ignores its
cancellation can't hold your dispatcher up. Add this inside the ticker case,
after the attach:

```go
if n > 2 {
	leaving := fmt.Sprintf("tenant-%d", n-2)

	dctx, cancel := context.WithTimeout(ctx, time.Second)
	err := sup.Detach(dctx, leaving)
	cancel()

	fmt.Println("detached", leaving, "->", err)
}
```

`Detach` cancels that child's context, waits for its session to return, and
forgets it. The session prints `disconnected` on the way out:

```text
tenant-1 disconnected
detached tenant-1 -> <nil>
health: tenant-2=running tenant-3=failed | process ready: true | check: 1 of 2 supervised children have failed
```

Two ticks later the broken tenant is detached the same way. It had already
stopped, so there's nothing to wait for and the call returns at once:

```text
detached tenant-3 -> <nil>
tenant-5 connected
health: tenant-4=running tenant-5=running | process ready: true | check: ok
```

Once it's gone, the check is back to `ok`. If a session *had* ignored its
cancellation past the one-second budget, you'd have got `ErrDetachTimeout`
here, and the child would still have been forgotten. You're told, rather than
left with a goroutine nothing reports.

## Shut down with Ctrl-C

Press `Ctrl-C`. The controller stops services in reverse registration order, so
the dispatcher's context is cancelled first and it stops attaching. Then the
supervisor's `Stop` runs, which cancels every remaining session *at once* and
waits for all of them, bounded by the shutdown timeout:

```text
tenant-4 disconnected
tenant-5 disconnected
shutdown complete
```

Concurrent, not one at a time, so those two lines can arrive in either order:
ten sessions taking 100ms each would stop in about 100ms. The price is that children must not depend on each other. If yours
do, that ordering is what a `Controller` is for.

## The complete program

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gitlab.com/phpboyscout/go/controls"
)

func main() {
	c := controls.NewController(context.Background(), controls.WithSignals())

	sup := controls.NewSupervisor(
		controls.WithOnFailure(func(f controls.Failure) {
			fmt.Println("FAILED:", f.Name, "->", f.Err)
		}),
	)

	c.Register("tenants",
		controls.WithStart(sup.Start),
		controls.WithStop(sup.Stop),
		controls.WithReadiness(sup.Readiness),
	)
	_ = c.RegisterHealthCheck(sup.HealthCheck("tenant-sessions"))

	c.Register("dispatcher",
		controls.WithStart(func(ctx context.Context) error {
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()

			for n := 1; ; n++ {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-ticker.C:
					name := fmt.Sprintf("tenant-%d", n)
					if err := sup.Attach(controls.Child{Name: name, Start: session(name, n == 3)}); err != nil {
						fmt.Println("attach", name, "refused:", err)
					}

					if n > 2 {
						leaving := fmt.Sprintf("tenant-%d", n-2)

						dctx, cancel := context.WithTimeout(ctx, time.Second)
						err := sup.Detach(dctx, leaving)
						cancel()

						fmt.Println("detached", leaving, "->", err)
					}

					ready := c.Readiness().OverallHealthy

					check, _ := c.GetCheckResult("tenant-sessions")
					verdict := check.Message
					if verdict == "" {
						verdict = "ok"
					}

					fmt.Println("health:", states(sup), "| process ready:", ready, "| check:", verdict)
				}
			}
		}),
	)

	c.Start()
	c.Wait()

	fmt.Println("shutdown complete")
}

func session(name string, broken bool) controls.StartFunc {
	return func(ctx context.Context) error {
		if broken {
			return errors.New("no such tenant")
		}

		fmt.Println(name, "connected")
		<-ctx.Done()
		fmt.Println(name, "disconnected")

		return ctx.Err()
	}
}

func states(sup *controls.Supervisor) string {
	h := sup.Health()

	names := make([]string, 0, len(h))
	for n := range h {
		names = append(names, n)
	}

	sort.Strings(names)

	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, n+"="+string(h[n].State))
	}

	return strings.Join(parts, " ")
}
```

## Success criterion

Run it for about ten seconds, then press `Ctrl-C`. You should see tenants
connect every two seconds, `tenant-3` fail with `process ready: true` on the
same line, tenants disconnect as they're detached, the check return to `ok`
once the failed tenant is gone, and `shutdown complete` after the remaining
sessions disconnect. If shutdown hangs, a session is not returning when its
context is cancelled; the supervisor abandons it at the deadline, but your
`Ctrl-C` will feel slow.

## Next steps

- Give tenants a restart policy, and see what a `Failure` carries when one
  finally gives up: [Supervise children that come and go](../how-to/supervise-dynamic-children.md).
- Understand why a failed child never makes the process unready:
  [Registered means required](../explanation/supervision.md).
- Look up what every `Supervisor` call does when it arrives out of order:
  [Supervisor reference](../reference/supervisor.md).
- If a service of yours *owns* a supervisor and can be restarted, it needs a
  fresh one per run: [Survive a restart](../how-to/survive-a-restart.md).

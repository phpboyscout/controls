# Register and run services

This guide covers registering one or more services, the ordering guarantees
during startup and shutdown, and what happens when you omit a lifecycle
callback.

## Register a single service

A service is a unique name plus lifecycle callbacks supplied as
`ServiceOption`s. The common shape wraps a real server:

```go
c := controls.NewController(context.Background())

srv := &http.Server{Addr: ":8080", Handler: mux}

c.Register("http-api",
	controls.WithStart(func(ctx context.Context) error {
		// ListenAndServe blocks until the server is closed. Report
		// ErrServerClosed as an expected terminal error, not a failure.
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}),
	controls.WithStop(func(ctx context.Context) {
		// ctx is bounded by the shutdown timeout; Shutdown drains in-flight
		// requests until it elapses.
		_ = srv.Shutdown(ctx)
	}),
)
```

> **Blocking vs. background starts.** A `WithStart` may either block for the
> service's whole lifetime (like `ListenAndServe` above) or spawn a goroutine and
> return `nil` immediately. Both are supported. A `nil` return is treated as a
> *clean start*, not an exit: the supervisor keeps the service alive until
> shutdown. See [The restart supervisor](../explanation/restart-supervisor.md).

> **This `srv` is captured once, so it cannot survive a restart.** With no
> restart policy that is fine, and it is the common case. Under a policy, a
> second run would hand a fresh listener to a server that `Shutdown` has already
> closed. [Survive a restart](survive-a-restart.md) is the recipe for that.

## Give every service a distinct name

`Register` takes no error return and does not check names for uniqueness.
Registering two services as `worker` is accepted: both run, both appear in every
health report as separate entries named `worker`, and `GetServiceInfo("worker")`
can only return one of them, the one registered last. Nothing warns you. Use
distinct names.

The same applies across kinds: a standalone `HealthCheck` may share a name with a
service, because `RegisterHealthCheck` only checks for collisions among health
checks.

## Register several services

Call `Register` once per service, all before `Start`:

```go
c.Register("database", controls.WithStart(openDB), controls.WithStop(closeDB))
c.Register("http-api", controls.WithStart(serveHTTP), controls.WithStop(stopHTTP))
c.Register("worker", controls.WithStart(runWorker))

c.Start()
c.Wait()
```

## Startup and shutdown ordering

- **Startup** launches a supervisor goroutine per service in **registration
  order**. Services start concurrently: the controller does not wait for one
  `WithStart` to return before launching the next, which is what lets a blocking
  server and a background worker coexist.
- **Shutdown** runs each `WithStop` in **reverse registration order**, one at a
  time. Registering `database` first and `http-api` second means the HTTP server
  is stopped *before* the database on the way down. Dependencies you bring up
  first are torn down last.

> **Ordering is by registration, not by readiness.** The controller does not
> model inter-service dependencies or block a start until a dependency is
> "ready". If service B must not begin work until service A is up, gate that
> inside B's `WithStart`, for example by having it wait on a channel A closes.

## Omitting Start or Stop

Both callbacks are optional. A service registered without a `WithStart` defaults
to a no-op that returns `nil`; one without a `WithStop` defaults to a no-op stop.
Neither ever panics.

```go
// A marker service that only reports health: no start/stop behaviour.
c.Register("readiness-gate",
	controls.WithReadiness(func() error { return checkDependencies() }),
)
```

This is useful for a service whose only job is to contribute a probe to the
aggregate [health report](health-checks.md), or as a placeholder during
incremental development.

## What happens if you register after Start

`Register` still accepts the service, logs
`WARN "Register called after Start; service will not be supervised"`, and adds it
to the collection. But `Start` has already snapshotted the service set and
launched the supervisor goroutines, so that service's `StartFunc` is never
called, its `StopFunc` never runs, and no restart policy applies to it.

The trap is that it *does* appear in `Status()`, `Liveness()` and `Readiness()`,
reporting `"OK"` if it has no probe. A late registration therefore looks healthy
while doing nothing at all. Treat the warning as a bug in your wiring rather than
a caution.

**If you genuinely need a set that changes while the process runs**, that is what
a [`Supervisor`](supervise-dynamic-children.md) is for. A `Controller` manages a
fixed set with ordered shutdown; a `Supervisor` manages a changing one. They are
two sides of one coin and a process usually wants both.

## Inspect a running service

`GetServiceInfo` returns runtime metadata for a registered service: its restart
count, last start and stop times, last error, and how its last stop ended.

```go
info, ok := c.GetServiceInfo("worker")
if ok {
	fmt.Printf("restarts=%d lastErr=%v\n", info.RestartCount, info.Error)
}
```

## Related

- [Add health checks](health-checks.md): attach probes and standalone checks.
- [Services and restart policy reference](../reference/services.md): every
  option, the callback contracts, and every `RestartPolicy` field.
- [Configure restart policy](restart-policy.md): make a service self-healing.
- [Architecture](../explanation/architecture.md): how the supervisor goroutines
  and the state machine fit together.

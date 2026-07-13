# Register & run services

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
> *clean start*, not an exit — the supervisor keeps the service alive until
> shutdown. See [The restart supervisor](../explanation/restart-supervisor.md).

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
  order**. Services start concurrently — the controller does not wait for one
  `WithStart` to return before launching the next (that is what lets a blocking
  server and a background worker coexist).
- **Shutdown** runs each `WithStop` in **reverse registration order**, one at a
  time. Registering `database` first and `http-api` second means the HTTP server
  is stopped *before* the database on the way down — dependencies you bring up
  first are torn down last.

> **Ordering is by registration, not by readiness.** The controller does not
> model inter-service dependencies or block a start until a dependency is
> "ready". If service B must not begin work until service A is up, gate that
> inside B's `WithStart` (for example, by having it wait on a channel A closes).

## Omitting Start or Stop

Both callbacks are optional. A service registered without a `WithStart` defaults
to a no-op that returns `nil`; one without a `WithStop` defaults to a no-op stop.
Neither ever panics.

```go
// A marker service that only reports health — no start/stop behaviour.
c.Register("readiness-gate",
	controls.WithReadiness(func() error { return checkDependencies() }),
)
```

This is useful for a service whose only job is to contribute a probe to the
aggregate [health report](health-checks.md), or as a placeholder during
incremental development.

## Inspect a running service

`GetServiceInfo` returns runtime metadata for a registered service — its restart
count, last start/stop times, and last error:

```go
info, ok := c.GetServiceInfo("worker")
if ok {
	fmt.Printf("restarts=%d lastErr=%v\n", info.RestartCount, info.Error)
}
```

## Related

- [Add health checks](health-checks.md) — attach probes and standalone checks.
- [Configure restart policy](restart-policy.md) — make a service self-healing.
- [Architecture](../explanation/architecture.md) — how the supervisor goroutines
  and the state machine fit together.

# Configure restart policy

By default a service is **not** restarted: if its `WithStart` returns an error,
the controller records it and moves on. To make a service self-healing, attach a
`RestartPolicy` with `WithRestartPolicy`.

## Enable restarts

```go
c.Register("worker",
	controls.WithStart(runWorker),
	controls.WithRestartPolicy(controls.RestartPolicy{
		MaxRestarts:    5,
		InitialBackoff: time.Second,
		MaxBackoff:     30 * time.Second,
	}),
)
```

Now, if `runWorker` returns a genuine error while the controller context is
still live, the supervisor waits a backoff interval and starts it again — up to
`MaxRestarts` consecutive failures.

## The policy fields

```go
type RestartPolicy struct {
	MaxRestarts            int           // cap on consecutive failures (0 = unlimited)
	InitialBackoff         time.Duration // first wait before restart (default 1s)
	MaxBackoff             time.Duration // backoff ceiling (default 30s)
	HealthFailureThreshold int           // consecutive Status failures before restart
	HealthCheckInterval    time.Duration // how often Status is polled (default 10s)
	RestartResetInterval   time.Duration // healthy run length that resets the counter (default 30s)
}
```

- **`InitialBackoff` / `MaxBackoff`.** The wait before each restart doubles from
  `InitialBackoff`, capped at `MaxBackoff`. A zero field selects its default
  (1s / 30s).
- **`MaxRestarts`.** Caps **consecutive** failures (see below), not lifetime
  restarts. It counts *restarts*, so `MaxRestarts: 5` lets the service run six
  times in all. When exceeded, the supervisor gives up on the service and
  forwards a `max restarts exceeded` error — wrapping the last `StartFunc` error
  when there was one. `0` means unlimited.
- **The controller keeps running either way.** Giving up on a service does not
  stop the process or the other services; the error is recorded on
  `ServiceInfo.Error`, forwarded on the error channel and logged. If a dead
  service should take the process down, watch for it and call `Stop()` yourself.

## Health-driven restarts

A service whose `WithStart` returns `nil` immediately (it serves in a background
goroutine) never "exits", so there is no error to restart on. To supervise such
a service, combine a `WithStatus` probe with a `HealthFailureThreshold`:

```go
c.Register("streamer",
	controls.WithStart(startStreamerInBackground), // returns nil once serving
	controls.WithStatus(func() error {
		return streamer.Ping() // non-nil == unhealthy
	}),
	controls.WithRestartPolicy(controls.RestartPolicy{
		HealthFailureThreshold: 3,               // 3 consecutive bad pings...
		HealthCheckInterval:    5 * time.Second, // ...polled every 5s
		MaxRestarts:            10,
	}),
)
```

The supervisor polls `Status` every `HealthCheckInterval`. Each failure
increments a counter; a single success resets it. When the counter reaches
`HealthFailureThreshold`, the service is stopped and restarted (subject to
`MaxRestarts` and backoff).

> **`HealthFailureThreshold` requires a `Status` probe.** Health monitoring only
> runs when both `HealthFailureThreshold > 0` and a `WithStatus` func are
> present. Without them, a clean-start service simply runs until shutdown.

> **Which restart path calls `WithStop`.** A health-threshold restart stops the
> service first: `WithStop` runs before each restart, and again at shutdown, so
> it must be idempotent. An **error**-triggered restart does not call `WithStop`
> at all — the `StartFunc` has already returned, and the supervisor goes straight
> to the backoff wait. Release resources on the way out of your `StartFunc` if
> the error path needs cleaning up.

## Consecutive failures and the reset window

`MaxRestarts` counts **consecutive** failures, not the lifetime total. A service
that fails, restarts, and then runs healthily for `RestartResetInterval` has its
counter reset to zero — so a service that flakes once an hour is never
permanently killed by an old failure.

The reset window defaults to 30s (`DefaultRestartResetInterval`). Tune it with
the policy field, or with `WithRestartResetInterval`:

```go
c.Register("flaky",
	controls.WithStart(run),
	controls.WithRestartPolicy(controls.RestartPolicy{MaxRestarts: 3}),
	controls.WithRestartResetInterval(2*time.Minute), // must run 2m clean to reset
)
```

> **`WithRestartResetInterval` implies a policy.** If the service has no
> `RestartPolicy` yet, this option creates a default one so the interval takes
> effect. Order does not matter — apply it before or after `WithRestartPolicy`.

## Exempt expected terminal errors

Some errors are a *normal* end of run, not a failure — the canonical example is
`http.ErrServerClosed`, returned by `ListenAndServe` after a graceful
`Shutdown`. Register a controller-wide predicate with `WithValidError` so the
supervisor treats a matching error as a clean stop: it is neither counted toward
the restart total nor forwarded on the error channel.

```go
c := controls.NewController(ctx,
	controls.WithValidError(func(err error) bool {
		return errors.Is(err, http.ErrServerClosed) ||
			errors.Is(err, context.Canceled)
	}),
)
```

`WithValidError` is a **controller** option (passed to `NewController`), and the
predicate applies to every service. Contrast it with the per-service restart
options above, which are `ServiceOption`s passed to `Register`.

## Observe restart counts

```go
info, _ := c.GetServiceInfo("worker")
fmt.Printf("restarts=%d lastErr=%v\n", info.RestartCount, info.Error)
```

## Related

- [The restart supervisor](../explanation/restart-supervisor.md) — the run-outcome
  classification and why a clean start is never restarted.
- [Services and restart policy reference](../reference/services.md) — every field
  with its default, and what a zero value selects.
- [Add health checks](health-checks.md) — the `WithStatus` probe the health
  threshold depends on.

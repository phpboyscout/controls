# The restart supervisor

The restart supervisor is the part of the controller that decides whether — and
when — to restart a failing service. Its behaviour hinges on one idea: **not
every return from `Start` is a failure**. This page explains how a run is
classified, why a clean start is never restarted, and how backoff and the reset
window work.

## Run-outcome classification

Every time a service's `WithStart` returns, the supervisor classifies the run
into exactly one of three outcomes:

| Outcome | When | Restarted? | Forwarded as error? |
|---|---|:---:|:---:|
| **Clean start** | `Start` returned `nil` | no | no |
| **Cancelled / valid error** | context was cancelled, or the error matched `WithValidError` | no | no |
| **Error** | non-nil, non-valid error while the context was live | yes (if a policy allows) | yes |

Only the third outcome — a genuine error — is restart-worthy. The other two are
normal ends of run.

### Why a clean start is never restarted

This is the subtlety that a naive supervisor gets wrong. Consider an HTTP server
that spawns its listener in a background goroutine and returns `nil`:

```go
controls.WithStart(func(ctx context.Context) error {
	go srv.ListenAndServe() // serves in the background
	return nil              // "started", not "exited"
})
```

A `nil` return here means the service **started successfully**, not that it
finished its work. Restarting it would be absurd — it is happily serving. So the
supervisor treats a clean start as "up", and supervises it one of two ways:

- if the service has a `WithStatus` probe **and** a `HealthFailureThreshold > 0`,
  the supervisor polls that probe and restarts on sustained health failure;
- otherwise it simply blocks until shutdown — the clean start *is* the supervision.

Contrast a service whose `WithStart` **blocks** for its whole lifetime (like a
bare `srv.ListenAndServe()` that returns only when the server closes). There, a
non-nil return while the context is live genuinely *is* an exit, and is
classified as an error.

### Why cancellation and valid errors are exempt

When the controller shuts down it cancels the shared context, which unblocks
`WithStart` functions — often causing them to return `context.Canceled` or a
context-derived error. Restarting a service *because the controller asked it to
stop* would be a bug, so a cancelled run never restarts.

`WithValidError` extends the same courtesy to expected terminal errors that are
not context errors — most importantly `http.ErrServerClosed`, which
`ListenAndServe` returns after a graceful `Shutdown`. A matching error is
reclassified as a clean end of run: not counted, not forwarded.

## Backoff

When a genuine error triggers a restart, the supervisor waits before trying
again, and the wait grows exponentially:

- the first wait is `InitialBackoff` (default 1s);
- each subsequent wait doubles, capped at `MaxBackoff` (default 30s).

The backoff wait is itself cancellable — if the controller shuts down mid-wait,
the supervisor abandons the restart and exits.

## Consecutive failures and the reset window

`MaxRestarts` caps **consecutive** failures, not lifetime restarts. The
distinction is what separates a genuinely broken service from a merely flaky one.

The supervisor tracks how long each run lasted. If a run survived at least
`RestartResetInterval` (default 30s) before failing, the consecutive-failure
counter is reset to zero — the service proved it can run healthily, so an old
failure should not count against it. Only failures that pile up *faster* than the
reset window accumulate toward `MaxRestarts`.

When the counter does reach `MaxRestarts`, the supervisor gives up: it records a
`max restarts exceeded` error (wrapping the last real error) and stops
supervising that service.

## The error channel contract

The supervisor forwards errors on the controller's error channel, where the
error-and-context handler logs them. Two rules keep that channel well-behaved:

- **It never sends `nil`.** A health-threshold breach records its error via the
  service's `ServiceInfo` rather than the channel, and a wrapped-`nil` is never
  forwarded — so a receiver never has to guard against a spurious `nil` error.
- **Every send is non-blocking against shutdown.** Each forward is guarded by a
  `select` on the shutdown-complete signal, so once the handler has exited there
  is no way for a late error to block the supervisor forever. This is the D9
  property discussed in [Concurrency & shutdown correctness](concurrency.md).

## Related

- [Configure restart policy](../how-to/restart-policy.md) — the practical recipe.
- [Health, liveness & readiness](health-model.md) — the `WithStatus` probe behind
  health-driven restarts.
- [Concurrency & shutdown correctness](concurrency.md) — the guarantees the
  supervisor relies on.

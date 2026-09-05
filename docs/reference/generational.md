---
title: Generational reference
description: Every Generational field, method and sentinel, the Release retry, and what each call does when it arrives out of order.
---

# Generational reference

`Generational[R]` owns one generation of `R` at a time, for a service that can
be stopped and started again in-process. It exists because a restart calls the
same `StartFunc` closure again, so anything that closure captured is shared
between runs. For when to reach for it, see
[Survive a restart](../how-to/survive-a-restart.md); for why it is shaped this
way, see
[What a restart shares](../explanation/restart-supervisor.md#what-a-restart-shares-and-what-it-must-not).

## The lifecycle, and every out-of-order call

A `Generational` is in one of three conditions: **no generation** (before the
first `Start`, and after a `Stop` has released), **live** (a `Start` succeeded
and `Stop` has not been called), or **releasing** (`Stop` has closed admission
and `Release` has not yet returned `nil`). Every call has an answer in each.

| Call | No generation | Live | Releasing |
|---|---|---|---|
| `Start` | builds and installs generation *n+1* | `ErrGenerationRunning` | waits within `ctx` for the release, then returns `ErrPredecessorLive` if it has not finished |
| `Use` | `ErrNoGeneration` | runs `fn` under a lease | `ErrNoGeneration` |
| `Healthy` | `ErrNoGeneration` | `Probe(value)`, or `nil` with no `Probe` | `ErrNoGeneration` |
| `Generation` | `0` | *n* | `0` |
| `Stop` | `nil`, nothing to do | closes admission, drains, releases | `nil` at once; it does not wait for the release in flight |

Two `Start` calls racing each other build two values and install one. The
loser releases what it built **synchronously**, on the caller's goroutine, and
returns `ErrGenerationRunning`, joined with the release error if that failed
too. A `Start` that fails leaves nothing behind, which is why the loser does not
hand its value to the background disposer: nothing would ever wait for it.

A `Build` that returns an error installs nothing and the error is returned
unchanged. `Generation` still reads `0`, and the next `Start` tries again.

## Fields

```go
type Generational[R any] struct {
    Build          func(ctx context.Context) (R, error)
    Release        func(ctx context.Context, r R) error
    Probe          func(r R) error
    ReleaseAttempt time.Duration
}
```

| Field | Required | Contract |
|---|---|---|
| `Build` | yes | Called once per `Start`, outside any lock, with the `Start` caller's context. Everything it acquires that needs releasing must be reachable from the `R` it returns, or it leaks. That obligation is the whole contract this type places on a consumer. |
| `Release` | yes | Called once per generation, with a context bounded to `ReleaseAttempt` per attempt (see below). It must be **idempotent**, and it must be **safe to run beside a lease that outlived the stop budget**, because such a lease is disowned rather than waited for. A `nil` return means every resource is gone, and that is the only thing that permits the next `Start`. |
| `Probe` | no | Called by `Healthy` with the live value. `nil` means always healthy while a generation is live. |
| `ReleaseAttempt` | no | The budget each call to `Release` gets. Zero **or negative** selects 250ms: zero because that is what an omitted field holds, negative because an already-expired context is one `Release` could never satisfy, and every later `Start` would then be refused. Set it when `Release` legitimately needs longer, a TLS close over a slow link or a client library with its own drain, rather than making `Release` return early. |

The zero value is not usable: a nil `Build` or `Release` is called and panics.
The type is safe for concurrent use. `Use` is the hot path and takes no mutex;
the mutex that guards start and stop is never held across `Build` or `Release`.

## Methods

```go
func (g *Generational[R]) Start(ctx context.Context) error
func (g *Generational[R]) Stop(ctx context.Context) error
func (g *Generational[R]) Use(fn func(R) error) error
func (g *Generational[R]) Healthy() error
func (g *Generational[R]) Generation() uint64
```

`Start` is a `StartFunc`, `Stop` is a `StopErrFunc` and `Healthy` is a
`StatusFunc`, so the three register with a `Controller` directly:

```go
controller.Register("api",
    controls.WithStart(g.Start),
    controls.WithStopErr(g.Stop),
    controls.WithStatus(g.Healthy),
)
```

`Start` returns once the generation is installed, so to the controller's restart
supervisor it is a **clean start** that serves in the background. Under a
`RestartPolicy` that means a `Build` error, `ErrPredecessorLive` or a lost race
is a failure that the policy restarts with backoff, and a `WithStatus(g.Healthy)`
probe with a `HealthFailureThreshold` is what triggers a restart of a live
generation whose `Probe` fails.

### What `Stop` does, in order

1. Swaps the live generation out, so `Use`, `Healthy` and `Generation` see no
   generation from this instant. A concurrent `Start` sees the predecessor and
   waits for step 4.
2. Closes admission, **before** draining, so a caller using the generation
   continuously cannot starve the drain.
3. Waits for outstanding leases within `ctx`. A lease still held at the deadline
   is **disowned**: its callback keeps running, but it can no longer reach the
   generation once step 4 completes, and nothing waits for it. Go cannot end a
   goroutine, so this is the honest bound rather than a hanging one.
4. Hands the generation to the disposer, which calls `Release` and retries until
   it returns `nil`.
5. Returns `nil` when `Release` has succeeded, or `ErrStopTimeout` when `ctx`
   expires first. On `ErrStopTimeout` the generation is still being released in
   the background, and the next `Start` waits for that.

`Stop` is idempotent. A second `Stop` finds no live generation and returns
`nil` immediately, without waiting for the first call's release; a `Start` after
it does wait.

### `Use` holds a lease

`Use` loads the live generation, takes a lease, re-checks that admission is
still open, and runs `fn`. The re-check is what a `WaitGroup` cannot express: a
lease acquired an instant before `Stop` shut the gate would otherwise run
against a generation already being released. `fn`'s error is returned as-is.

There is deliberately no method that returns `R` for a caller to keep. A
retained handle with no staleness protection is the defect this type exists to
prevent.

## The `Release` retry

| Path | Budget per attempt | Attempts | Context |
|---|---|---|---|
| `Stop` (the disposer) | `ReleaseAttempt`, 250ms when zero | unbounded, until `Release` returns `nil` | detached from the caller's: values survive for tracing and logging, cancellation does not |
| a `Start` that lost a race | `ReleaseAttempt`, 250ms when zero | 4, then the error is returned wrapped | the losing `Start`'s own context, likewise detached |

The disposer never abandons, on purpose. An unreleased generation left behind is
how a resource the next generation will duplicate stays alive, and a duplicate
is worse than being unavailable. So a `Release` that ignores its context does
not leak quietly: it blocks the next `Start` with `ErrPredecessorLive`, loudly,
and that is a bug in the release path to fix rather than something to work
around. A `Release` that needs longer than 250ms per attempt sets
`ReleaseAttempt`; a cap on the retry is a decision to leak, and nothing here
makes it.

The synchronous path is bounded where the disposer is not because it runs on
the caller's goroutine: retrying for ever there would hang `Start` rather than
merely refuse a later one, and the caller already has an error to act on. Its
ceiling is four times `ReleaseAttempt`, so a consumer that declares a long
budget accepts a longer worst case on a lost race, which takes two concurrent
`Start` calls to happen at all.

## Sentinel errors

| Sentinel | Kind | Returned by | When |
|---|---|---|---|
| `ErrNoGeneration` | `controls.no_generation` | `Use`, `Healthy` | no generation is live: before the first `Start`, during a stop, and after one. One error rather than three because the caller's response is the same |
| `ErrGenerationRunning` | `controls.generation_running` | `Start` | a generation is already live, or another `Start` won the race |
| `ErrPredecessorLive` | `controls.predecessor_live` | `Start` | the previous generation had not finished releasing within `ctx`. Never caused by a lease, which is bounded by its own call; only a `Release` that ignores its context can hold it |
| `ErrStopTimeout` | `controls.stop_timeout` | `Stop` | the budget expired before `Release` returned `nil`. The release continues in the background and a later `Start` waits for it |

All four are `errors.NewSentinel` values, so `errors.Is` matches them and the
kind survives a process boundary.

## Related

- [Survive a restart](../how-to/survive-a-restart.md): the recipe.
- [The restart supervisor](../explanation/restart-supervisor.md#what-a-restart-shares-and-what-it-must-not):
  the two failures this type exists to prevent.
- [Services and restart policy](services.md#stoperrfunc-funcctx-contextcontext-error):
  what the controller does with the error `Stop` returns.
- [Defaults and timings](defaults.md#generational-defaults).

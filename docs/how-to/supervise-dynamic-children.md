---
title: Supervise children that come and go
description: A Supervisor runs units that attach and detach while the process is running, under the same restart policy a registered service uses.
---

# Supervise children that come and go

A `Controller` manages a **fixed** set, registered before `Start`, stopped in
reverse registration order. A `Supervisor` manages a set that **changes**. Two
sides of one coin, and a process usually wants both.

Reach for a `Supervisor` when the units are not knowable at wiring: a worker per
tenant as tenants arrive, a subscription per topic, a pool that grows under load.

## Wire one

A `Supervisor` is itself a service, so it registers like anything else:

```go
sup := controls.NewSupervisor(
    controls.WithOnFailure(func(f controls.Failure) {
        logger.Error("supervised child failed",
            "child", f.Name, "restarts", f.Restarts, "panicked", f.Panicked, "error", f.Err)
    }),
)

controller.Register("workers",
    controls.WithStart(sup.Start),
    controls.WithStop(sup.Stop),
    controls.WithReadiness(sup.Readiness),
)

// A distinct name: RegisterHealthCheck checks a name only against other health
// checks, so a check sharing a name with the service above would be accepted
// and the report would carry both entries under one name.
controller.RegisterHealthCheck(sup.HealthCheck("workers-children"))
```

## Attach and detach

```go
err := sup.Attach(controls.Child{
    Name:  "tenant-01JQ",
    Start: func(ctx context.Context) error { return worker.Run(ctx) },
    RestartPolicy: &controls.RestartPolicy{
        MaxRestarts:    5,
        InitialBackoff: time.Second,
        MaxBackoff:     30 * time.Second,
    },
})
```

Attaching **after** `Start` is the point: the child is supervised identically
whenever it arrives, which is exactly what a `Controller` declines to do.

```go
ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
defer cancel()

if err := sup.Detach(ctx, "tenant-01JQ"); errors.Is(err, controls.ErrDetachTimeout) {
    logger.Warn("child outlived its detach budget and is still running")
}
```

`Detach` waits, bounded by your context. If the budget expires the child is
forgotten anyway and you are **told**. The alternative is a goroutine still
running that nothing reports, which is the one shape this API does not offer.

## The restart policy is the one you already know

`RestartPolicy` is the same type a registered service uses, read through the
same helpers, including that `MaxRestarts <= 0` is **unlimited**, not none. A
child with a `nil` policy runs once and its outcome is final. The value is
copied at `Attach`, so you may reuse or edit your policy struct afterwards.

Two fields are inert on a child. `HealthFailureThreshold` and
`HealthCheckInterval` drive a service's health-based restarts through its
`Status` probe, and a child has no probe to read. Setting them changes nothing.

## What counts as a failure

A child's `Start` returning is classified by the same rule a service's is
(`classifyOutcome`, shared):

| It returns | The supervisor does |
|---|---|
| `nil` | treats the child as finished. It is not restarted, and reports `stopped`. **This differs from a `Service`**, where a `nil` return means "still serving in the background" |
| `context.Canceled`, or anything at all once the supervisor's context is cancelled | treats it as a clean cancellation. No restart, no `Failure` |
| any other error | a failure: recorded, counted against the restart policy, and restarted or reported terminal |

A panic is converted to an error and counted separately, so `Failure.Panicked`
and `ChildStatus.Panics` tell a bug from a capacity problem.

## What `Health` reports

`sup.Health()` returns a `map[string]ChildStatus`, one entry per attached child,
carrying its `State`, `Restarts`, `Panics` and `LastErr`. It is data, not
health: what to do about a failed child is your judgement, which is the whole
point of the boundary below.

| `ChildState` | Means |
|---|---|
| `pending` | attached, and the supervisor has not started it yet |
| `running` | its `Start` has been called and has not returned |
| `backoff` | it failed and is waiting out its restart delay |
| `failed` | it exhausted its restart policy. Terminal |
| `stopped` | it returned cleanly, or was cancelled |

## A failed child does not make the process unready

This is the difference that matters, and it is deliberate:

| | Registered `Service` | Attached `Child` |
|---|---|---|
| Membership | fixed, before `Start` | dynamic |
| Its probe failing | sets `OverallHealthy: false` for the **whole process** | never affects the process |
| Shutdown | reverse registration order | concurrent |
| Panic in its start function | **crashes the process** | recovered, counted, restarted |

**Registered means required.** A registered service that reports unready takes
the process out of rotation. A supervised child is not a requirement, so
`sup.Readiness()` reflects whether the *supervisor* is working and never whether
its children are. Otherwise one dead child would take the process down through
the registration above.

What you get instead is `HealthCheck` reporting `DEGRADED` while any child has
failed, visible to an operator and inert to a probe, and a `Failure` handed to
you to judge. If that child really was load-bearing, the consumer that attached
it can make **itself** unready, which is what registration is for.

## Both ways to hear about a failure

```go
// A callback, for recording it. Ordered, and never dropped.
controls.NewSupervisor(controls.WithOnFailure(func(f controls.Failure) { ... }))

// A channel, for a control loop that wants to act. Note the select: the channel
// is never closed, so a bare `range` over it does not end at shutdown.
for {
    select {
    case f := <-sup.Failures():
        if f.Panicked {
            alert(f)   // a bug, not a capacity problem
        }
        reattachWithBackoff(f.Name)
    case <-done:
        return
    }
}
```

The two shed differently, on purpose. The **channel** is opt-in and bounded: a
consumer that never calls `Failures` never has a queue filling behind it, and a
send that would block is dropped and counted, so check `sup.DroppedReports()`,
which should be zero. The **callback** is neither opt-in nor bounded, because a
consumer that registered one did not agree to lose notifications, so its queue
is unbounded and ordered. The cost is the usual one: a callback that never
returns is a queue that never drains.

A callback may call back into the supervisor, `Stop` included. Nothing is held
while it runs, and nothing joins the goroutine it runs on. The corollary is worth
saying: `Stop` returning does **not** mean every callback has finished. One still
executing runs to completion, and the goroutine ends when the queue drains.

## Out of order

A `Supervisor` is single use, and every call has a defined answer when it
arrives in the wrong order:

| Call | Before `Start` | While running | Once `Stop` has begun |
|---|---|---|---|
| `Attach` | accepted, started with the supervisor | accepted, started immediately | `ErrSupervisorStopped` |
| `Start` | starts | "already started" | `ErrSupervisorStopped` |
| `Stop` | returns at once, nothing was started | full shutdown | waits for the shutdown in flight |
| `Detach` | returns `nil`, the child never ran | stops and waits, bounded | still resolves; the child is stopping or already stopped |
| `Readiness` | `ErrSupervisorNotStarted` | `nil` | `ErrSupervisorStopped` |

`Stop` and `Detach` are bounded by the context you give them, across the child's
own goroutine **and** its `Child.Stop`. A child that outlives the budget is
abandoned rather than allowed to hold shutdown open, which is the same bargain a
`Controller` strikes at its shutdown deadline. `Health` still lists it, so an
abandoned child is visible rather than silent.

## Children must not depend on each other

`Stop` cancels every child at once. Shutdown is bounded by the slowest child
rather than by their number: ten children taking 100 ms each stop in about
100 ms, where one at a time would take a second.

If your units genuinely depend on each other, that ordering is what a
`Controller` provides deliberately. Use one, or sequence them inside a single
child.

## A supervisor cannot be restarted, so wrap it if its service can be

A `Supervisor` is single use: `Start` after `Stop` returns
`ErrSupervisorStopped` for ever. A service that owns one and runs under a
`RestartPolicy` therefore cannot capture it at wiring, or the second run gets a
supervisor that refuses everything. Build it inside the run, or hand it to a
[`Generational`](survive-a-restart.md), which builds a fresh one per generation.

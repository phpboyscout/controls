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

controller.RegisterHealthCheck(sup.HealthCheck("workers"))
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
forgotten anyway and you are **told** — the alternative is a goroutine still
running that nothing reports, which is the one shape this API does not offer.

## The restart policy is the one you already know

`RestartPolicy` is the same type a registered service uses, and means the same
things — including that `MaxRestarts <= 0` is **unlimited**, not none. A child
with a `nil` policy runs once and its outcome is final.

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
its children are — otherwise one dead child would take the process down through
the registration above.

What you get instead is `HealthCheck` reporting `DEGRADED` while any child has
failed — visible to an operator, inert to a probe — and a `Failure` handed to
you to judge. If that child really was load-bearing, the consumer that attached
it can make **itself** unready, which is what registration is for.

## Both ways to hear about a failure

```go
// A callback, for recording it.
controls.NewSupervisor(controls.WithOnFailure(func(f controls.Failure) { ... }))

// A channel, for a control loop that wants to act.
for f := range sup.Failures() {
    if f.Panicked {
        alert(f)   // a bug, not a capacity problem
    }
    reattachWithBackoff(f.Name)
}
```

The channel is created on first call and bounded, so a consumer that never asks
for one never has a queue filling behind it. Sends that would block are dropped
and counted — check `sup.DroppedReports()`, which should be zero.

## Children must not depend on each other

`Stop` cancels every child at once. Shutdown is bounded by the slowest child
rather than by their number: ten children taking 100 ms each stop in about
100 ms, where one at a time would take a second.

If your units genuinely depend on each other, that ordering is what a
`Controller` provides deliberately — use one, or sequence them inside a single
child.

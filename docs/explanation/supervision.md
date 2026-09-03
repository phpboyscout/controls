---
title: Registered means required
description: Why a Controller and a Supervisor are two sides of one coin, and why a failed child never takes the process out of rotation.
---

# Registered means required

A `Controller` manages a fixed set of services. A `Supervisor` manages a set
that changes. They share the restart machinery and they nest, and the line
between them is not really about membership at all. It is about what a failure
means. This page explains that line, and the design that follows from it. The
decisions are recorded in wiki spec
[0002](https://gitlab.com/phpboyscout/go/controls/-/wikis/specs/0002-supervision-for-children-that-come-and-go).

## Two sides of one coin

| | `Controller` | `Supervisor` |
|---|---|---|
| Membership | fixed, registered before `Start` | dynamic, attach and detach at any time |
| Ordering | starts concurrently, stops in reverse registration order | starts on attach, stops concurrently |
| Concerned with | the process's lifecycle | the execution of a changing set of workers |
| Its unit failing | the **process** reports unready | the **consumer** is told, and decides |

A process has one `Controller` and may have several `Supervisor`s, each owning
a set that changes for its own reasons: a worker per tenant, a subscription per
topic, a pool that grows under load.

The fixed set is not a limitation supervision needs. It exists because
`Controller.Start` snapshots its service count to size a wait group and launch
one supervisor goroutine per service, and nothing registered afterwards is part
of that snapshot. The `Supervisor` completes the shape the package already had:
the same restart policy, the same run classification, applied to a set that can
change.

## A supervisor is a service

A `Supervisor` has a lifecycle, so it registers with a `Controller` like
anything else: `Start` is a `StartFunc`, `Stop` is a `StopFunc` and `Readiness`
is a `ProbeFunc`. The controller supervises the supervisor; the supervisor
supervises the children that come and go. One hierarchy, each layer doing what
it is good at, and a consumer's wiring never grows a second lifecycle concept to
manage by hand.

That registration is also what makes the next section load-bearing.

## The boundary is in the health surface

A registered service whose readiness or status probe fails sets
`OverallHealthy: false` for the whole process. That is the meaning of
registering something: **registered means required**. One unready registered
service takes the process out of rotation, which is right, because the process
cannot do its job without it.

An attached child is supervised, not required. So the supervisor's own
`Readiness` **never fails because a child has failed**, at any proportion,
including all of them. If it did, a `Supervisor` registered with a `Controller`
would carry one dead child straight into `OverallHealthy` through that
registration, which is exactly the coupling attaching rather than registering
exists to avoid, arriving by the back door.

What the supervisor does instead:

- `HealthCheck(name)` returns a standalone check that reports `DEGRADED` while
  any child has terminally failed, with a message counting them. `DEGRADED`
  passes every gate, so it is visible to an operator reading `Status()` and
  inert to a readiness probe.
- The failure is handed to the consumer as a `Failure`, by callback or channel,
  to judge.

If that child really was load-bearing, the consumer that attached it makes
**itself** unready, which is what registration is for. The judgement stays with
the code that has the context to make it.

This is not a difference about stopping. A failed registered service does not
stop anything either: the controller logs the error, stays `Running`, and stops
only on `Stop()`, a signal it owns, or the parent context completing. Both types
restart under policy and neither cascades a shutdown. The distinction is entirely
about whether a failure propagates into `OverallHealthy`.

## A terminal failure is delivered, not judged

When a child exhausts its restart policy the supervisor has done everything it
was told to. Whether that child mattered is not its call, so it hands the
consumer a `Failure` (name, last error, restart count, and whether the last run
was a panic) and lets them make it.

Two mechanisms, because they serve different consumers:

- **`WithOnFailure`**, a callback, for recording: log it, count it, page someone.
- **`Failures()`**, a channel, for a control loop that already selects and wants
  to act: re-attach with different settings, detach a sibling, escalate, or make
  itself unready.

They shed differently, on purpose. The channel is bounded at sixteen and a send
that would block is dropped and counted in `DroppedReports`, because a
supervisor that blocks on an undrained notification channel has let its
reporting stall the thing it reports on. It is only created when `Failures()` is
first called, so a consumer that only wanted the callback never has a queue
quietly overflowing behind it.

The callback queue is unbounded and ordered. That was not the first design: the
review found the callback sharing the channel's sixteen-slot non-blocking queue,
and measured forty failures behind a blocked callback producing seventeen
invocations. The channel may shed *because* it is opt-in; a consumer who
registered a callback never agreed to lose anything, and a terminal failure is
rare by construction, so an unbounded queue costs little. What remains is the
callback's own contract: one that never returns is a queue that never drains.

The callback runs on a dedicated goroutine that nothing joins. That buys three
things: a slow callback cannot stall supervision, failures arrive in order, and
a callback that calls back into the supervisor cannot deadlock. The last one
includes `Stop`. An earlier version waited for the dispatch goroutine during
shutdown, so a callback calling `Stop` waited for itself, under a doc comment
promising it could not. The corollary is worth saying plainly: `Stop` returning
does not mean every callback has finished.

## A child's panic is recovered; a service's is not

A child runs on a goroutine the supervisor launched, behind a `recover` that
converts the panic into an error, counts it separately, and feeds it to the
restart policy. This is not optional: an unrecovered panic on any goroutine
ends the process, so a supervisor that launches a goroutine and leaves it
unguarded has contained nothing. It has moved where the crash comes from.

It is the same line the package already draws around every other
caller-supplied function it runs on its own goroutine: a stop callback and a
probe on the report path are recovered for the same reason. A `Service`'s
`StartFunc` is the exception, and the asymmetry is deliberate. The controller
does not launch a service's work behind a boundary of its own; a panic there is
the process's panic.

A panic is still a defect. `Failure.Panicked` and `ChildStatus.Panics` keep it
apart from an error return, because a bug and a capacity problem want different
responses and a count that conflates them hides one inside the other.

## Children stop concurrently, so they must not depend on each other

`Stop` cancels every child at once and waits for all of them, bounded by the
context it is given. Shutdown is bounded by the slowest child rather than by
their number. Measured while settling the spec: ten children each taking 100ms
to stop took 101ms concurrently and 1.003s one at a time, and a supervisor's
count is dynamic in a way a controller's fixed set is not. A bus with a hundred
subscriptions would take ten seconds sequentially.

So children must not depend on each other, and that is a property of a
`Supervisor` rather than an implicit hope. Ordering is the guarantee a
`Controller`'s reverse-registration stop exists to provide. A consumer whose
units do depend on each other wants a `Controller`, or wants them sequenced
inside one child.

## One direction, and an answer for every call

A `Supervisor` moves through new, running, stopping and stopped, and never goes
back. Every public call has a defined answer in each state; the
[reference](../reference/supervisor.md#the-lifecycle-and-every-out-of-order-call)
tabulates them. That table was added after review, because its absence was the
single root cause behind five defects that reached the merge request, none of
which needed concurrency to show: `Stop` before `Start` hung on a channel no
goroutine would close, `Detach` before `Start` burned the caller's whole budget
for a child that never ran, `Attach` after `Stop` was accepted and run against a
dead context. Each has a right answer once the state machine is written down,
and most of the answers are "refuse, and say which rule was broken".

Single use follows from the one direction. `Start` after `Stop` returns
`ErrSupervisorStopped`, which matters because a `Supervisor` registered under a
`RestartPolicy` would otherwise loop. A service that owns a supervisor and has
to survive its own restart builds a fresh one per run, which is what
[`Generational`](../how-to/survive-a-restart.md) is for, and the
[restart supervisor](restart-supervisor.md#what-a-restart-shares-and-what-it-must-not)
explains why the captured one cannot be reused.

## Related

- [Supervise children that come and go](../how-to/supervise-dynamic-children.md):
  the recipe.
- [Supervisor reference](../reference/supervisor.md): every method, option and
  out-of-order answer.
- [Health, liveness and readiness](health-model.md): the gate a `DEGRADED`
  result passes.
- [The restart supervisor](restart-supervisor.md): the classification a child
  shares with a service, and the one place it differs.

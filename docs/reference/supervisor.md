---
title: Supervisor reference
description: Every Supervisor method and option, the Child contract, the child states, and what each call does when it arrives out of order.
---

# Supervisor reference

A `Supervisor` runs children that attach and detach while the process is
running. A `Controller` runs a fixed set registered before `Start`; this is the
other side of that coin. For when to reach for which, see
[Supervise children that come and go](../how-to/supervise-dynamic-children.md).

## The lifecycle, and every out-of-order call

A `Supervisor` moves through four states in one direction and never goes back.
Every public call has a defined answer in each of them, so nothing is left to
whichever flag happened to be set.

| Call | new | running | stopping | stopped |
|---|---|---|---|---|
| `Attach` | accepted, launched at `Start` | accepted, launched now | `ErrSupervisorStopped` | `ErrSupervisorStopped` |
| `Start` | starts | `"controls: the supervisor is already started"` | `ErrSupervisorStopped` | `ErrSupervisorStopped` |
| `Stop` | returns at once, nothing was started | full shutdown, bounded by ctx | waits for the shutdown in flight, bounded by ctx | returns at once |
| `Detach` | `nil`, the child never ran | stops and waits, bounded by ctx | resolves; the child is stopping or stopped | resolves and returns `nil` |
| `Readiness` | `ErrSupervisorNotStarted` | `nil` | `ErrSupervisorStopped` | `ErrSupervisorStopped` |
| `Health` | every child, `pending` | current states | current states | last known states |

**A `Supervisor` is single use.** There is no restart. `Start` after `Stop`
returns `ErrSupervisorStopped` rather than starting a second time, which matters
because a `Supervisor` registered with a `Controller` under a restart policy
would otherwise loop.

## Constructor and options

```go
func NewSupervisor(opts ...SupervisorOption) *Supervisor
func WithOnFailure(fn func(Failure)) SupervisorOption
```

`WithOnFailure` registers a callback for a child that has exhausted its restart
policy. It runs on a dedicated goroutine, from an **unbounded ordered queue**,
behind a `recover`. So it cannot stall supervision, failures arrive in order,
and none is lost.

The callback may call back into the supervisor, `Stop` included: nothing is held
while it runs and nothing joins its goroutine. The corollary is that `Stop`
returning does not mean every callback has finished.

## Methods

```go
func (s *Supervisor) Attach(c Child) error
func (s *Supervisor) Detach(ctx context.Context, name string) error
func (s *Supervisor) Start(ctx context.Context) error
func (s *Supervisor) Stop(ctx context.Context)
func (s *Supervisor) Health() map[string]ChildStatus
func (s *Supervisor) Readiness() error
func (s *Supervisor) HealthCheck(name string) HealthCheck
func (s *Supervisor) Failures() <-chan Failure
func (s *Supervisor) DroppedReports() int64
```

`Start` is a `StartFunc`, `Stop` is a `StopFunc` and `Readiness` is a
`ProbeFunc`, so a `Supervisor` registers with a `Controller` like any other
service.

**`Stop` and `Detach` are bounded by the context you give them**, across the
child's own goroutine *and* its `Child.Stop`. A child that outlives the budget is
abandoned rather than allowed to hold shutdown open, which is the same bargain a
`Controller` strikes at its shutdown deadline. `Detach` says so, by returning
`ErrDetachTimeout`; `Stop` returns nothing, so an abandoned child shows up in
`Health` instead.

**`HealthCheck(name)` is a constructor, not a probe.** It returns a
`CheckTypeReadiness` check to register with a `Controller` alongside the
supervisor. Give it a name distinct from the service registration:
`RegisterHealthCheck` checks a name only against other health checks, so a
collision with the service is accepted and the report carries both entries
under one name.

## `Child`

```go
type Child struct {
    Name          string
    Start         StartFunc
    Stop          StopFunc
    ValidError    ValidErrorFunc
    RestartPolicy *RestartPolicy
}
```

- **`Name`** must be non-empty and unique within the supervisor. A duplicate
  returns `ErrChildAttached`.
- **`Start`** must be non-nil. Its outcome is classified by the same rule a
  service's is, with one difference: see the table below.
- **`Stop`** is optional. Cancelling the context passed to `Start` is the primary
  mechanism. It runs behind a `recover` and **exactly once**, however many of
  `Detach` and `Stop` reach the same child.
- **`ValidError`** is optional. See the table below for what it changes.
- **`RestartPolicy`** is the same type a service uses, read through the same
  helpers, and is **copied at `Attach`** so you may reuse or edit your struct
  afterwards. `nil` means never restart. `MaxRestarts <= 0` means **unlimited**,
  as it does for a service.

  Two of its fields are inert here: `HealthFailureThreshold` and
  `HealthCheckInterval` drive a service's health-based restarts through its
  `Status` probe, and a `Child` has no probe. Setting them changes nothing.

### What `Start` returning means

| It returns | The supervisor does |
|---|---|
| `nil` | the child finished. Not restarted, reports `stopped`. **A `Service` returning `nil` is still serving**; a child is not |
| `context.Canceled`, or anything once the supervisor's context is cancelled | a clean cancellation. No restart, no `Failure` |
| any other error | a failure: recorded, counted against the policy, restarted or reported terminal |
| a panic | converted to an error, counted separately in `ChildStatus.Panics` and flagged in `Failure.Panicked` |

**`ValidError` is a child's own `WithValidError`.** A predicate that accepts an
error makes it a clean stop: no restart, no `Failure`, exactly as a `Controller`'s
`WithValidError` does for a service. It is per child rather than per supervisor,
because a supervisor may hold children of very different kinds and one predicate
for all of them is the wrong unit. Nil means no error is exempt.

```go
sup.Attach(controls.Child{
    Name:  "api",
    Start: srv.Run,
    ValidError: func(err error) bool {
        return errors.Is(err, http.ErrServerClosed)
    },
})
```

## `ChildState` and `ChildStatus`

```go
type ChildStatus struct {
    State    ChildState
    Restarts int
    Panics   int
    LastErr  error
}
```

| `ChildState` | Means |
|---|---|
| `pending` | attached, and the supervisor has not started it yet |
| `running` | its `Start` has been called and has not returned |
| `backoff` | it failed and is waiting out its restart delay |
| `failed` | it exhausted its restart policy. Terminal |
| `stopped` | it returned cleanly, or was cancelled |

`Health()` is data, not health. `Readiness()` never fails because a child has
failed, at any proportion including all of them, because a `Supervisor`
registered with a `Controller` would otherwise take the whole process out of
rotation through that registration. `HealthCheck` reports `DEGRADED` instead,
which is visible to an operator and inert to a probe.

## `Failure`, and the two ways to hear about one

```go
type Failure struct {
    Name     string
    Err      error
    Restarts int
    Panicked bool
}
```

| | `WithOnFailure` callback | `Failures()` channel |
|---|---|---|
| Opt-in | at construction | on first call to `Failures()` |
| Queue | unbounded, ordered | bounded at `DefaultFailureBufferSize` (16) |
| When full | cannot be | the send is dropped and counted |
| Closed at shutdown | the goroutine ends when the queue drains | never closed |

They shed differently on purpose. The channel is opt-in, so a consumer that
asked for it accepted the job of draining it; the callback is not, so losing
notifications from it would lose something nobody agreed to lose.

`DroppedReports()` counts the **channel** alone and should be zero. Because
`Failures()` is never closed, a `for f := range sup.Failures()` loop does not end
at shutdown: select on it alongside your own done channel.

## Sentinel errors

| Sentinel | Returned by | When |
|---|---|---|
| `ErrSupervisorNotStarted` | `Readiness` | before `Start` |
| `ErrSupervisorStopped` | `Attach`, `Start`, `Readiness` | once `Stop` has begun |
| `ErrChildAttached` | `Attach` | the name is already in use |
| `ErrChildNotAttached` | `Detach` | no child under that name |
| `ErrDetachTimeout` | `Detach` | the child outlived the budget. It is forgotten either way |
| `ErrRestartsExhausted` | inside `Failure.Err`, by `errors.Is` | the child used up its restart policy; the child's own last error matches beside it |

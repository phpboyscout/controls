# Stop when a service gives up, or when the work is done

By default nothing a service does stops the controller. A failure is recorded,
logged and reported, and every other service carries on. Two opt-ins change
that, and each names the service on `Outcome` (spec 0010).

## Stop when a service gives up

```go
c := controls.NewController(ctx, controls.WithStopOnGiveUp())
```

With it, a registered service that fails for good stops the controller with
`CauseGaveUp`. That is one of three cases:

- it exhausts its restart policy;
- its `Start` fails and it has no restart policy;
- it has a `Status` and no restart policy, starts cleanly, and then dies.

The third is a background server: a go/transport HTTP or gRPC server returns
`nil` once it is listening and records a later serve failure that its `Status`
reports. Under `WithStopOnGiveUp` the controller polls that `Status` after the
clean start, the **death watch**. By default it polls every 10s and gives up
after three failures in a row, so a dead server stops the process 20 to 30
seconds after it dies:

```go
controls.WithDeathWatch(2*time.Second, 3) // poll every 2s, give up after 3
```

A zero or negative value keeps that value's default, and the option does
nothing without `WithStopOnGiveUp`. A panicking `Status` gives up at once,
unless `WithProbePanicsAsFailures` makes it count as one failure.

The death watch never calls the service's stop; shutdown does that a moment
later. It runs on the service's supervising goroutine, so a `Status` that blocks
for ever shows up on `Outcome` as `SupervisorAbandoned`.

Two kinds of death stay invisible. A service **with** a restart policy whose
`HealthFailureThreshold` is zero, or which has no `Status`, is not watched:
give it a threshold and the policy restarts it on a health breach. And a child
of a `Supervisor` never reaches the controller, by design.

## Finish when the work is done

```go
c.Register("migrate",
	controls.WithStart(runMigrations),
	controls.WithRunToCompletion(),
)
```

A service declared with `WithRunToCompletion` is a unit of work: its `Start`
blocks while the work runs, and its return says how the work ended.

| `Start` returns | The controller is live | What happens |
|---|---|---|
| `nil` | yes | completed: the controller stops with `CauseCompleted` |
| anything | no | shutdown was already under way; the cause stays whatever started it |
| any error, `context.Canceled` and a `WithValidError` match included | yes | a failure: retried under a restart policy, and when it fails for good the controller stops with `CauseGaveUp` |

Declaring completion is also the service's opt-in to giving up, with or without
`WithStopOnGiveUp`, so a job that can never finish does not hold the process up.
A completed service is not health-monitored, so its policy's health fields do
nothing. A declared service with no `WithStart` completes at once.

With several declared services, `WithCompletion` picks which completion ends the
controller:

```go
controls.WithCompletion(controls.CompleteOnAll) // wait for every declared service
```

`CompleteOnFirst`, the default, stops on the first. `CompleteOnAll` stops when
the last one still outstanding completes, counting the declared services
registered before `Start`. A declared service that fails for good meanwhile
stops the controller with `CauseGaveUp`. A service that completed early is
stopped in its turn at shutdown, and its probes still count towards the
controller's reports.

## Turn the outcome into an exit code

The process exit code is yours to choose. Decide on `Cause` before reading
`Err`: under `CauseGaveUp`, `Err` is the failure that gave up, and it can be
`context.Canceled`.

```go
<-c.Done()

o, _ := c.Outcome()

switch o.Cause {
case controls.CauseCompleted:
	os.Exit(0)
case controls.CauseGaveUp:
	log.Printf("%s gave up: %v", o.Service, o.Err)
	os.Exit(1)
}
```

The terminal `ServiceEvent` is published before the stop, so a `WithOnEvent`
callback or an `Errors()` reader that keeps up receives it before `Done`. If
another trigger got there first, a `Stop()` or a signal, its cause stands and the
give-up only adds its event.

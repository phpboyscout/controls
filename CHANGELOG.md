# Changelog

## [v0.8.3](https://gitlab.com/phpboyscout/go/controls/-/releases/v0.8.3)

[Compare to previous version](https://gitlab.com/phpboyscout/go/controls/-/compare/v0.8.2...v0.8.3)

### Notes

- Generational.Stop no longer reports ErrStopTimeout when the release finished by the time it reported, which it did
  at random when the budget expired with a lease held and Release then returned quickly. It returns nil in that case.
  ErrStopTimeout still means the release was not finished: a counter keyed on it now counts only those.

### Bug Fixes

- report a finished release as nil, not ErrStopTimeout ([10dd6da](https://gitlab.com/phpboyscout/go/controls/-/commit/10dd6da8d3c00d88b3f8f81e30140b9f21cfa8d0))

## [v0.8.2](https://gitlab.com/phpboyscout/go/controls/-/releases/v0.8.2)

[Compare to previous version](https://gitlab.com/phpboyscout/go/controls/-/compare/v0.8.1...v0.8.2)

### Notes

- A clean shutdown no longer logs at warn. "Stopping Services" and the first "received signal" are now info, and
  "Stopping Services" carries a cause attribute (stop, signal, parent, gave_up or completed). An alert keyed on those
  warn lines should read Outcome.Cause, or that attribute, instead.

### Bug Fixes

- log a clean shutdown at info, not warn ([70aca5b](https://gitlab.com/phpboyscout/go/controls/-/commit/70aca5b56cc1d74203840b4d5dfaf09cf452a75c))

## [v0.8.1](https://gitlab.com/phpboyscout/go/controls/-/releases/v0.8.1)

[Compare to previous version](https://gitlab.com/phpboyscout/go/controls/-/compare/v0.8.0...v0.8.1)

### Other

- deliver a Supervisor's failures through the shared forwarder ([818afec](https://gitlab.com/phpboyscout/go/controls/-/commit/818afecae44bb1bd745477eb5c913fe0936d8fb6))

## [v0.8.0](https://gitlab.com/phpboyscout/go/controls/-/releases/v0.8.0)

[Compare to previous version](https://gitlab.com/phpboyscout/go/controls/-/compare/v0.7.1...v0.8.0)

### Notes

- Two opt-ins let a service's outcome stop the controller and name itself on Outcome:

  - WithStopOnGiveUp stops the controller with CauseGaveUp when a service fails for good: it exhausts its restart
    policy, its start fails with no policy, or it dies after a clean start. Outcome.Service names it and Outcome.Err
    carries its error. A service with a Status and no restart policy is then polled for that death, by default every
    10s with three failures in a row ending the process, which is how a go/transport HTTP or gRPC server that loses
    its listener now stops it, 20 to 30 seconds after it dies. WithDeathWatch(interval, threshold) changes both.
  - WithRunToCompletion declares that a service's Start returning nil means its work is done. The controller then stops
    with CauseCompleted. Any error return, context.Canceled included, is a failure, under the restart policy if it has
    one, and a completing service that fails for good stops the controller with CauseGaveUp, with or without
    WithStopOnGiveUp. With several declared, WithCompletion(CompleteOnAll) waits for all of them; the default ends
    on the first.

  Under CauseGaveUp, Outcome.Err can be context.Canceled: decide on Outcome.Cause first.

  Nothing changes in behaviour for a controller that sets none of these. Service gains an unexported field, so a
  Service struct literal written positionally, without field names, no longer compiles; use field names.
  EventFailed now also covers a service with no restart policy that died after starting cleanly.

- A panic in a registered service's start function, in the Status probe a restart policy polls, or in a standalone
  HealthCheck is now recovered instead of crashing the process. A panicking start is that service's failure, handled by
  its restart policy like an error, so a service with no policy becomes UnableToStart. A polled Status panic is a health
  breach at once, whatever the threshold; WithProbePanicsAsFailures makes it count as one failure instead. A panicking
  HealthCheck reports unhealthy with "health check panicked: …", and so does a nil HealthCheck.Check, which crashed.

  Every recovered panic, including those in stop callbacks, probes and Supervisor children that were already
  recovered, now satisfies errors.Is(err, controls.ErrPanicked), and errors.OriginStack on it names the line that
  panicked. Their messages are unchanged. A panic is never treated as a valid terminal error, so a ValidErrorFunc no
  longer sees one, a child's included.

  WithOnPanic (on the controller) and WithOnChildPanic (on a Supervisor) hand each recovered panic, with its raw value,
  to a callback for an error tracker. The controller's runs from Start until shutdown's cutoff; while a callback is
  busy, repeats at one site are merged into the latest with a count.

  This changes a default: a service whose start panics no longer takes the process down with it. With no restart
  policy it becomes UnableToStart and the process stays up, unready. A deployment that relied on the crash for a
  restart sets WithStopOnGiveUp, and maps CauseGaveUp to a non-zero exit.

  A restart policy exhausted by health breaches now ends with its last health error, "max restarts exceeded: health
  check failed: …", where it was the bare "max restarts exceeded".

- Adds `Outcome`, `Done` and `WithOnEvent`, and changes how `Errors()` behaves:

  - The controller no longer reads the error channel. A consumer reading
    Errors() gets every failure decided before shutdown and after its first
    call, with a waiting retry replaced by a newer one, as long as it reads
    until the channel closes. Calling Errors() subscribes: failures before
    the first call are not delivered, so a consumer that must see a boot
    failure calls it before Start or uses WithOnEvent. A reader that stops
    early delays shutdown by up to the remaining budget whenever errors are
    queued for it.
  - The controller's own error channel is closed when it is done; a
    receive after that returns nil, false. A channel installed with
    SetErrorsChannel is not closed, nothing is sent on it after Done, and a
    reader of a buffered one takes what is still buffered after Done.
  - Wait and WaitContext can return later: shutdown now includes
    delivering queued events, up to the remaining budget when a consumer is
    slow.
  - A WithOnEvent callback can run after Stopped, and one in progress at
    the deadline can outlive Done.
  - Supervisors no longer block on an undrained channel installed with
    SetErrorsChannel; the forwarder holds the backlog instead.
  - Sending on the channel Errors() returns now blocks, since nothing in
    the controller reads it, and panics once it is closed.
  - Health-breach retries now arrive on the error channel, where they were
    absent.
  - A consumer using SetErrorsChannel now gets the controller's logging
    too, and may log each failure twice.
  - The controller's failure log line changes from "control error" to one
    naming the kind, with service_name and kind attributes.
  - A health breach noticed after shutdown begins is no longer reported as
    a retry or an exhaustion.
  - Health probes are called outside the services mutex, so two reports
    built at once may call a probe concurrently, and a slow probe no longer
    blocks shutdown. A probe must be safe to call concurrently with itself.
  - The shutdown budget starts when shutdown is triggered, not when the
    stop message is processed.
  - ServiceInfo.StopErr changes in three ways: it holds ErrStopAbandoned
    for a stop abandoned at the deadline or never awaited because the
    budget had gone, where it held nil; a stop that
    returns after being abandoned no longer overwrites it; and a clean
    shutdown stop now writes nil, clearing an error an earlier
    health-triggered stop left there.

### Features

- let a service end the controller by giving up or finishing ([1972368](https://gitlab.com/phpboyscout/go/controls/-/commit/1972368700a5b655d69b2ef6e4a51edfeeb07ec6))
- recover a panic in a service as that service's failure ([8666672](https://gitlab.com/phpboyscout/go/controls/-/commit/8666672f75903fccb178e615b069750f8ddd5f1b))
- say why a controller stopped, and stop competing for its error channel ([3890cf4](https://gitlab.com/phpboyscout/go/controls/-/commit/3890cf4a794bcaf7b09dbf05931b9b120e1f9981))

## [v0.7.1](https://gitlab.com/phpboyscout/go/controls/-/releases/v0.7.1)

[Compare to previous version](https://gitlab.com/phpboyscout/go/controls/-/compare/v0.7.0...v0.7.1)

### Notes

- Releases are announced to the estate's release feed.

### Bug Fixes

- **deps**: update module golang.org/x/tools to v0.51.0 ([5af6d0a](https://gitlab.com/phpboyscout/go/controls/-/commit/5af6d0a7280f0f3cf77942004d224e80e3456d91))
- **deps**: update module golang.org/x/tools to v0.50.0 ([b9b6ce6](https://gitlab.com/phpboyscout/go/controls/-/commit/b9b6ce655d89cd0bf93b026f9bc172cc2f90accd))

## [v0.7.0](https://gitlab.com/phpboyscout/go/controls/-/releases/v0.7.0)

[Compare to previous version](https://gitlab.com/phpboyscout/go/controls/-/compare/v0.6.0...v0.7.0)

### Notes

- `Generational` gains a `ReleaseAttempt` field, the budget each call to `Release` gets. Zero or negative keeps the 250ms default, so nothing changes unless you set it. The retry until `Release` returns `nil` is unchanged; only each attempt's length is yours.

- A consumer that replaces the error channel with `SetErrorsChannel` is now its only receiver: the controller neither reads nor logs from a replaced channel, so every forwarded error reaches the consumer. Before, the controller's own handler competed for it and a consumer could miss errors. If you relied on the controller logging errors from a channel you replaced, log them yourself.

- `ErrRestartsExhausted` is inside the error a service leaves on `ServiceInfo.Error` and `Errors()` when it has used up its restart policy, and inside a child's `Failure.Err`. Test for it with `errors.Is`; the last error still matches beside it, and no message changes.

### Features

- **lifecycle**: let a Generational set the budget each Release call gets ([9a242cf](https://gitlab.com/phpboyscout/go/controls/-/commit/9a242cfd692f86ae7ffa0b922778d6eb181b47e1))
- **services**: export ErrRestartsExhausted, and keep the last error beside it ([90fd27e](https://gitlab.com/phpboyscout/go/controls/-/commit/90fd27e8220659aed75f5b06c70d6e501618388e))

### Bug Fixes

- **controller**: stop reading an error channel a consumer replaced ([a41004f](https://gitlab.com/phpboyscout/go/controls/-/commit/a41004f0dd32735ff3706f33e0516d06a0ea1f6f))
- **services**: serialise the writers of a service's ServiceInfo ([45a50a1](https://gitlab.com/phpboyscout/go/controls/-/commit/45a50a1c7c6f2aab210ea114e895a5746fdf88da))

## [v0.6.0](https://gitlab.com/phpboyscout/go/controls/-/releases/v0.6.0)

[Compare to previous version](https://gitlab.com/phpboyscout/go/controls/-/compare/v0.5.0...v0.6.0)

### Notes

- A nested module `lint/` ships `singleuse`, an advisory analyzer that names the line where a `StartFunc` captures a server, listener or supervisor a restart cannot reuse. Run it with `go run gitlab.com/phpboyscout/go/controls/lint/cmd/singleuse@latest ./...`. It adds nothing to this module's dependency graph.

### Features

- **lint**: add singleuse, an advisory analyzer for the capture a restart cannot reuse ([22ff7e4](https://gitlab.com/phpboyscout/go/controls/-/commit/22ff7e4e06b250ca24e91a7c7fa0e42b5cd48be0))

## [v0.5.0](https://gitlab.com/phpboyscout/go/controls/-/releases/v0.5.0)

[Compare to previous version](https://gitlab.com/phpboyscout/go/controls/-/compare/v0.4.0...v0.5.0)

### Notes

- Generational[R] gives a service that holds something - a listener, a connection, a supervisor, a session - a fresh value on every Start, and refuses access to a stopped one rather than handing back a stale handle. Reach for it when a restart would otherwise give the second run the first run's resources, which has happened five times across this estate. Use is the only accessor by design. See the new how-to guide, Survive a restart.

- WithStopErr registers a stop function that reports whether it released everything, and the result is recorded in ServiceInfo.StopErr. WithStop is unchanged and equivalent to one returning nil, so nothing that already works needs to move. A panicking stop is now contained and recorded on both stop paths; previously it was swallowed on shutdown and unrecovered on the health-restart path, so the same defect either vanished or killed the process depending on which reached it.

### Features

- **lifecycle**: add Generational, the unit a restart replaces ([282bea2](https://gitlab.com/phpboyscout/go/controls/-/commit/282bea21a07f18e638ae64076da5a35f42c86f7d))
- **services**: let a stop report that it failed to release ([4df9c80](https://gitlab.com/phpboyscout/go/controls/-/commit/4df9c80384b572e82d8195b3b547bbf5d2ccf2e1))

## [v0.4.0](https://gitlab.com/phpboyscout/go/controls/-/releases/v0.4.0)

[Compare to previous version](https://gitlab.com/phpboyscout/go/controls/-/compare/v0.3.2...v0.4.0)

### Features

- **supervisor**: let a child declare an expected terminal error ([dccf749](https://gitlab.com/phpboyscout/go/controls/-/commit/dccf749b5b42c3a5bfb38c7a57d0514eb98b3c6e))
- **controls**: report unready outside Running, and split Unknown into three states ([85c004f](https://gitlab.com/phpboyscout/go/controls/-/commit/85c004fb4b65f4b74a19c8e9d06d02753c05a970))
- **supervisor**: supervise children that attach and detach at runtime ([85448cd](https://gitlab.com/phpboyscout/go/controls/-/commit/85448cde2642e6a15d06f70f50a964adc26ee62e))

### Bug Fixes

- **controls**: a cancelled parent is not a health check timeout ([c0e1de6](https://gitlab.com/phpboyscout/go/controls/-/commit/c0e1de6e45ef3fc5924125e4d2cff8a4f0cabedc))
- **supervisor**: make the lifecycle a state machine and bound every wait ([e297bfa](https://gitlab.com/phpboyscout/go/controls/-/commit/e297bfa37b0814a2f59f8a2922c703af2bf603b4))

## [v0.3.2](https://gitlab.com/phpboyscout/go/controls/-/releases/v0.3.2)

[Compare to previous version](https://gitlab.com/phpboyscout/go/controls/-/compare/v0.3.1...v0.3.2)

### Bug Fixes

- **deps**: update module github.com/stretchr/testify to v1.12.1 ([7d6c19c](https://gitlab.com/phpboyscout/go/controls/-/commit/7d6c19cfa6b86d1faadbe300947bacd0f3bb0877))
- **deps**: update go modules ([1d69f79](https://gitlab.com/phpboyscout/go/controls/-/commit/1d69f7963da4c44458b41b72dd52ecc9452259dd))
- **deps**: require go 1.26.6 for the stdlib advisories ([3708a18](https://gitlab.com/phpboyscout/go/controls/-/commit/3708a187220438a44116835f6159c02449bd0ffb))
- **ci**: bump the cicd components to v0.36.0 for Go 1.26.6 ([a266993](https://gitlab.com/phpboyscout/go/controls/-/commit/a266993463a21317e3060cfe05ea1d43c4e7aead))

## [v0.3.1](https://gitlab.com/phpboyscout/go/controls/-/releases/v0.3.1)

[Compare to previous version](https://gitlab.com/phpboyscout/go/controls/-/compare/v0.3.0...v0.3.1)

### Bug Fixes

- **deps**: update module gitlab.com/phpboyscout/go/errors to v0.2.0 ([eb167d7](https://gitlab.com/phpboyscout/go/controls/-/commit/eb167d7955de81301f0e32403002cd48c5b64cf6))

## [v0.3.0](https://gitlab.com/phpboyscout/go/controls/-/releases/v0.3.0)

[Compare to previous version](https://gitlab.com/phpboyscout/go/controls/-/compare/v0.2.0...v0.3.0)

### Features

- adopt gitlab.com/phpboyscout/go/errors ([bdc510c](https://gitlab.com/phpboyscout/go/controls/-/commit/bdc510cc3ca63d096c75bbe85ea9cd2d0f9da176))

## [v0.2.0](https://gitlab.com/phpboyscout/go/controls/-/releases/v0.2.0)

[Compare to previous version](https://gitlab.com/phpboyscout/go/controls/-/compare/v0.1.4...v0.2.0)

### Features

- **controls**: single-owner signal handling and deterministic shutdown cause ([bd332b7](https://gitlab.com/phpboyscout/go/controls/-/commit/bd332b7db26ea84a0c3a78ccea898da13e6cfa7a))

## [v0.1.4](https://gitlab.com/phpboyscout/go/controls/-/releases/v0.1.4)

[Compare to previous version](https://gitlab.com/phpboyscout/go/controls/-/compare/v0.1.3...v0.1.4)

### Bug Fixes

- **controller**: defer OS-signal registration to Start ([acaf06c](https://gitlab.com/phpboyscout/go/controls/-/commit/acaf06ca771e8769737c88102ecbefdc6c449e8c))
- **services**: reset restart backoff after a healthy run ([71346a6](https://gitlab.com/phpboyscout/go/controls/-/commit/71346a67b9a1768e90a5be11e34765a7877533d6))
- **services**: keep health probes responsive during the stop sequence ([b38102e](https://gitlab.com/phpboyscout/go/controls/-/commit/b38102e7a1e237b42ae75d0852a87e5e7176f5e6))
- **controller**: guard the Stop() send against a completed shutdown ([72b057a](https://gitlab.com/phpboyscout/go/controls/-/commit/72b057aa4aacbcaa329c8c10b611e82900296931))
- **healthcheck**: enforce check timeout and bound async cache staleness ([ff512a8](https://gitlab.com/phpboyscout/go/controls/-/commit/ff512a85375d32fc7e2bc5c80ac80ac85551bbac))

## [v0.1.3](https://gitlab.com/phpboyscout/go/controls/-/releases/v0.1.3)

[Compare to previous version](https://gitlab.com/phpboyscout/go/controls/-/compare/v0.1.2...v0.1.3)

### Bug Fixes

- **controller**: bound the wait against context-ignoring StartFuncs (D10) ([c22f525](https://gitlab.com/phpboyscout/go/controls/-/commit/c22f5254df506d9d941617d972e41e992b5a8cf5))

## [v0.1.2](https://gitlab.com/phpboyscout/go/controls/-/releases/v0.1.2)

### Bug Fixes

- **deps**: bump golang.org/x/text to v0.40.0

## [v0.1.1](https://gitlab.com/phpboyscout/go/controls/-/releases/v0.1.1)

### Bug Fixes

- guard workflow dedup rule so release tag pipelines fire

## [v0.1.0](https://gitlab.com/phpboyscout/go/controls/-/releases/v0.1.0)

### Features

- extract the controls lifecycle supervisor from go-tool-base

### Bug Fixes

- **security**: bump golang.org/x/sys to v0.46.0

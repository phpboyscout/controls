# Changelog

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

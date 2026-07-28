# Changelog

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

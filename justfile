# Task runner for gitlab.com/phpboyscout/go/controls

# Default: tidy, lint, test
default: tidy lint test

# Tidy modules, the nested lint module included
tidy:
    go mod tidy
    cd lint && go mod tidy

# Unit tests with coverage, both modules
test:
    go test ./... -cover
    cd lint && go test ./... -cover

# Race detector, both modules
test-race:
    go test -race ./...
    cd lint && go test -race ./...

# E2E BDD (Godog) tests
test-e2e:
    INT_TEST_E2E=1 go test ./test/e2e/... -v -timeout 10m

# Lint, both modules (the nested one reuses the root config)
lint:
    golangci-lint run
    cd lint && golangci-lint run --config ../.golangci.yaml ./...

# Run the singleuse analyzer over this module. Built from lint/ and run from
# the root, because a package pattern is resolved against the module the
# process runs in.
singleuse:
    #!/usr/bin/env bash
    set -euo pipefail
    bin="$(mktemp)"
    trap 'rm -f "$bin"' EXIT
    (cd lint && go build -o "$bin" ./cmd/singleuse)
    "$bin" ./...

# Auto-fix lint, both modules
lint-fix:
    golangci-lint run --fix
    cd lint && golangci-lint run --fix --config ../.golangci.yaml ./...

# Regenerate mocks
mocks:
    mockery

# HTML coverage report
coverage:
    go test ./... -coverprofile=coverage.out
    go tool cover -html=coverage.out

# Benchmarks
bench:
    go test -bench=. -benchmem ./...

# Vulnerability scan, both modules
vuln:
    govulncheck ./...
    cd lint && govulncheck ./...

# Find unreachable exported symbols
deadcode:
    deadcode ./...

# Full local CI: tidy, test, race, lint
ci: tidy test test-race lint

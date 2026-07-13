# Task runner for gitlab.com/phpboyscout/go/controls

# Default: tidy, lint, test
default: tidy lint test

# Tidy modules
tidy:
    go mod tidy

# Unit tests with coverage
test:
    go test ./... -cover

# Race detector
test-race:
    go test -race ./...

# E2E BDD (Godog) tests
test-e2e:
    INT_TEST_E2E=1 go test ./test/e2e/... -v -timeout 10m

# Lint
lint:
    golangci-lint run

# Auto-fix lint
lint-fix:
    golangci-lint run --fix

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

# Vulnerability scan
vuln:
    govulncheck ./...

# Find unreachable exported symbols
deadcode:
    deadcode ./...

# Full local CI: tidy, test, race, lint
ci: tidy test test-race lint

# Controls

The `controls` package provides a sophisticated service lifecycle management system. It enables centralized control of multiple concurrent services with shared communication channels for errors, signals, health monitoring, and control messages.

## Overview

The Controls package is built around the `Controllable` interface and the `Controller` struct, providing a unified API for managing service lifecycles.

- **Centralized Service Management**: Coordinate multiple services (HTTP servers, background workers, schedulers) from a single controller with consistent start/stop behavior.
- **Shared Communication Channels**: Services share common channels for errors, OS signals, health monitoring, and control messages.
- **Graceful Shutdown**: Built-in support for graceful shutdown with proper cleanup ordering and timeout handling.
- **Health Monitoring**: Integrated health check system that services can use to report their status.

## Quick Start

A simple example of an HTTP server managed by the controls system:

```go
func main() {
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    // Create controller
    controller := controls.NewController(ctx, controls.WithLogger(logger))

    // Create HTTP server
    srv := &http.Server{Addr: ":8080", Handler: mux}

    // Register service
    controller.Register(
        "http-server",
        func() error { return srv.ListenAndServe() }, // Start
        func() { srv.Shutdown(ctx) },               // Stop
        func() { /* status report */ },            // Status
    )

    // Start and wait
    controller.Start()
    controller.Wait()
}
```

## Core Interface

The `Controllable` interface provides the primary API for service control:

```go
type Controllable interface {
    // Channel access
    Messages() chan Message
    Health() chan HealthMessage
    Errors() chan error
    Signals() chan os.Signal

    // Lifecycle management
    Start()
    Stop()
    SetWaitGroup(wg *sync.WaitGroup)

    // Context and state
    GetContext() context.Context
    GetState() State
    IsRunning() bool

    // Service registration
    Register(id string, start StartFunc, stop StopFunc, status StatusFunc)
}
```

## Basic Usage

### Creating a Controller
```go
controller := controls.NewController(ctx,
    controls.WithLogger(logger),
  // controls.WithoutSignals(), // Optional: disable OS signal handling
)
```

### Registering Services
Services are registered with a unique ID and three functions: `Start`, `Stop`, and `Status`.

```go
controller.Register("my-service", startFunc, stopFunc, statusFunc)
```

## Advanced Usage

### Error Handling Strategy
Monitor the `Errors()` channel to respond to service failures.

```go
go func() {
    for err := range controller.Errors() {
        if isCritical(err) {
            controller.Stop() // Initiate graceful shutdown
            return
        }
    }
}()
```

### Health Monitoring
Request status updates via the `Messages()` channel and monitor reports on the `Health()` channel.

```go
// Request status from all services
controller.Messages() <- controls.Status

// Monitor reports
go func() {
    for health := range controller.Health() {
        logger.Info("Service status", "id", health.Message, "status", health.Status)
    }
}()
```

### Signal Handling
The controller automatically handles `SIGINT` and `SIGTERM` unless disabled. Custom signal handling can be implemented by monitoring the `Signals()` channel.

## Testing & Mocking

The `controls` package includes auto-generated mocks for testing:

```go
func TestService(t *testing.T) {
    mockController := controls.NewMockControllable(t)
    mockController.EXPECT().Register("test-service", mock.Anything, mock.Anything, mock.Anything).Return()
    
    service := NewTestService(mockController)
    service.Initialize()
}
```

## Best Practices

1. **Concrete Types**: Use `*controls.Controller` in production and `controls.Controllable` for DI/testing.
2. **Context Awareness**: Services should respect the controller's context for long-running operations.
3. **Fail Fast**: Categorize errors and use `controller.Stop()` for critical failures.
4. **Graceful Shutdown**: Implement proper timeout handling in service `Stop` functions.
5. **Meaningful Health**: Implement health checks that verify actual service state (e.g., database ping).

// Package controls provides a lifecycle controller for managing concurrent,
// long-running services such as HTTP servers, background workers, and schedulers.
//
// A [Controller] starts a fixed set of registered services concurrently,
// aggregates the health probes they supply into [HealthReport] values, and
// drives a bounded graceful shutdown in reverse registration order, with an
// optional [RestartPolicy]. It installs no OS signal handler unless asked to
// with [WithSignals], and it serves no health endpoint: the reports are plain
// values for a transport to expose.
//
// A [Supervisor] runs children that attach and detach while the process is
// running, and a [Generational] owns one generation of a single-use resource at
// a time for a service that has to survive a restart.
//
// The [Controllable] interface, and the narrower role interfaces it is composed
// of, let code that depends on a controller substitute a fake.
package controls

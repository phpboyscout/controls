package controls

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"
)

// CheckStatus represents the health state of a check.
type CheckStatus int

const (
	// CheckHealthy indicates the check passed.
	CheckHealthy CheckStatus = iota
	// CheckDegraded indicates the check passed but with warnings.
	// Maps to OverallHealthy: true with Status: "DEGRADED".
	CheckDegraded
	// CheckUnhealthy indicates the check failed.
	// Maps to OverallHealthy: false with Status: "ERROR".
	CheckUnhealthy
)

// CheckResult represents the outcome of a health check.
type CheckResult struct {
	// Status is the health status.
	Status CheckStatus
	// Message provides human-readable detail about the check result.
	Message string
	// Timestamp is when this result was produced.
	Timestamp time.Time
}

// CheckType determines which health endpoint(s) a check contributes to.
type CheckType int

const (
	// CheckTypeReadiness contributes to the readiness endpoint.
	CheckTypeReadiness CheckType = iota
	// CheckTypeLiveness contributes to the liveness endpoint.
	CheckTypeLiveness
	// CheckTypeBoth contributes to both liveness and readiness endpoints.
	CheckTypeBoth
)

// HealthCheck defines a named health check function.
type HealthCheck struct {
	// Name is the unique identifier for this check.
	Name string
	// Check is the function that performs the health check.
	// It receives a context with the check's timeout applied.
	Check func(ctx context.Context) CheckResult
	// Timeout is the maximum duration for a single check execution.
	// Default: 5s.
	Timeout time.Duration
	// Interval is the polling interval for async checks.
	// Zero means synchronous (run on every health request).
	Interval time.Duration
	// Type determines which health endpoints this check feeds into.
	// Default: CheckTypeReadiness.
	Type CheckType
}

const (
	Stop Message = "stop"
)

const (
	// Unknown means the state could not be determined. It is what [Controller.GetState]
	// reports for a zero-valued Controller, i.e. one built without NewController,
	// which is otherwise undetectable because State is a string type and its zero
	// value is the empty string.
	Unknown State = "unknown"

	// NeverStarted means the controller was constructed and Start has not been
	// called. Registration is only honoured in this state, and a Stop here leaves
	// it unchanged: stopping something that never started is a no-op, so the
	// controller stays startable.
	NeverStarted State = "never_started"

	// Running means Start has been called and shutdown has not begun. It is the
	// only state in which the controller reports ready.
	Running State = "running"

	// UnableToStart means a registered service has proven it will never start: it
	// has failed without ever starting cleanly and has exhausted its restart
	// policy. Nothing is stopped, and the error still reaches the error channel;
	// what changes is that the controller stops reporting ready, so an
	// orchestrator routes no traffic to a process that cannot do its job.
	//
	// A service that fails and then recovers on a restart never reaches this, and
	// neither does one that started cleanly and failed later. Both conditions are
	// required, so the state is terminal rather than something readiness flaps on
	// through a slow boot.
	UnableToStart State = "unable_to_start"

	Stopping State = "stopping"
	Stopped  State = "stopped"
)

// State represents the lifecycle state of the controller.
//
// It moves in one direction: NeverStarted to Running, then either to Stopping and
// Stopped or to UnableToStart and then Stopping and Stopped. Unknown is not part
// of that sequence; it means the value could not be determined.
type State string

// Message represents a control message sent to the controller (e.g. "stop").
type Message string

// StartFunc is the callback invoked to start a service. It receives a context
// that is cancelled when the controller shuts down.
//
// # A restart calls this again, so what the closure captured is shared
//
// Everything this function's closure captured is shared across generations and
// must be safe to use again. Anything single-use or generation-scoped — a
// listener, a server, a supervisor, an exit status, a per-tenant handle — must
// be built INSIDE the run that consumes it, or the second run gets the first
// run's corpse.
//
// The two failures this causes are worth naming, because they look nothing
// alike. Either the restart cannot work at all, or the stopped thing carries on
// looking healthy — a status cell that was never cleared, a handle whose
// backing is gone. So: a stopped generation must refuse further use, loudly,
// and a new generation is built whole, from one recipe, never revived and never
// partially reused.
//
// [Generational] provides both for a service that needs them. A service that
// genuinely captures nothing single-use needs neither, and most do not.
type StartFunc func(context.Context) error

// StopErrFunc stops a service and reports how the stop ended.
//
// A nil error means every resource the run acquired has been released. A
// non-nil one means the budget expired or release failed, and the service may
// still be holding something — a listener, a connection, a subscription.
//
// # It is recorded, not acted upon
//
// The Controller stores the result in [ServiceInfo] and changes nothing else:
// no restart is refused and no policy is altered on the strength of it. Knowing
// the answer is the prerequisite, and making a restart depend on it is a
// behaviour change for every consumer and a separate decision.
//
// Before this existed a stop that ignored its context was abandoned at the
// deadline silently, and nothing anywhere recorded that it had happened.
type StopErrFunc func(context.Context) error

// StopFunc is the callback invoked to stop a service gracefully. The context
// carries the shutdown timeout.
type StopFunc func(context.Context)

// StatusFunc is the callback invoked to check a service's health.
// Returns nil if healthy, an error otherwise.
//
// It must report the CURRENT run. A status cell captured by the closure and
// never cleared between generations keeps reporting the previous run's failure,
// which fails the health threshold, which restarts the service, which reports
// the same stale failure — a service churning to restart exhaustion without one
// log line naming the cause. See [StartFunc] on what a restart shares.
type StatusFunc func() error

// ProbeFunc is a health check function for liveness or readiness probes.
type ProbeFunc func() error

// ValidErrorFunc determines whether an error from a service is expected
// (e.g. http.ErrServerClosed) and should not trigger a restart.
type ValidErrorFunc func(error) bool

// ServiceOption is a functional option for configuring a Service.
type ServiceOption func(*Service)

// WithStart sets the service's start function.
func WithStart(fn StartFunc) ServiceOption {
	return func(s *Service) {
		s.Start = fn
	}
}

// WithStop sets the service's graceful shutdown function.
func WithStop(fn StopFunc) ServiceOption {
	return func(s *Service) {
		s.Stop = fn
	}
}

// WithStopErr registers a stop function that can report failure to release.
//
// It is purely additive: [WithStop] is unchanged and is equivalent to a
// [StopErrFunc] that always returns nil, so nothing that already works needs to
// move. Setting both is a mistake rather than a merge — the error-reporting one
// wins, because the alternative is calling a service's stop twice.
func WithStopErr(fn StopErrFunc) ServiceOption {
	return func(s *Service) {
		s.StopErr = fn
	}
}

// WithStatus sets the service's health check function.
func WithStatus(fn StatusFunc) ServiceOption {
	return func(s *Service) {
		s.Status = fn
	}
}

// WithLiveness sets a liveness probe for the service.
func WithLiveness(fn ProbeFunc) ServiceOption {
	return func(s *Service) {
		s.Liveness = fn
	}
}

// WithReadiness sets a readiness probe for the service.
func WithReadiness(fn ProbeFunc) ServiceOption {
	return func(s *Service) {
		s.Readiness = fn
	}
}

// RestartPolicy defines how a service should be restarted on failure.
type RestartPolicy struct {
	MaxRestarts            int
	InitialBackoff         time.Duration
	MaxBackoff             time.Duration
	HealthFailureThreshold int
	HealthCheckInterval    time.Duration
	// RestartResetInterval is how long a service must run healthily before its
	// consecutive-failure restart counter is reset to zero. Zero selects
	// DefaultRestartResetInterval. The count therefore measures consecutive
	// failures, not lifetime restarts.
	RestartResetInterval time.Duration
}

// WithRestartPolicy configures automatic restart behaviour for a service.
func WithRestartPolicy(policy RestartPolicy) ServiceOption {
	return func(s *Service) {
		s.RestartPolicy = &policy
	}
}

// WithRestartResetInterval sets how long a service must run healthily before its
// consecutive-failure restart counter resets. It implies a restart policy: if
// the service has none, a default policy is created so the interval takes effect.
func WithRestartResetInterval(d time.Duration) ServiceOption {
	return func(s *Service) {
		if s.RestartPolicy == nil {
			s.RestartPolicy = &RestartPolicy{}
		}

		s.RestartPolicy.RestartResetInterval = d
	}
}

// ServiceInfo holds runtime metadata about a registered service.
type ServiceInfo struct {
	Name         string
	RestartCount int
	LastStarted  time.Time
	LastStopped  time.Time
	Error        error

	// StopErr is how the last stop ended: nil when every resource was
	// released, non-nil when it was not, and always nil for a service
	// registered with [WithStop], which cannot report either way.
	//
	// A panic inside a stop is contained and recorded here rather than ending
	// the process.
	StopErr error
}

// ServiceStatus is the health status of a single service, used in HealthReport.
type ServiceStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "OK", "ERROR"
	Error  string `json:"error,omitempty"`
}

// HealthReport is the aggregate health status across all registered services.
type HealthReport struct {
	OverallHealthy bool `json:"overall_healthy"`

	// State is the controller's lifecycle state when the report was taken.
	//
	// Carried on every report, including Status, which is explicitly not a gate.
	// A reader that only sees OverallHealthy cannot tell an unhealthy service
	// from a controller that has begun shutting down, and those want different
	// responses.
	State State `json:"state"`

	Services []ServiceStatus `json:"services"`
}

// Runner provides service lifecycle operations.
type Runner interface {
	Start()
	Stop()
	IsRunning() bool
	IsStopped() bool
	IsStopping() bool
	Register(id string, opts ...ServiceOption)
}

// HealthReporter provides read access to service health, liveness, and readiness
// reports, and to per-service runtime information. Handlers and transports that
// only need to query health should depend on this interface rather than the full
// Controllable.
type HealthReporter interface {
	Status() HealthReport
	Liveness() HealthReport
	Readiness() HealthReport
	GetServiceInfo(name string) (ServiceInfo, bool)
}

// HealthCheckReporter extends HealthReporter with check-specific queries.
type HealthCheckReporter interface {
	HealthReporter
	// GetCheckResult returns the latest result for a named health check.
	GetCheckResult(name string) (CheckResult, bool)
}

// StateAccessor provides access to controller state and context, and is meant
// to be consumed AND implemented outside this module.
//
// The setter is deliberate, and is why this is not a read-only interface: a
// consumer driving a controller it owns needs to be able to say what state it is
// in, and a consumer implementing this interface over its own type needs the
// same surface the Controller has. It was proposed for removal once on the
// reading that this was a read-side view, which the doc comment then said it
// was; see wiki spec 0003 D7.
//
// SetState mutates a field the control goroutines read, so the contract is the
// one Configurable states: call it during construction, or on a controller you
// own. Reaching into a running controller from elsewhere races those goroutines,
// and since readiness is gated on the state (0003 D2) it can also take a healthy
// process out of rotation.
type StateAccessor interface {
	GetState() State
	SetState(state State)
	GetContext() context.Context
	GetLogger() *slog.Logger
}

// Configurable provides controller configuration setters.
//
// These setters mutate channel and logger fields that controller
// goroutines read after Start. They carry no internal synchronization
// and must only be called during construction — before Start — which is
// how the WithX ControllerOpt options apply them inside NewController.
// Calling any setter after Start races the running goroutines and is a
// programming error.
type Configurable interface {
	SetErrorsChannel(errs chan error)
	SetMessageChannel(control chan Message)
	SetSignalsChannel(sigs chan os.Signal)
	SetWaitGroup(wg *sync.WaitGroup)
	SetShutdownTimeout(d time.Duration)
	SetLogger(l *slog.Logger)
}

// ChannelProvider provides access to controller channels.
type ChannelProvider interface {
	Messages() chan Message
	Errors() chan error
	Signals() chan os.Signal
}

// Controllable is the full controller interface, composed of all role-based interfaces.
// Prefer using the narrower interfaces (Runner, HealthReporter, Configurable, etc.) where possible.
type Controllable interface {
	Runner
	HealthReporter
	StateAccessor
	Configurable
	ChannelProvider
}

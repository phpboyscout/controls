package controls

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	errors "gitlab.com/phpboyscout/go/errors"
)

// ErrShutdown is the cause attached to the controller context when a graceful
// shutdown is initiated. Callers can distinguish a controlled stop from an
// upstream cancellation via context.Cause(ctx) == controls.ErrShutdown.
var ErrShutdown = errors.NewSentinel("controls.shutdown", "controller shutdown")

// DefaultShutdownTimeout is the time allowed for graceful shutdown before
// services are force-stopped.
const DefaultShutdownTimeout = 5 * time.Second

// signalChanBuffer sizes the opt-in signal channel. One slot is enough: the
// handler distinguishes only "first signal" (graceful stop) from "second
// signal" (release the handler), and it is reading again before the second can
// matter.
const signalChanBuffer = 1

// Controller orchestrates the lifecycle of registered services: concurrent
// startup, health monitoring, ordered (reverse-registration) shutdown, and
// signal handling.
type Controller struct {
	// ctx is the context every service receives. It is deliberately NOT
	// cancellation-linked to the caller's context: the controller owns its own
	// cancellation so that ErrShutdown is the cause of every stop it drives,
	// whatever triggered it (spec 0001, D3).
	ctx    context.Context
	cancel context.CancelCauseFunc
	// parent is the caller's context, retained only so the error/context handler
	// can watch it. Its completion — by cancellation OR deadline — is a TRIGGER
	// for a graceful Stop, not the cancellation itself.
	parent   context.Context
	logger   *slog.Logger
	messages chan Message
	errs     chan error
	// ownErrs is the error channel NewController made. The controller closes
	// it when done; a channel installed with SetErrorsChannel is the
	// consumer's and is never closed here (spec 0008 D9).
	ownErrs         chan error
	signals         chan os.Signal
	wg              *sync.WaitGroup
	shutdownTimeout time.Duration
	state           State
	stateMutex      sync.Mutex
	services        Services
	healthChecks    map[string]*healthCheckEntry
	validError      ValidErrorFunc
	// shutdownComplete is closed once handleStopMessage has finished the full
	// shutdown sequence. The parent watch and signal handler goroutines watch
	// it as their exit condition so they terminate rather than spin or leak.
	shutdownComplete chan struct{}

	// Under stateMutex, set with the transition that starts shutdown (spec
	// 0008 D1, D3).
	deadline       time.Time
	outcome        Outcome
	outcomeWritten bool

	onEvent func(ServiceEvent)

	// subMu orders subscribing to the error channel against shutdown's cutoff,
	// after which no forwarder can start (spec 0008 D9).
	subMu       sync.Mutex
	subscribed  bool
	subStarted  bool
	subFinished bool
	errsFwd     *forwarder
	eventsFwd   *forwarder
}

func (c *Controller) GetContext() context.Context {
	return c.ctx
}

func (c *Controller) Messages() chan Message {
	return c.messages
}

func (c *Controller) SetMessageChannel(messages chan Message) {
	c.messages = messages
}

func (c *Controller) Signals() chan os.Signal {
	return c.signals
}

func (c *Controller) SetSignalsChannel(signals chan os.Signal) {
	// Detach any prior OS-signal registration so a swapped-out channel does not
	// keep receiving SIGINT/SIGTERM (D6). signal.Stop is a no-op for channels
	// that were never passed to signal.Notify.
	if c.signals != nil {
		signal.Stop(c.signals)
	}

	c.signals = signals
}

// Errors returns the error channel, which receives the error of every terminal
// service failure and of the latest of each run of retries. Calling it
// subscribes, so failures before the first call are not delivered; read it
// until it closes, and never send on it.
func (c *Controller) Errors() chan error {
	c.subMu.Lock()
	defer c.subMu.Unlock()

	if !c.subFinished && !c.subscribed {
		c.subscribed = true

		if c.subStarted && c.errsFwd == nil {
			c.startErrorsForwarder()
		}
	}

	return c.errs
}

// SetErrorsChannel installs a channel of the consumer's own in place of the
// controller's. Like every Configurable setter it must be called before Start.
// The controller never closes it, and sends nothing on it after Done; read it
// until Done, then take what is still buffered.
func (c *Controller) SetErrorsChannel(errs chan error) {
	c.errs = errs
}

func (c *Controller) WaitGroup() *sync.WaitGroup {
	return c.wg
}

func (c *Controller) SetWaitGroup(wg *sync.WaitGroup) {
	c.wg = wg
}

func (c *Controller) SetShutdownTimeout(d time.Duration) {
	c.shutdownTimeout = d
}

func (c *Controller) SetState(state State) {
	c.stateMutex.Lock()
	defer c.stateMutex.Unlock()

	c.state = state
}

func (c *Controller) GetState() State {
	c.stateMutex.Lock()
	defer c.stateMutex.Unlock()

	// A Controller built without NewController holds the zero value of State,
	// which is the empty string: neither a valid state nor otherwise detectable.
	// Reporting it as Unknown is what gives that constant a job.
	if c.state == "" {
		return Unknown
	}

	return c.state
}

func (c *Controller) SetLogger(l *slog.Logger) {
	c.logger = l
}

func (c *Controller) GetLogger() *slog.Logger {
	return c.logger
}

func (c *Controller) IsRunning() bool {
	return c.GetState() == Running
}

func (c *Controller) IsStopped() bool {
	return c.GetState() == Stopped
}

func (c *Controller) IsStopping() bool {
	return c.GetState() == Stopping
}

func (c *Controller) Register(id string, opts ...ServiceOption) {
	s := Service{
		Name: id,
	}

	for _, opt := range opts {
		opt(&s)
	}

	// Registering after Start has already transitioned the controller away from
	// the initial NeverStarted state cannot supervise the service: Start has already
	// snapshotted the service count and launched the supervisor goroutines, so a
	// late registration is never started, monitored, or stopped. Mirror
	// RegisterHealthCheck's guard by surfacing the problem — but as a WARNING,
	// since Register has no error return and the supervisor spec defaults missing
	// funcs to no-ops. The service is still added (behaviour unchanged) so any
	// status query still reflects it; it simply is not supervised.
	if c.GetState() != NeverStarted {
		c.logger.Warn(
			"Register called after Start; service will not be supervised",
			"service_name", id,
			"current_state", c.GetState(),
		)
	}

	c.services.add(s)
	c.logger.Debug("Registered service", "service_name", id)
}

// RegisterHealthCheck adds a standalone health check to the controller.
// Must be called before Start(). The check name must be unique among health
// checks; it is not checked against service names, so a check that shares a
// name with a service is accepted and the report carries both entries.
func (c *Controller) RegisterHealthCheck(check HealthCheck) error {
	if c.GetState() != NeverStarted {
		return errors.New("cannot register health check after start")
	}

	if _, exists := c.healthChecks[check.Name]; exists {
		return errors.Newf("duplicate health check name: %q", check.Name)
	}

	c.healthChecks[check.Name] = &healthCheckEntry{check: check}
	c.logger.Debug("Registered health check", "name", check.Name)

	return nil
}

// GetCheckResult returns the latest result for a named health check.
func (c *Controller) GetCheckResult(name string) (CheckResult, bool) {
	entry, ok := c.healthChecks[name]
	if !ok {
		return CheckResult{}, false
	}

	r := entry.lastResult.Load()
	if r == nil {
		return CheckResult{}, false
	}

	return *r, true
}

// markUnableToStart records that a registered service has proven it will never
// start (D4). Only a Running controller transitions: a shutdown already under
// way is not reclassified, and a second unstartable service changes nothing.
func (c *Controller) markUnableToStart(name string, err error) {
	if c.compareAndSetState(Running, UnableToStart) {
		c.logger.Error("a registered service will never start; the controller is now reporting unready",
			"service_name", name, "error", err)
	}
}

// beginShutdown transitions into Stopping from any state a shutdown may start
// from and, in the same critical section, records the cause and the deadline.
// It reports whether this caller made the transition, so exactly one trigger's
// cause is recorded however many race (spec 0008 D1).
//
// UnableToStart is one of those states, and forgetting it would be quiet and
// bad: Stop would refuse, and the services that ARE running would never be told
// to stop (D8). The controller reports unready in that state but it is still a
// live process holding whatever its working services hold.
func (c *Controller) beginShutdown(cause StopCause, err error, sig os.Signal) bool {
	c.stateMutex.Lock()
	defer c.stateMutex.Unlock()

	if !c.enterStoppingLocked() {
		return false
	}

	c.recordCause(cause, err, sig)

	return true
}

// enterStoppingLocked moves Running or UnableToStart to Stopping. Callers hold
// stateMutex.
func (c *Controller) enterStoppingLocked() bool {
	if c.state != Running && c.state != UnableToStart {
		return false
	}

	c.state = Stopping

	return true
}

// recordCause records the first cause and the deadline it starts. Callers hold
// stateMutex.
func (c *Controller) recordCause(cause StopCause, err error, sig os.Signal) {
	if c.outcome.Cause != "" {
		return
	}

	c.outcome.Cause, c.outcome.Err, c.outcome.Signal = cause, err, sig
	c.deadline = time.Now().Add(c.shutdownTimeout)
}

// compareAndSetState atomically checks if the current state matches expected,
// and if so, sets it to next. Returns true if the transition occurred.
func (c *Controller) compareAndSetState(expected, next State) bool {
	c.stateMutex.Lock()
	defer c.stateMutex.Unlock()

	if c.state != expected {
		return false
	}

	c.state = next

	return true
}

// Start launches all registered services. It is idempotent: a second call while
// already running (or stopping/stopped) returns early without double-starting
// services or double-counting the wait group (D3).
func (c *Controller) Start() {
	// CAS NeverStarted -> Running. Only the first caller proceeds; this also sets the
	// Running state before launching services so the signal handler (running
	// concurrently via controls()) can transition to Stopping if an interrupt
	// arrives while services are still initialising.
	if !c.compareAndSetState(NeverStarted, Running) {
		c.logger.Warn("Start called, but controller has already started; ignoring", "current_state", c.GetState())

		return
	}

	c.logger.Debug("Controller set to running state")

	// Propagate the valid-error predicate to the supervisor before any service
	// run can be classified.
	c.services.validError = c.validError
	c.services.onUnableToStart = c.markUnableToStart
	c.services.publish = c.publish

	c.startForwarders()

	// Snapshot the service count under the services mutex so the wait-group add
	// matches exactly the goroutines services.start will spawn.
	c.services.mu.Lock()
	serviceCount := len(c.services.services)
	c.services.mu.Unlock()

	// +1 for the controller lifecycle itself — this is only decremented
	// when handleStopMessage completes, ensuring Wait() blocks until the
	// full shutdown sequence (stop all services, set state) has finished.
	c.wg.Add(1 + serviceCount)

	// Wire up services and async health checks BEFORE launching the control
	// goroutines (D8). controls() starts the signal handler and message
	// processor, either of which can drive a shutdown that reads each async
	// check's CancelFunc via cancelHealthChecks. Recording those CancelFuncs
	// (startAsyncCheck writes entry.cancel) must therefore happen-before the
	// goroutines that read them start, or the two accesses race when a shutdown
	// lands mid-startup.
	c.services.start(c.ctx, c.wg)
	c.startAsyncHealthChecks()

	go c.controls()

	c.logger.Debug("All services should now be running")
}

// Wait blocks until the shutdown sequence has finished, every async health
// check has exited, and every service has either started cleanly or had its
// supervisor goroutine exit. The shutdown sequence includes delivering queued
// events, so Wait can return up to the shutdown budget later when a consumer is
// slow.
//
// It is unbounded: a service whose Start fails and whose retry then ignores
// cancellation keeps Wait blocked even though the controller reports Stopped.
// Use Done and Outcome to decide an exit, or WaitContext to bound the wait
// (D10).
func (c *Controller) Wait() {
	c.wg.Wait()
}

// WaitContext waits for what Wait waits for, or until ctx is done, whichever
// comes first. It returns nil on a clean drain and ctx.Err()
// when the wait is abandoned. On the abandon path the internal helper goroutine
// (and any stuck supervisors pinning the wait group) are deliberately leaked —
// the same abandon-at-deadline tradeoff the shutdown sequence applies to
// context-ignoring StopFuncs (D10).
func (c *Controller) WaitContext(ctx context.Context) error {
	drained := make(chan struct{})

	go func() {
		c.wg.Wait()
		close(drained)
	}()

	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop initiates a graceful shutdown. Duplicate calls while already
// stopping or stopped are safely ignored. It may be called from a WithOnEvent
// callback.
func (c *Controller) Stop() {
	if !c.stopFor(CauseStop, nil, nil) {
		c.logger.Warn("Stop called, but not in expected state, unable to continue", "current_state", c.GetState())
	}
}

// stopFor starts a shutdown for cause and hands it to the message processor,
// reporting whether this caller started it.
func (c *Controller) stopFor(cause StopCause, err error, sig os.Signal) bool {
	if !c.beginShutdown(cause, err, sig) {
		return false
	}

	// If this caller won the transition but a Stop sent directly on Messages()
	// reached the processor first, nothing will receive this send. Completion
	// (D9) and the cancel only handleStopMessage makes both release it; the
	// cancel comes early enough that a WithOnEvent callback here does not wait
	// on its own drain (spec 0008 D1).
	select {
	case c.messages <- Stop:
	case <-c.shutdownComplete:
	case <-c.ctx.Done():
	}

	return true
}

// Controls sets the handlers for different control operations.
func (c *Controller) controls() {
	c.startSignalHandler()
	c.startParentWatch()
	c.processControlMessages()
}

func (c *Controller) startSignalHandler() {
	// Handle OS signals. Only runs when a signal channel is configured.
	if c.signals == nil {
		return
	}

	// Register the OS-signal handler here — paired with the reader goroutine
	// launched just below — rather than at construction. A controller that is
	// constructed but never started must not register a handler with no reader,
	// which would swallow SIGINT/SIGTERM and leave the process ignoring Ctrl-C
	// (F5). The registration is detached again on shutdown and on SetSignalsChannel
	// (D6).
	signal.Notify(c.signals, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		select {
		case sig := <-c.Signals():
			c.logger.Warn("received signal", "signal", sig)

			if !c.stopFor(CauseSignal, nil, sig) {
				c.logger.Warn("signal received, but not in expected state, unable to continue", "current_state", c.GetState())
			}
		case <-c.shutdownComplete:
			// Shutdown was driven by some other path (context cancel, direct
			// Stop). Exit rather than leak waiting for a signal that will never
			// come.
			return
		}

		// First signal initiated a graceful stop. Wait for shutdown to complete,
		// but allow a second signal to force an immediate exit of this goroutine
		// (the caller can then escalate to os.Exit if the shutdown is wedged).
		select {
		case sig := <-c.Signals():
			c.logger.Warn("received second signal, forcing handler exit", "signal", sig)
		case <-c.shutdownComplete:
		}
	}()
}

func (c *Controller) startParentWatch() {
	go func() {
		// Watch the PARENT, not c.ctx. Since D3 severed the two, c.ctx is only
		// cancelled by the shutdown sequence itself — watching it here would mean
		// this could never be the thing that initiates a stop. The parent's
		// Done() closes on cancellation AND on deadline expiry, so both arrive here
		// and both become a graceful, bounded Stop.
		select {
		case <-c.parent.Done():
			if !c.IsStopping() && !c.IsStopped() {
				// Report the PARENT's cause: c.ctx has not been cancelled yet at
				// this point (the stop below is what cancels it).
				cause := context.Cause(c.parent)
				c.logger.Debug("stopping due to parent context completion", "error", cause)
				c.stopFor(CauseParent, cause, nil)
			}
		case <-c.shutdownComplete:
		}
	}()
}

func (c *Controller) processControlMessages() {
	// Handle control messages until shutdown completes, then exit so the
	// goroutine terminates rather than blocking forever on the channel.
	for {
		select {
		case msg := <-c.Messages():
			if msg == Stop {
				c.logger.Debug("received Stop message")
				c.handleStopMessage()
			}
		case <-c.shutdownComplete:
			return
		}
	}
}

func (c *Controller) handleStopMessage() {
	deadline, ok := c.enterShutdownFromMessage()
	if !ok {
		return
	}

	c.logger.Warn("Stopping Services")

	// Detach OS-signal handling at shutdown so a late signal does not land on a
	// channel no one is reading (D6).
	if c.signals != nil {
		signal.Stop(c.signals)
	}

	// Cancel the controller context so all StartFuncs blocking on
	// ctx.Done() are unblocked before the shutdown timeout fires.
	c.cancel(ErrShutdown)

	// The deadline was recorded at the trigger (spec 0008 D1), and derives from
	// a fresh background context: c.ctx is already cancelled above, so using it
	// as a parent would produce a context that is dead on arrival — causing
	// http.Server.Shutdown to fail immediately instead of draining in-flight
	// connections.
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	// Cancel each async health-check context explicitly. Cancelling c.ctx above
	// already propagates to these derived contexts, but the per-check CancelFunc
	// must still be invoked to release its resources (the Go contract for every
	// context.WithCancel); skipping it leaks the cancellation goroutine until the
	// parent is collected.
	c.cancelHealthChecks()

	unfinished := c.services.stop(ctx)

	// Bound the wait for supervisor exits with the remaining shutdown budget —
	// one deadline covers the whole "bounded shutdown" contract (D10). A
	// StartFunc that ignores cancellation pins its supervisor goroutine (and
	// the wait group) forever; abandon it at the deadline and name it, turning
	// a silent Wait() hang into a diagnosable message.
	for _, name := range c.services.awaitSupervisors(ctx) {
		c.logger.Warn(
			"service StartFunc did not return before the shutdown deadline; abandoning its supervisor goroutine",
			"service_name", name,
		)

		unfinished = append(unfinished, Unfinished{Service: name, Reason: SupervisorAbandoned})
	}

	forwarders := c.cutOffSubscriptions()

	c.stateMutex.Lock()
	c.outcome.Unfinished = unfinished
	c.outcomeWritten = true
	c.state = Stopped
	c.stateMutex.Unlock()

	c.logger.Info("Stopped")

	c.drain(ctx, forwarders)

	// Signal the handler goroutines (context, signal, message processor) that
	// the shutdown sequence is complete so they terminate.
	close(c.shutdownComplete)

	c.wg.Done()
}

// enterShutdownFromMessage makes the transition for a Stop sent directly on
// Messages(), and records CauseStop and a deadline if nothing has: that covers
// the direct send and a consumer that SetState(Stopping) first, which no
// transition saw (spec 0003 D7). It reports the deadline, and false when the
// controller is not Stopping.
func (c *Controller) enterShutdownFromMessage() (time.Time, bool) {
	c.stateMutex.Lock()
	defer c.stateMutex.Unlock()

	c.enterStoppingLocked()

	if c.state != Stopping {
		return time.Time{}, false
	}

	c.recordCause(CauseStop, nil, nil)

	return c.deadline, true
}

// startAsyncHealthChecks launches background goroutines for health checks
// that have a non-zero Interval.
func (c *Controller) startAsyncHealthChecks() {
	for _, entry := range c.healthChecks {
		if entry.check.Interval > 0 {
			c.startAsyncCheck(entry)
		}
	}
}

// cancelHealthChecks invokes each async health check's CancelFunc, releasing the
// per-check context derived in startAsyncCheck. It is best-effort and nil-safe:
// sync checks (and any entry whose async goroutine was never launched) have a nil
// cancel and are skipped.
func (c *Controller) cancelHealthChecks() {
	for _, entry := range c.healthChecks {
		if entry.cancel != nil {
			entry.cancel()
		}
	}
}

func (c *Controller) startAsyncCheck(entry *healthCheckEntry) {
	ctx, cancel := context.WithCancel(c.ctx)
	entry.cancel = cancel

	c.wg.Add(1)

	go func() {
		defer c.wg.Done()

		// Run immediately on start
		entry.runCheck(ctx)

		ticker := time.NewTicker(entry.check.Interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				entry.runCheck(ctx)
			}
		}
	}()
}

// healthCheckStatuses collects ServiceStatus entries from health checks
// matching the given filter function. When failClosed is true (readiness gating),
// an async check whose first run has not yet produced a cached result is reported
// as not-ready rather than defaulting to OK (D7).
func (c *Controller) healthCheckStatuses(filter func(CheckType) bool, failClosed bool) ([]ServiceStatus, bool) {
	var statuses []ServiceStatus

	allHealthy := true

	for _, entry := range c.healthChecks {
		if !filter(entry.check.Type) {
			continue
		}

		r := entry.result(c.ctx)
		s, healthy := toServiceStatus(entry.check.Name, r, failClosed)

		// A stale async cache means the refresh loop is no longer producing fresh
		// results; the cached value cannot be trusted. Surface it as an error in
		// every aggregation (so Status() shows it) and fail readiness closed (D11).
		if entry.stale(r, time.Now()) {
			s.Status = "ERROR"
			s.Error = "cached health result is stale"
			healthy = false
		}

		statuses = append(statuses, s)

		if !healthy {
			allHealthy = false
		}
	}

	return statuses, allHealthy
}

// Status returns an aggregate health report for all registered services and health checks.
func (c *Controller) Status() HealthReport {
	report := c.services.status()
	// Status includes all check types. Not a readiness gate, so do not fail closed,
	// and the lifecycle state is carried as data rather than as a verdict.
	checks, healthy := c.healthCheckStatuses(func(_ CheckType) bool { return true }, false)
	report.Services = append(report.Services, checks...)
	report.State = c.GetState()

	if !healthy {
		report.OverallHealthy = false
	}

	return report
}

// Liveness returns an aggregate liveness report for all registered services and health checks.
func (c *Controller) Liveness() HealthReport {
	report := c.services.liveness()
	checks, healthy := c.healthCheckStatuses(func(ct CheckType) bool {
		return ct == CheckTypeLiveness || ct == CheckTypeBoth
	}, false)
	report.Services = append(report.Services, checks...)
	report.State = c.GetState()

	// Liveness is deliberately NOT gated on the lifecycle state (0003 D2). A
	// liveness probe failing during a graceful shutdown invites the orchestrator
	// to kill a process that is shutting down correctly, destroying the in-flight
	// requests the drain existed to protect.
	if !healthy {
		report.OverallHealthy = false
	}

	return report
}

// Readiness returns an aggregate readiness report for all registered services and health checks.
func (c *Controller) Readiness() HealthReport {
	report := c.services.readiness()
	// Readiness gates traffic: fail closed when an async check has not yet run.
	checks, healthy := c.healthCheckStatuses(func(ct CheckType) bool {
		return ct == CheckTypeReadiness || ct == CheckTypeBoth
	}, true)
	report.Services = append(report.Services, checks...)

	state := c.GetState()
	report.State = state

	// Ready only while Running (0003 D2). Stated positively rather than as a list
	// of failing states, so a state added later is unready by default, which is
	// the safe direction for something that gates traffic.
	//
	// Without this a stopping controller reports ready and go/transport answers
	// HTTP 200. Shutdown is reverse registration order, so a transport server
	// registered early is stopped last and keeps serving while everything it
	// depends on is torn down beneath it.
	if !healthy || state != Running {
		report.OverallHealthy = false
	}

	return report
}

// GetServiceInfo returns the runtime information and statistics for a specific service.
func (c *Controller) GetServiceInfo(name string) (ServiceInfo, bool) {
	if v, ok := c.services.info.Load(name); ok {
		return v.(ServiceInfo), true
	}

	return ServiceInfo{}, false
}

// Compile-time interface satisfaction checks.
var (
	_ Runner              = (*Controller)(nil)
	_ StateAccessor       = (*Controller)(nil)
	_ Configurable        = (*Controller)(nil)
	_ ChannelProvider     = (*Controller)(nil)
	_ Controllable        = (*Controller)(nil)
	_ HealthCheckReporter = (*Controller)(nil)
)

// ControllerOpt is a functional option for configuring a Controller.
type ControllerOpt func(Configurable)

// WithSignals gives the controller ownership of SIGINT/SIGTERM, so the first
// signal drives a graceful Stop.
//
// Signal disposition is process-global, so it belongs to whichever layer is
// outermost — which is why it is opt-in. In a CLI framework that already
// translates signals into context cancellation (as go-tool-base's root command
// does), do NOT use this: the controller observes the parent context instead,
// and a second handler would race the framework's own. Reach for it in a
// standalone main where the controller genuinely is the outermost thing.
func WithSignals() ControllerOpt {
	return func(c Configurable) {
		// Buffered: signal.Notify never blocks, so an unbuffered channel would
		// drop a signal that arrives before the handler goroutine is ready.
		c.SetSignalsChannel(make(chan os.Signal, signalChanBuffer))
	}
}

// WithShutdownTimeout sets the graceful shutdown timeout.
func WithShutdownTimeout(d time.Duration) ControllerOpt {
	return func(c Configurable) {
		c.SetShutdownTimeout(d)
	}
}

// WithLogger sets the controller logger.
func WithLogger(l *slog.Logger) ControllerOpt {
	return func(c Configurable) {
		c.SetLogger(l.With("component", "controller"))
	}
}

// WithValidError registers a predicate that identifies expected terminal errors
// (e.g. http.ErrServerClosed, context.Canceled). The restart supervisor treats a
// matching error as a graceful end-of-run rather than a failure, so it neither
// counts toward the restart total nor is forwarded on the error channel (D7).
func WithValidError(fn ValidErrorFunc) ControllerOpt {
	return func(c Configurable) {
		if vs, ok := c.(validErrorSetter); ok {
			vs.setValidError(fn)
		}
	}
}

// validErrorSetter is satisfied by *Controller and lets WithValidError set the
// predicate through the Configurable option surface without widening the public
// Configurable interface.
type validErrorSetter interface {
	setValidError(fn ValidErrorFunc)
}

func (c *Controller) setValidError(fn ValidErrorFunc) {
	c.validError = fn
}

// publish is how a supervisor reports a failure: logged here, at the source,
// then queued for each consumer without blocking. Nothing reads a channel to
// log, so nothing competes with a consumer for it (spec 0008 D7, issue 19).
func (c *Controller) publish(index int, ev ServiceEvent) {
	c.logger.Error(failureMessage(ev.Kind),
		"service_name", ev.Service, "kind", ev.Kind, "restarts", ev.Restarts, "error", ev.Err)

	c.subMu.Lock()
	fwds := c.activeForwarders()
	c.subMu.Unlock()

	for _, f := range fwds {
		f.enqueue(index, ev)
	}
}

// activeForwarders lists the forwarders that exist, the channel's first so it
// gets the drain budget first. Callers hold subMu.
func (c *Controller) activeForwarders() []*forwarder {
	var fwds []*forwarder

	for _, f := range []*forwarder{c.errsFwd, c.eventsFwd} {
		if f != nil {
			fwds = append(fwds, f)
		}
	}

	return fwds
}

func failureMessage(kind EventKind) string {
	switch kind {
	case EventRetrying:
		return "service failed; restarting"
	case EventFailed:
		return "service failed; restarts exhausted"
	default:
		return "service unable to start"
	}
}

// startForwarders starts the callback's forwarder and, for a subscribed or
// installed channel, the channel's (spec 0008 D8, D9).
func (c *Controller) startForwarders() {
	c.subMu.Lock()
	defer c.subMu.Unlock()

	c.subStarted = true

	if c.onEvent != nil {
		c.eventsFwd = newForwarder(&forwarder{
			consumer: "WithOnEvent",
			deliver: func(ev ServiceEvent, _ <-chan struct{}) bool {
				c.invokeOnEvent(ev)

				return true
			},
		})
	}

	if c.subscribed || c.errs != c.ownErrs {
		c.startErrorsForwarder()
	}
}

// startErrorsForwarder starts the forwarder for whichever channel the controller
// holds. It is that channel's only sender, so for the controller's own channel
// it is also the only goroutine that may close it. Callers hold subMu.
func (c *Controller) startErrorsForwarder() {
	out := c.errs
	if out == nil {
		return
	}

	var onExit func()
	if out == c.ownErrs {
		onExit = func() { close(out) }
	}

	c.errsFwd = newForwarder(&forwarder{
		consumer:          "Errors",
		awaitAfterAbandon: true,
		deliver: func(ev ServiceEvent, abandon <-chan struct{}) bool {
			select {
			case out <- ev.Err:
				return true
			case <-abandon:
				return false
			}
		},
		onExit: onExit,
	})
}

// invokeOnEvent recovers a panicking callback, which has still had its event,
// so the next one is delivered.
func (c *Controller) invokeOnEvent(ev ServiceEvent) {
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error("WithOnEvent callback panicked", "service_name", ev.Service, "kind", ev.Kind, "panic", r)
		}
	}()

	c.onEvent(ev)
}

// cutOffSubscriptions refuses further events and subscriptions, and closes the
// controller's own channel when no forwarder sends on it. It returns the
// forwarders still to drain.
func (c *Controller) cutOffSubscriptions() []*forwarder {
	c.subMu.Lock()
	defer c.subMu.Unlock()

	c.subFinished = true

	if c.ownErrs != nil && (c.errsFwd == nil || c.errs != c.ownErrs) {
		close(c.ownErrs)
	}

	fwds := c.activeForwarders()
	for _, f := range fwds {
		f.closeAdmission()
	}

	return fwds
}

// drain waits for each forwarder to empty its queue within the remaining
// budget, then abandons what is left. A forwarder that honours abandonment is
// still waited for, so nothing is sent after Done (spec 0008 D8).
func (c *Controller) drain(ctx context.Context, fwds []*forwarder) {
	for _, f := range fwds {
		select {
		case <-f.exited:
			continue
		case <-ctx.Done():
		}

		left := f.abandonDelivery()

		if f.awaitAfterAbandon {
			<-f.exited

			left += f.dropped
		}

		if left > 0 {
			c.logger.Warn("events undelivered at the shutdown deadline",
				"consumer", f.consumer, "count", left)
		}
	}
}

// NewController creates a Controller with the given context and options.
//
// It does NOT install an OS signal handler. Signal disposition is process-global
// state and belongs to whichever layer is outermost — typically the CLI framework
// or main. Pass WithSignals when the controller genuinely is that outermost layer.
//
// The caller's context is watched but not inherited for cancellation: its
// completion, by cancel or deadline, triggers a graceful Stop, so every service
// observes ErrShutdown as its context cause. See docs/how-to/graceful-shutdown.md.
func NewController(ctx context.Context, opts ...ControllerOpt) *Controller {
	// Sever cancellation from the caller's context, keeping its values (D3).
	// The parent's completion still stops the services — startParentWatch
	// watches it and drives a graceful Stop — but it does so THROUGH the shutdown
	// sequence, so the cause every service observes is ErrShutdown rather than
	// whatever the parent happened to carry. Deriving directly from the parent
	// would let the parent's cause win the race and silently void the contract
	// documented in docs/how-to/graceful-shutdown.md.
	parent := ctx
	ctx, cancel := context.WithCancelCause(context.WithoutCancel(parent))

	errs := make(chan error)

	c := &Controller{
		ctx:      ctx,
		cancel:   cancel,
		parent:   parent,
		logger:   slog.New(slog.DiscardHandler),
		messages: make(chan Message),
		errs:     errs,
		ownErrs:  errs,
		// nil by default: signal disposition is process-global and belongs to the
		// outermost layer. Opt in with WithSignals (D1/D2).
		signals:          nil,
		wg:               &sync.WaitGroup{},
		shutdownTimeout:  DefaultShutdownTimeout,
		state:            NeverStarted,
		services:         Services{},
		healthChecks:     make(map[string]*healthCheckEntry),
		shutdownComplete: make(chan struct{}),
	}

	for _, opt := range opts {
		opt(c)
	}

	// OS-signal registration is deferred to Start (startSignalHandler), where it is
	// paired with the reader goroutine. Registering here would leave a controller
	// that is constructed but never started swallowing SIGINT/SIGTERM with no
	// reader (F5).

	return c
}

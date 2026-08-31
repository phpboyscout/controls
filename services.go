package controls

import (
	"context"
	"sync"
	"time"

	"gitlab.com/phpboyscout/go/errors"
)

const (
	defaultInitialBackoff = 1 * time.Second
	defaultMaxBackoff     = 30 * time.Second
	defaultHealthInterval = 10 * time.Second
	backoffMultiplier     = 2.0

	// DefaultRestartResetInterval is the duration a service must run healthily
	// before its consecutive-failure restart counter resets to zero.
	DefaultRestartResetInterval = 30 * time.Second
)

// runOutcome classifies the result of a single service run for the supervisor.
type runOutcome int

const (
	// outcomeCleanStart means Start returned nil — the service either completed
	// cleanly or, more commonly, serves in a background goroutine. It is NOT an
	// exit and is never restarted; such services are supervised via health checks.
	outcomeCleanStart runOutcome = iota
	// outcomeCancelled means the run ended because the controller context was
	// cancelled (graceful shutdown). It never triggers a restart.
	outcomeCancelled
	// outcomeError means Start returned a non-nil, non-valid error while the
	// context was still live. Only this outcome may trigger a restart.
	outcomeError
)

// noopStop is the default StopFunc used when a service registers none.
func noopStop(context.Context) {}

// noopStart is the default StartFunc used when a service registers none.
func noopStart(context.Context) error { return nil }

// Services manages the collection of registered services and their lifecycle.
type Services struct {
	mu         sync.Mutex
	services   []Service
	info       sync.Map // map[string]ServiceInfo
	validError ValidErrorFunc
	// onUnableToStart is called when a service has failed without ever starting
	// cleanly and has exhausted its restart policy, so it will never start. Set
	// by the controller at Start, alongside validError.
	onUnableToStart func(name string, err error)
	// exits pairs each launched supervisor goroutine with the channel closed
	// when it returns, so the shutdown sequence can bound its wait for
	// supervisor exit and name any service whose StartFunc never returned (D10).
	exits []supervisorExit
}

// supervisorExit records the exit channel for a single supervisor goroutine.
// The channel is closed when the goroutine returns.
type supervisorExit struct {
	name   string
	exited chan struct{}
}

func (q *Services) add(s Service) {
	q.mu.Lock()
	defer q.mu.Unlock()

	// D5: default missing lifecycle funcs to no-ops so the supervisor and the
	// shutdown sequence never dereference a nil func.
	if s.Start == nil {
		s.Start = noopStart
	}

	if s.Stop == nil {
		s.Stop = noopStop
	}

	q.services = append(q.services, s)
	q.info.Store(s.Name, ServiceInfo{Name: s.Name})
}

// classifyRun determines the outcome of a single Start invocation. The validError
// predicate (if set) exempts expected terminal errors (e.g. http.ErrServerClosed)
// from being treated as failures.
func (q *Services) classifyRun(ctx context.Context, err error) runOutcome {
	return classifyOutcome(ctx, err, q.validError)
}

// classifyOutcome is the rule itself, with no receiver, so a Supervisor's
// children read it rather than reimplementing it. Note the second clause: a
// returned context.Canceled counts as a cancellation even when ctx is still
// live, because a unit that reports itself cancelled is not failing.
func classifyOutcome(ctx context.Context, err error, valid ValidErrorFunc) runOutcome {
	if err == nil {
		return outcomeCleanStart
	}

	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return outcomeCancelled
	}

	if valid != nil && valid(err) {
		return outcomeCancelled
	}

	return outcomeError
}

// restartsExhausted reports whether a unit has used up its restart allowance.
//
// MaxRestarts <= 0 means UNLIMITED, not none. Writing it the other way round is
// precisely the divergence sharing RestartPolicy exists to prevent, and the
// first draft of the supervisor's loop had it. One copy, two callers.
func restartsExhausted(policy *RestartPolicy, restarts int) bool {
	return policy.MaxRestarts > 0 && restarts >= policy.MaxRestarts
}

// monitorHealth supervises a background-serving service via its Status probe. It
// returns true if it ended because the health-failure threshold was breached (a
// restart-worthy condition), or false if it ended because the context was
// cancelled or there is nothing to monitor (in which case the service is supervised
// purely by its clean start and should simply wait for shutdown).
func (q *Services) monitorHealth(ctx context.Context, srv Service, updateInfo func(func(*ServiceInfo))) bool {
	if srv.RestartPolicy.HealthFailureThreshold <= 0 || srv.Status == nil {
		// Nothing to monitor: a clean start is not an exit, so block until the
		// controller shuts down rather than falling through to a restart.
		<-ctx.Done()

		return false
	}

	healthInterval := srv.RestartPolicy.HealthCheckInterval
	if healthInterval == 0 {
		healthInterval = defaultHealthInterval
	}

	healthFailures := 0

	for {
		select {
		case <-time.After(healthInterval):
			if err := srv.Status(); err != nil {
				healthFailures++
				if healthFailures >= srv.RestartPolicy.HealthFailureThreshold {
					stopErr := callStop(ctx, srv)

					updateInfo(func(i *ServiceInfo) {
						i.Error = errors.Wrap(err, "health check failed")
						i.StopErr = stopErr
					})

					return true
				}
			} else {
				healthFailures = 0 // Reset on success
			}
		case <-ctx.Done():
			return false
		}
	}
}

// sendErr forwards err on errs unless shutdown has already completed (D9). Once
// handleStopMessage closes done, the error/context handler has exited and there
// is no receiver, so an unguarded send would block the supervisor goroutine
// forever. The guard makes every forward provably non-blocking.
func sendErr(done <-chan struct{}, errs chan error, err error) {
	select {
	case errs <- err:
	case <-done:
	}
}

func (q *Services) start(ctx context.Context, wg *sync.WaitGroup, errChan chan error, done <-chan struct{}) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for _, s := range q.services {
		exited := make(chan struct{})
		q.exits = append(q.exits, supervisorExit{name: s.Name, exited: exited})

		go func(s Service, exited chan struct{}) {
			defer close(exited)

			q.supervise(ctx, s, errChan, wg, done)
		}(s, exited)
	}
}

// awaitSupervisors blocks until every supervisor goroutine has exited or ctx is
// done, whichever comes first. It returns the names of services whose
// supervisors had not exited by the deadline — i.e. whose StartFuncs ignored
// cancellation. The stuck goroutines are abandoned rather than force-drained:
// decrementing the wait group on their behalf would race a late-returning
// Start into a double-decrement panic (D10).
func (q *Services) awaitSupervisors(ctx context.Context) []string {
	q.mu.Lock()
	exits := make([]supervisorExit, len(q.exits))
	copy(exits, q.exits)
	q.mu.Unlock()

	var stuck []string

	for _, e := range exits {
		select {
		case <-e.exited:
		case <-ctx.Done():
			// Deadline reached (or already elapsed when we got here). A final
			// non-blocking check distinguishes "exited at the same instant"
			// from genuinely stuck, so a service is never reported abandoned
			// after its supervisor actually returned.
			select {
			case <-e.exited:
			default:
				stuck = append(stuck, e.name)
			}
		}
	}

	return stuck
}

func (q *Services) supervise(ctx context.Context, srv Service, errs chan error, wg *sync.WaitGroup, done <-chan struct{}) {
	started := false

	markStarted := func() {
		if !started {
			wg.Done()

			started = true
		}
	}
	defer markStarted() // ensure wg is decremented if we exit early

	// Deliberately NOT the `started` flag above: that one is a wg.Done guard and
	// is fired by the defer on every exit, including a failing one. This records
	// whether the service ever actually started cleanly, which is half of what
	// UnableToStart requires (D4).
	cleanStart := false

	updateInfo := func(update func(*ServiceInfo)) {
		if v, ok := q.info.Load(srv.Name); ok {
			info := v.(ServiceInfo)
			update(&info)
			q.info.Store(srv.Name, info)
		}
	}

	if srv.RestartPolicy == nil {
		q.runOnce(ctx, srv, errs, updateInfo, done)

		return
	}

	q.runWithRestartPolicy(ctx, srv, errs, markStarted, &cleanStart, updateInfo, done)
}

func (q *Services) runOnce(ctx context.Context, srv Service, errs chan error, updateInfo func(func(*ServiceInfo)), done <-chan struct{}) {
	updateInfo(func(i *ServiceInfo) { i.LastStarted = time.Now() })

	err := srv.Start(ctx)

	updateInfo(func(i *ServiceInfo) {
		i.LastStopped = time.Now()
		i.Error = err
	})

	// Only forward genuine errors; a clean start, a cancellation, or a valid
	// terminal error (e.g. http.ErrServerClosed) is not a failure.
	if q.classifyRun(ctx, err) == outcomeError {
		// A service with no restart policy has no retries, so its first genuine
		// error is already exhaustion, and it never started cleanly (that would
		// have classified as outcomeCleanStart). Both halves of D4 hold here on
		// the first failure, which is easy to miss when the restart loop is the
		// code being reasoned about.
		if q.onUnableToStart != nil {
			q.onUnableToStart(srv.Name, err)
		}

		sendErr(done, errs, err)
	}
}

// reportExhausted records and forwards the terminal failure of a service that
// has run out of restarts.
//
// cleanStart is whether it ever started cleanly. Never having done so, having
// now exhausted its policy, is what UnableToStart means (0003 D4): the service
// will never start. A service that DID start cleanly and failed later is an
// ordinary failure, however many restarts it then used up.
func (q *Services) reportExhausted(srv Service, err error, cleanStart bool,
	updateInfo func(func(*ServiceInfo)), errs chan error, done <-chan struct{},
) {
	finalErr := errors.New("max restarts exceeded")
	if err != nil {
		finalErr = errors.Wrap(err, "max restarts exceeded")
	}

	updateInfo(func(i *ServiceInfo) { i.Error = finalErr })

	if !cleanStart && q.onUnableToStart != nil {
		q.onUnableToStart(srv.Name, finalErr)
	}

	sendErr(done, errs, finalErr)
}

func calculateNextBackoff(current, max time.Duration) time.Duration {
	next := time.Duration(float64(current) * backoffMultiplier)
	if next > max || next < 0 {
		return max
	}

	return next
}

// restartTimings holds the resolved backoff/reset parameters for a restart loop.
type restartTimings struct {
	backoff       time.Duration
	maxBackoff    time.Duration
	resetInterval time.Duration
}

// initialBackoff resolves the starting backoff for a policy, applying the default
// when unset. Shared by the initial timings and the post-healthy-run reset.
func initialBackoff(p *RestartPolicy) time.Duration {
	if p.InitialBackoff == 0 {
		return defaultInitialBackoff
	}

	return p.InitialBackoff
}

func resolveRestartTimings(p *RestartPolicy) restartTimings {
	t := restartTimings{
		backoff:       initialBackoff(p),
		maxBackoff:    p.MaxBackoff,
		resetInterval: p.RestartResetInterval,
	}

	if t.maxBackoff == 0 {
		t.maxBackoff = defaultMaxBackoff
	}

	if t.resetInterval == 0 {
		t.resetInterval = DefaultRestartResetInterval
	}

	return t
}

// runOnceWithRestart performs a single supervised run. It returns the Start error
// and whether the restart loop should keep going (false means terminate: graceful
// shutdown or a clean start with no exit).
func (q *Services) runOnceWithRestart(ctx context.Context, srv Service, markStarted func(), cleanStart *bool, updateInfo func(func(*ServiceInfo))) (error, bool) {
	updateInfo(func(i *ServiceInfo) { i.LastStarted = time.Now() })

	err := srv.Start(ctx)

	updateInfo(func(i *ServiceInfo) {
		i.LastStopped = time.Now()
		i.Error = err
	})

	switch q.classifyRun(ctx, err) {
	case outcomeCancelled:
		// Graceful shutdown (or an expected terminal error). Never restart.
		return err, false
	case outcomeCleanStart:
		// Start returned nil: the service serves in the background. Mark it
		// started and supervise it via its health check. monitorHealth blocks
		// until the context is cancelled (shutdown) or the health threshold is
		// breached. A clean start that is not health-failed is never an exit.
		markStarted()

		*cleanStart = true

		return err, q.monitorHealth(ctx, srv, updateInfo)
	default: // outcomeError
		return err, true
	}
}

func (q *Services) runWithRestartPolicy(ctx context.Context, srv Service, errs chan error, markStarted func(), cleanStart *bool, updateInfo func(func(*ServiceInfo)), done <-chan struct{}) {
	restarts := 0
	timings := resolveRestartTimings(srv.RestartPolicy)

	for {
		runStarted := time.Now()

		err, keepGoing := q.runOnceWithRestart(ctx, srv, markStarted, cleanStart, updateInfo)
		if !keepGoing {
			return
		}

		// The run ended in a failure (Start error or health breach). If it ran
		// healthily for long enough, reset both the consecutive-failure counter
		// and the backoff (F6a) — otherwise a service healthy for hours still
		// waits the accumulated MaxBackoff before its next restart.
		if time.Since(runStarted) >= timings.resetInterval {
			restarts = 0
			timings.backoff = initialBackoff(srv.RestartPolicy)
		}

		// Check if we've exhausted restarts.
		if restartsExhausted(srv.RestartPolicy, restarts) {
			q.reportExhausted(srv, err, *cleanStart, updateInfo, errs, done)

			return
		}

		restarts++

		updateInfo(func(i *ServiceInfo) { i.RestartCount = restarts })

		// Never send nil on errs (errors.Wrap(nil,...) returns nil). A health
		// failure stores its error via monitorHealth/updateInfo; only forward a
		// non-nil Start error here.
		if err != nil {
			sendErr(done, errs, err)
		}

		// Wait for backoff or cancellation.
		select {
		case <-time.After(timings.backoff):
			timings.backoff = calculateNextBackoff(timings.backoff, timings.maxBackoff)

			continue
		case <-ctx.Done():
			return
		}
	}
}

// stop shuts services down in reverse registration order, one at a time. Each
// StopFunc runs in its own goroutine and is awaited against ctx.Done(): a
// context-ignoring stop is abandoned at the shutdown deadline rather than hanging
// the caller (and Wait()) forever. The abandoned goroutine is left to finish on
// its own. Returns the number of services.
//
// The service slice is snapshotted under the lock and the lock is then released
// for the whole stop sequence (D12). Holding q.mu while awaiting every StopFunc —
// up to the entire shutdown timeout — would block status()/liveness()/readiness()
// on the same mutex, so every health probe would hang exactly when a load
// balancer most needs a prompt not-ready answer. Registration is impossible once
// the controller is Stopping, so the snapshot cannot go stale.
// recordStopErr stores how a stop ended, for a service the caller may no longer
// be holding.
func (q *Services) recordStopErr(name string, stopErr error) {
	if v, ok := q.info.Load(name); ok {
		info, _ := v.(ServiceInfo)
		info.StopErr = stopErr
		q.info.Store(name, info)
	}
}

func (q *Services) stop(ctx context.Context) int {
	q.mu.Lock()
	services := make([]Service, len(q.services))
	copy(services, q.services)
	q.mu.Unlock()

	for i := len(services) - 1; i >= 0; i-- {
		s := services[i]

		done := make(chan struct{})

		go func() {
			defer close(done)

			if stopErr := callStop(ctx, s); stopErr != nil {
				q.recordStopErr(s.Name, stopErr)
			}
		}()

		select {
		case <-done:
			// Stop completed within the remaining deadline.
		case <-ctx.Done():
			// Deadline reached: abandon this stop and move on to the next service.
			// Remaining stops still get a best-effort attempt, but with the
			// deadline already elapsed their own ctx.Done() fires immediately.
			// The abandoned goroutine is left to finish on its own.
		}
	}

	return len(services)
}

// callStop invokes a StopFunc, recovering from a panic so a misbehaving stop
// cannot crash the shutdown sequence. fn is never nil (defaulted at registration).
// callStop stops a service and reports how it ended.
//
// A panic is contained and converted rather than swallowed. It used to be
// swallowed here and not contained at all on the health-restart path, which
// called Stop directly — so the same defect either vanished or killed the
// process depending on which path reached it.
func callStop(ctx context.Context, s Service) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errors.Newf("controls: stopping %q panicked: %v", s.Name, r)
		}
	}()

	// Both set is a mistake rather than something to merge: calling a service's
	// stop twice is worse than ignoring the half that cannot report.
	if s.StopErr != nil {
		return s.StopErr(ctx)
	}

	return callStopFunc(ctx, s.Stop)
}

// callStopFunc runs a plain StopFunc, which cannot report anything, and
// contains a panic in it.
//
// It exists because a Supervisor's Child carries a bare StopFunc: a child is
// not a Service and giving it one to satisfy this signature would be the tail
// wagging the dog.
func callStopFunc(ctx context.Context, fn StopFunc) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errors.Newf("controls: stop panicked: %v", r)
		}
	}()

	if fn != nil {
		fn(ctx)
	}

	return nil
}

// callProbe calls fn and returns any error it produces. If fn panics, the panic
// value is converted to an error so that a misbehaving StatusFunc or ProbeFunc
// cannot crash the server.
func callProbe(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errors.Newf("probe panicked: %v", r)
		}
	}()

	return fn()
}

func (q *Services) status() HealthReport {
	q.mu.Lock()
	defer q.mu.Unlock()

	report := HealthReport{
		OverallHealthy: true,
		Services:       make([]ServiceStatus, 0, len(q.services)),
	}

	for _, s := range q.services {
		status := ServiceStatus{
			Name:   s.Name,
			Status: "OK",
		}

		if s.Status != nil {
			if err := callProbe(s.Status); err != nil {
				status.Status = "ERROR"
				status.Error = err.Error()
				report.OverallHealthy = false
			}
		}

		report.Services = append(report.Services, status)
	}

	return report
}

func (q *Services) liveness() HealthReport {
	q.mu.Lock()
	defer q.mu.Unlock()

	report := HealthReport{
		OverallHealthy: true,
		Services:       make([]ServiceStatus, 0, len(q.services)),
	}

	for _, s := range q.services {
		status := ServiceStatus{
			Name:   s.Name,
			Status: "OK",
		}

		var err error
		if s.Liveness != nil {
			err = callProbe(s.Liveness)
		} else if s.Status != nil {
			err = callProbe(s.Status)
		}

		if err != nil {
			status.Status = "ERROR"
			status.Error = err.Error()
			report.OverallHealthy = false
		}

		report.Services = append(report.Services, status)
	}

	return report
}

func (q *Services) readiness() HealthReport {
	q.mu.Lock()
	defer q.mu.Unlock()

	report := HealthReport{
		OverallHealthy: true,
		Services:       make([]ServiceStatus, 0, len(q.services)),
	}

	for _, s := range q.services {
		status := ServiceStatus{
			Name:   s.Name,
			Status: "OK",
		}

		var err error
		if s.Readiness != nil {
			err = callProbe(s.Readiness)
		} else if s.Status != nil {
			err = callProbe(s.Status)
		}

		if err != nil {
			status.Status = "ERROR"
			status.Error = err.Error()
			report.OverallHealthy = false
		}

		report.Services = append(report.Services, status)
	}

	return report
}

// Service represents a managed background service with start/stop lifecycle,
// health probes, and optional restart policy.
type Service struct {
	Name          string
	Start         StartFunc
	Stop          StopFunc
	StopErr       StopErrFunc
	Status        StatusFunc
	Liveness      ProbeFunc
	Readiness     ProbeFunc
	RestartPolicy *RestartPolicy
}

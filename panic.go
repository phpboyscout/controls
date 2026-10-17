package controls

import (
	"context"
	"log/slog"
	"runtime/debug"

	"gitlab.com/phpboyscout/go/errors"
)

// ErrPanicked marks an error converted from a recovered panic. The error's
// origin stack (errors.OriginStack) is the panic site's.
var ErrPanicked = errors.NewSentinel("controls.panicked", "panicked")

// PanicSite names what the controller or a Supervisor was calling when it
// recovered a panic.
type PanicSite string

const (
	// SiteStart is a service's or a child's start function.
	SiteStart PanicSite = "start"
	// SiteStop is a service's or a child's stop function.
	SiteStop PanicSite = "stop"
	// SiteProbe is a Status, Liveness or Readiness probe, polled or called
	// through a report.
	SiteProbe PanicSite = "probe"
	// SiteHealthCheck is a standalone HealthCheck's Check.
	SiteHealthCheck PanicSite = "health_check"
)

// PanicReport describes one recovered panic.
type PanicReport struct {
	// Service is the service, health check or child that panicked.
	Service string
	Site    PanicSite
	// Value is the recovered value, as an error tracker's recover API takes it.
	Value any
	// Err wraps ErrPanicked and carries the panic site's stack.
	Err error
	// Repeats counts further panics at this site merged into this report while
	// it waited; Value and Err are the latest.
	Repeats int
}

// WithOnPanic registers a callback for every panic the controller recovers
// from Start until shutdown's cutoff, delivered on a goroutine of its own. It is
// in addition to the failure the panic causes, never instead of it; see
// https://controls.go.phpboyscout.uk/reference/controller/#panics.
func WithOnPanic(fn func(PanicReport)) ControllerOpt {
	return func(c Configurable) {
		if s, ok := c.(onPanicSetter); ok {
			s.setOnPanic(fn)
		}
	}
}

// WithProbePanicsAsFailures counts a panic in a polled Status as one health
// failure towards the threshold, where by default it breaches at once.
func WithProbePanicsAsFailures() ControllerOpt {
	return func(c Configurable) {
		if s, ok := c.(probePanicsSetter); ok {
			s.setProbePanicsAsFailures()
		}
	}
}

// onPanicSetter and probePanicsSetter reach the controller without widening
// Configurable, as validErrorSetter does.
type onPanicSetter interface {
	setOnPanic(fn func(PanicReport))
}

type probePanicsSetter interface {
	setProbePanicsAsFailures()
}

func (c *Controller) setOnPanic(fn func(PanicReport)) {
	c.onPanic = fn
}

func (c *Controller) setProbePanicsAsFailures() {
	c.probePanicsAsFailures = true
}

// panicReporter hands a recovered panic and the error built from it to a
// consumer. A nil one reports nowhere.
type panicReporter func(value any, err error)

func (p panicReporter) report(value any, err error) {
	if p != nil {
		p(value, err)
	}
}

// panicKey is what reports merge on. reporter is a registration index, a
// *healthCheckEntry or a *supervisedChild: names may repeat, these do not.
type panicKey struct {
	reporter any
	site     PanicSite
}

type panicForwarder = forwarder[panicKey, PanicReport]

// newPanicForwarder coalesces a waiting report with later panics at its key,
// which bounds the queue by reporters and sites (spec 0009 D11).
func newPanicForwarder(consumer string, deliver func(PanicReport)) *panicForwarder {
	return newForwarder(&panicForwarder{
		consumer:  consumer,
		deliver:   unabandonable(deliver),
		mergeable: func(PanicReport) bool { return true },
		merge: func(waiting *PanicReport, next PanicReport) {
			waiting.Value, waiting.Err = next.Value, next.Err
			waiting.Repeats += 1 + next.Repeats
		},
	})
}

// logCallbackPanic logs a consumer callback's panic with its stack. It runs
// inside a deferred recover, so a handler that panics here would end the
// process; its panic is discarded, since the logger is what failed (spec 0011
// D5).
func logCallbackPanic(l *slog.Logger, msg string, args ...any) {
	defer func() { _ = recover() }()

	if !l.Enabled(context.Background(), slog.LevelError) {
		return
	}

	l.Error(msg, append(args, "stack", string(debug.Stack()))...)
}

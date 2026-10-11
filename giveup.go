package controls

import "time"

const (
	defaultDeathWatchInterval  = defaultHealthInterval
	defaultDeathWatchThreshold = 3
)

// CompletionPolicy says which completion of a service declared with
// [WithRunToCompletion] ends the controller.
type CompletionPolicy int

const (
	// CompleteOnFirst ends the controller on the first declared service to
	// complete. It is the zero value, and the default.
	CompleteOnFirst CompletionPolicy = iota
	// CompleteOnAll ends it when the last declared service still outstanding
	// completes.
	CompleteOnAll
)

// WithStopOnGiveUp stops the controller with CauseGaveUp when a registered
// service fails for good, and watches a service with a Status and no restart
// policy for a death after a clean start; see
// https://controls.go.phpboyscout.uk/how-to/stop-on-give-up/. Ignored on a
// Configurable that is not a *Controller.
func WithStopOnGiveUp() ControllerOpt {
	return func(c Configurable) {
		if s, ok := c.(giveUpSetter); ok {
			s.setStopOnGiveUp()
		}
	}
}

// WithDeathWatch sets the poll interval and the consecutive failures of the
// death watch WithStopOnGiveUp runs, 10s and 3 by default; a zero or negative
// value keeps its default. On its own it does nothing.
func WithDeathWatch(interval time.Duration, threshold int) ControllerOpt {
	return func(c Configurable) {
		if s, ok := c.(giveUpSetter); ok {
			s.setDeathWatch(deathWatch{interval: interval, threshold: threshold})
		}
	}
}

// WithCompletion sets which completion of a WithRunToCompletion service ends
// the controller. An unknown value acts as CompleteOnFirst, with a warning at
// Start.
func WithCompletion(p CompletionPolicy) ControllerOpt {
	return func(c Configurable) {
		if s, ok := c.(giveUpSetter); ok {
			s.setCompletion(p)
		}
	}
}

// WithRunToCompletion declares the service a unit of work: its Start blocks
// while the work runs, a nil return with the controller live stops the
// controller with CauseCompleted, and failing for good stops it with
// CauseGaveUp. A service with no WithStart completes at once.
func WithRunToCompletion() ServiceOption {
	return func(s *Service) {
		s.runToCompletion = true
	}
}

// giveUpSetter reaches the controller without widening Configurable, as
// validErrorSetter does (spec 0010 D8).
type giveUpSetter interface {
	setStopOnGiveUp()
	setDeathWatch(w deathWatch)
	setCompletion(p CompletionPolicy)
}

func (c *Controller) setStopOnGiveUp()                 { c.stopOnGiveUp = true }
func (c *Controller) setDeathWatch(w deathWatch)       { c.deathWatch = w }
func (c *Controller) setCompletion(p CompletionPolicy) { c.completion = p }

type deathWatch struct {
	interval  time.Duration
	threshold int
}

func (w deathWatch) resolved() deathWatch {
	if w.interval <= 0 {
		w.interval = defaultDeathWatchInterval
	}

	if w.threshold <= 0 {
		w.threshold = defaultDeathWatchThreshold
	}

	return w
}

// endFor is how a service's outcome stops the controller. Losing to an
// earlier trigger is silent: it is not a misuse, as a late Stop is.
func (c *Controller) endFor(cause StopCause, err error, service string) {
	c.stopFor(trigger{cause: cause, err: err, service: service})
}

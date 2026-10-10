package controls

import (
	"os"
	"slices"

	errors "gitlab.com/phpboyscout/go/errors"
)

// StopCause is what started a controller's shutdown.
//
// A string type, like [State], so it reads in a log, is stable in JSON, and can
// grow without breaking a switch that has a default.
type StopCause string

const (
	// CauseStop is a call to Stop, or a Stop sent on the message channel.
	CauseStop StopCause = "stop"
	// CauseSignal is the first signal on a channel WithSignals owns.
	CauseSignal StopCause = "signal"
	// CauseParent is the completion of the context given to NewController, by
	// cancellation or deadline.
	CauseParent StopCause = "parent"
	// CauseGaveUp is a service that failed for good. Reserved: nothing produces it
	// until stopping on a terminal failure lands (issue 15).
	CauseGaveUp StopCause = "gave_up"
	// CauseCompleted is a service whose work finished. Reserved: nothing produces
	// it until run-to-completion lands (issue 17).
	CauseCompleted StopCause = "completed"
)

// UnfinishedReason says why a shutdown step did not finish cleanly.
type UnfinishedReason string

const (
	// StopAbandoned is a stop that had budget left when it was called and had not
	// been seen to return when the budget expired.
	StopAbandoned UnfinishedReason = "stop_abandoned"
	// StopUnbudgeted is a stop whose turn came after the budget had expired. It is
	// still called, best effort, and not awaited.
	StopUnbudgeted UnfinishedReason = "stop_unbudgeted"
	// StopFailed is a stop seen to return an error, or to panic, inside the
	// budget. At the deadline instant a stop that returns because the deadline
	// fired may be reported as this or as StopAbandoned.
	StopFailed UnfinishedReason = "stop_failed"
	// SupervisorAbandoned is a service's supervising goroutine that had not exited
	// by the deadline: usually a Start that ignores cancellation, also a Status
	// probe or a health-triggered stop that does.
	SupervisorAbandoned UnfinishedReason = "supervisor_abandoned"
)

// ErrStopAbandoned is the StopErr of a service whose stop was abandoned at the
// shutdown deadline, or not awaited because the budget had already gone.
var ErrStopAbandoned = errors.NewSentinel("controls.stop_abandoned", "stop not awaited within the shutdown budget")

// Unfinished is one shutdown step that did not finish cleanly.
type Unfinished struct {
	Service string
	Reason  UnfinishedReason
	// Err is the stop's error, for StopFailed.
	Err error
}

// Outcome is how a controller's shutdown went.
type Outcome struct {
	Cause StopCause
	// Err is what the trigger carried: context.Cause of the parent for
	// CauseParent, nil otherwise.
	Err error
	// Signal is set for CauseSignal.
	Signal os.Signal
	// Service is the service that caused the stop, for CauseGaveUp and
	// CauseCompleted.
	Service string
	// Unfinished lists each shutdown step that did not finish cleanly, in the
	// order shutdown reached them. A service can appear more than once.
	Unfinished []Unfinished
}

// Complete reports whether every shutdown step finished cleanly.
func (o Outcome) Complete() bool { return len(o.Unfinished) == 0 }

// Outcome returns how the controller's shutdown went. ok is false until the
// controller's own shutdown sequence has written it, which happens as it
// reaches Stopped; a SetState(Stopped) from outside writes nothing.
func (c *Controller) Outcome() (o Outcome, ok bool) {
	c.stateMutex.Lock()
	defer c.stateMutex.Unlock()

	if !c.outcomeWritten {
		return Outcome{}, false
	}

	o = c.outcome
	o.Unfinished = slices.Clone(c.outcome.Unfinished)

	return o, true
}

// Done is closed when the shutdown sequence has finished, whatever triggered
// it, including delivering queued events (WithOnEvent, Errors). It is bounded by
// the shutdown timeout measured from the trigger, and stays open before Start.
func (c *Controller) Done() <-chan struct{} {
	return c.shutdownComplete
}

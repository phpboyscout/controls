package controls

import "gitlab.com/phpboyscout/go/errors"

// DefaultFailureBufferSize bounds the channel returned by
// [Supervisor.Failures].
//
// Bounded because a supervisor that blocks on an undrained notification channel
// has let its reporting stall the thing it reports on. Sixteen is generous for
// terminal failures, which are rare by construction — a child reaches one only
// after exhausting its restart policy.
const DefaultFailureBufferSize = 16

// ErrSupervisorNotStarted is returned by [Supervisor.Readiness] before Start.
var ErrSupervisorNotStarted = errors.NewSentinel("controls.supervisor_not_started",
	"controls: the supervisor has not started")

// ErrChildAttached is returned when a name is already in use.
var ErrChildAttached = errors.NewSentinel("controls.child_attached",
	"controls: a child is already attached under that name")

// ErrChildNotAttached is returned when a name is not known.
var ErrChildNotAttached = errors.NewSentinel("controls.child_not_attached",
	"controls: no child is attached under that name")

// ErrDetachTimeout is returned when a child outlives the context given to
// [Supervisor.Detach].
//
// The child is still forgotten — it is gone from the supervisor's bookkeeping
// either way. This error is how the caller learns that its goroutine had not
// yet returned, which is the whole difference between a bounded detach and a
// fire-and-forget one.
var ErrDetachTimeout = errors.NewSentinel("controls.detach_timeout",
	"controls: the child was still running when the detach budget expired")

// Child is one supervised unit.
//
// Unlike a [Service], a child is not a requirement for the process to operate:
// see [Supervisor] for what that distinction buys.
type Child struct {
	// Name identifies the child within its supervisor.
	Name string

	// Start runs the child. Returning nil means it finished cleanly and will
	// not be restarted; returning an error starts the restart policy.
	Start StartFunc

	// Stop is called when the child is detached or the supervisor stops. It is
	// optional: cancelling the context passed to Start is the primary mechanism.
	Stop StopFunc

	// RestartPolicy governs restarts. Nil means never restart: the child runs
	// once and its outcome is final.
	//
	// A non-nil policy follows a Service's rules exactly, including that
	// MaxRestarts <= 0 means UNLIMITED rather than none. The same type is used
	// deliberately: a caller should configure restart behaviour one way whether
	// the thing being restarted is registered or attached, and two rules with
	// the same fields is the divergence nobody notices until they disagree.
	RestartPolicy *RestartPolicy
}

// ChildState is what a supervisor can say about a child.
type ChildState string

const (
	// ChildRunning means the child's Start has not returned.
	ChildRunning ChildState = "running"

	// ChildFailed means the child exhausted its restart policy.
	ChildFailed ChildState = "failed"

	// ChildStopped means the child returned cleanly or was stopped.
	ChildStopped ChildState = "stopped"
)

// ChildStatus is a child's observable state.
type ChildStatus struct {
	State    ChildState
	Restarts int
	Panics   int
	LastErr  error
}

// Failure is a child that has exhausted its restart policy.
//
// It is delivered to the consumer because whether that child mattered is the
// consumer's judgement, not the supervisor's.
type Failure struct {
	// Name is the child that failed.
	Name string

	// Err is the error it last returned, wrapped to say restarts were exhausted.
	Err error

	// Restarts is how many were attempted before giving up.
	Restarts int

	// Panicked reports whether the last run ended in a recovered panic rather
	// than a returned error. A bug and a failure are different things, and a
	// consumer deciding how to proceed wants to know which it has.
	Panicked bool
}

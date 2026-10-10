package controls

import (
	"sync"
)

// EventKind is the kind of a [ServiceEvent].
type EventKind string

const (
	// EventRetrying is a failed run that the restart policy will run again.
	EventRetrying EventKind = "retrying"
	// EventFailed is terminal: the service started cleanly at least once, then
	// failed and exhausted its policy.
	EventFailed EventKind = "failed"
	// EventUnableToStart is terminal: the service never started cleanly and will
	// not be tried again. It is the per-service half of the UnableToStart state.
	EventUnableToStart EventKind = "unable_to_start"
)

// ServiceEvent is a failure of one registered service.
type ServiceEvent struct {
	Service string
	Kind    EventKind
	Err     error
	// Restarts is the service's consecutive-restart count: for EventRetrying,
	// including the restart this event announces; for a terminal event, the
	// count it gave up at. It resets after a healthy run, as the policy does.
	Restarts int
}

// WithOnEvent registers a callback for every terminal service failure and the
// latest of each run of retries, delivered in order on a goroutine of its own.
// Shutdown waits for it within its budget; see
// https://controls.go.phpboyscout.uk/reference/controller/#service-events.
func WithOnEvent(fn func(ServiceEvent)) ControllerOpt {
	return func(c Configurable) {
		if s, ok := c.(onEventSetter); ok {
			s.setOnEvent(fn)
		}
	}
}

// onEventSetter lets WithOnEvent reach the controller without widening
// Configurable, as validErrorSetter does.
type onEventSetter interface {
	setOnEvent(fn func(ServiceEvent))
}

func (c *Controller) setOnEvent(fn func(ServiceEvent)) {
	c.onEvent = fn
}

type queuedEvent struct {
	index int // registration index; merging is per service, never per name
	event ServiceEvent
}

// forwarder is the only deliverer to one consumer, fed by a queue that never
// blocks its producers (spec 0008 D8).
type forwarder struct {
	mu        sync.Mutex
	queue     []*queuedEvent
	waiting   map[int]*queuedEvent // an unclaimed retry, by registration index
	closed    bool
	abandoned bool
	// dropped is an event claimed and then abandoned mid-delivery. Read only
	// after exited.
	dropped int

	wake    chan struct{}
	abandon chan struct{}
	exited  chan struct{}

	consumer string // names the consumer in the undelivered-events warning
	// awaitAfterAbandon is set when deliver honours abandon, so the forwarder
	// stops promptly and must be waited for before Done.
	awaitAfterAbandon bool

	// deliver hands one event to the consumer, and reports false if delivery
	// was abandoned instead.
	deliver func(ev ServiceEvent, abandon <-chan struct{}) bool
	onExit  func()
}

func newForwarder(f *forwarder) *forwarder {
	f.waiting = make(map[int]*queuedEvent)
	f.wake = make(chan struct{}, 1)
	f.abandon = make(chan struct{})
	f.exited = make(chan struct{})

	go f.run()

	return f
}

func (f *forwarder) signal() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// enqueue appends ev, or replaces the service's retry still waiting in place.
// Terminal events are never merged. After closeAdmission it does nothing.
func (f *forwarder) enqueue(index int, ev ServiceEvent) {
	f.mu.Lock()

	if f.closed {
		f.mu.Unlock()

		return
	}

	if w, ok := f.waiting[index]; ok && ev.Kind == EventRetrying {
		w.event = ev
		f.mu.Unlock()

		return
	}

	q := &queuedEvent{index: index, event: ev}
	f.queue = append(f.queue, q)

	// Nothing merges across a terminal event, so a service's events keep their
	// order whatever arrives after it.
	if ev.Kind == EventRetrying {
		f.waiting[index] = q
	} else {
		delete(f.waiting, index)
	}

	f.mu.Unlock()
	f.signal()
}

// next claims the next event. Abandonment is checked in the same critical
// section as the claim, so an event is either claimed before it or never.
func (f *forwarder) next() (ServiceEvent, bool) {
	for {
		f.mu.Lock()

		if f.abandoned {
			f.mu.Unlock()

			return ServiceEvent{}, false
		}

		if len(f.queue) > 0 {
			q := f.queue[0]
			f.queue[0] = nil
			f.queue = f.queue[1:]

			if f.waiting[q.index] == q {
				delete(f.waiting, q.index)
			}

			f.mu.Unlock()

			return q.event, true
		}

		if f.closed {
			f.mu.Unlock()

			return ServiceEvent{}, false
		}

		f.mu.Unlock()

		select {
		case <-f.wake:
		case <-f.abandon:
		}
	}
}

func (f *forwarder) run() {
	defer close(f.exited)

	if f.onExit != nil {
		defer f.onExit()
	}

	for {
		ev, ok := f.next()
		if !ok {
			return
		}

		if !f.deliver(ev, f.abandon) {
			f.dropped++

			return
		}
	}
}

// closeAdmission refuses further events; the forwarder exits once the queue is
// empty.
func (f *forwarder) closeAdmission() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	f.signal()
}

// abandonDelivery ends delivery and returns how many events were left.
func (f *forwarder) abandonDelivery() int {
	f.mu.Lock()
	f.abandoned = true
	left := len(f.queue)
	f.mu.Unlock()

	close(f.abandon)

	return left
}

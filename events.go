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

// eventForwarder carries ServiceEvents, merging a service's waiting retry by
// registration index, never by name.
type eventForwarder = forwarder[int, ServiceEvent]

func newEventForwarder(f *eventForwarder) *eventForwarder {
	f.mergeable = func(ev ServiceEvent) bool { return ev.Kind == EventRetrying }
	f.merge = func(waiting *ServiceEvent, next ServiceEvent) { *waiting = next }

	return newForwarder(f)
}

type queued[K comparable, T any] struct {
	key  K
	item T
}

// forwarder is the only deliverer to one consumer, fed by a queue that never
// blocks its producers (spec 0008 D8). An item still waiting absorbs a later
// one with the same key while mergeable says it may.
type forwarder[K comparable, T any] struct {
	mu        sync.Mutex
	queue     []*queued[K, T]
	waiting   map[K]*queued[K, T] // an unclaimed mergeable item, by key
	closed    bool
	abandoned bool
	// dropped is an item claimed and then abandoned mid-delivery. Read only
	// after exited.
	dropped int

	wake    chan struct{}
	abandon chan struct{}
	exited  chan struct{}

	consumer string // names the consumer in the undelivered-events warning
	// awaitAfterAbandon is set when deliver honours abandon, so the forwarder
	// stops promptly and must be waited for before Done.
	awaitAfterAbandon bool

	// deliver hands one item to the consumer, and reports false if delivery
	// was abandoned instead.
	deliver   func(item T, abandon <-chan struct{}) bool
	mergeable func(item T) bool
	merge     func(waiting *T, next T)
	onExit    func()
}

// drainable is a forwarder of any item, as shutdown's cutoff and drain see it.
type drainable interface {
	closeAdmission()
	finished() <-chan struct{}
	abandonDelivery() int
	consumerName() string
}

func newForwarder[K comparable, T any](f *forwarder[K, T]) *forwarder[K, T] {
	f.waiting = make(map[K]*queued[K, T])
	f.wake = make(chan struct{}, 1)
	f.abandon = make(chan struct{})
	f.exited = make(chan struct{})

	go f.run()

	return f
}

func (f *forwarder[K, T]) signal() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// enqueue appends item, or merges it into the one with its key still waiting.
// After closeAdmission it does nothing.
func (f *forwarder[K, T]) enqueue(key K, item T) {
	f.mu.Lock()

	if f.closed {
		f.mu.Unlock()

		return
	}

	mergeable := f.mergeable != nil && f.mergeable(item)

	if w, ok := f.waiting[key]; ok && mergeable {
		f.merge(&w.item, item)
		f.mu.Unlock()

		return
	}

	q := &queued[K, T]{key: key, item: item}
	f.queue = append(f.queue, q)

	// Nothing merges across an unmergeable item, so one key's items keep their
	// order whatever arrives after it.
	if mergeable {
		f.waiting[key] = q
	} else {
		delete(f.waiting, key)
	}

	f.mu.Unlock()
	f.signal()
}

// next claims the next item. Abandonment is checked in the same critical
// section as the claim, so an item is either claimed before it or never.
func (f *forwarder[K, T]) next() (T, bool) {
	var none T

	for {
		f.mu.Lock()

		if f.abandoned {
			f.mu.Unlock()

			return none, false
		}

		if len(f.queue) > 0 {
			q := f.queue[0]
			f.queue[0] = nil
			f.queue = f.queue[1:]

			if f.waiting[q.key] == q {
				delete(f.waiting, q.key)
			}

			f.mu.Unlock()

			return q.item, true
		}

		if f.closed {
			f.mu.Unlock()

			return none, false
		}

		f.mu.Unlock()

		select {
		case <-f.wake:
		case <-f.abandon:
		}
	}
}

func (f *forwarder[K, T]) run() {
	defer close(f.exited)

	if f.onExit != nil {
		defer f.onExit()
	}

	for {
		item, ok := f.next()
		if !ok {
			return
		}

		if !f.deliver(item, f.abandon) {
			f.dropped++

			return
		}
	}
}

// closeAdmission refuses further items; the forwarder exits once the queue is
// empty.
func (f *forwarder[K, T]) closeAdmission() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	f.signal()
}

// abandonDelivery ends delivery and returns how many items were left,
// counting one abandoned mid-delivery when the forwarder is awaited.
func (f *forwarder[K, T]) abandonDelivery() int {
	f.mu.Lock()
	f.abandoned = true
	left := len(f.queue)
	f.mu.Unlock()

	close(f.abandon)

	if f.awaitAfterAbandon {
		<-f.exited

		left += f.dropped
	}

	return left
}

func (f *forwarder[K, T]) finished() <-chan struct{} { return f.exited }

func (f *forwarder[K, T]) consumerName() string { return f.consumer }

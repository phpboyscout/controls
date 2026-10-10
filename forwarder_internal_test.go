package controls

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func retrying(service string, restarts int) ServiceEvent {
	return ServiceEvent{Service: service, Kind: EventRetrying, Restarts: restarts}
}

// heldForwarder delivers into a channel the test reads, so nothing is claimed
// until the test asks.
func heldForwarder(t *testing.T) (*forwarder, chan ServiceEvent) {
	t.Helper()

	out := make(chan ServiceEvent)
	f := newForwarder(&forwarder{deliver: func(ev ServiceEvent, abandon <-chan struct{}) bool {
		select {
		case out <- ev:
			return true
		case <-abandon:
			return false
		}
	}})

	t.Cleanup(func() {
		f.closeAdmission()
		f.mu.Lock()
		abandoned := f.abandoned
		f.mu.Unlock()

		if !abandoned {
			f.abandonDelivery()
		}

		<-f.exited
	})

	return f, out
}

// holdInSend has the forwarder claim ev and wait in its send, unread, so
// whatever is enqueued next stays queued.
func holdInSend(t *testing.T, f *forwarder, ev ServiceEvent) {
	t.Helper()

	f.enqueue(len(t.Name()), ev)

	require.Eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()

		return len(f.queue) == 0
	}, 2*time.Second, time.Millisecond)
}

func TestAnEnqueueAfterCloseIsRefused(t *testing.T) {
	t.Parallel()

	f, out := heldForwarder(t)
	f.enqueue(0, retrying("a", 1))
	<-out
	f.closeAdmission()
	f.enqueue(1, retrying("b", 1))

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Empty(t, f.queue)
}

func TestAbandonmentBeforeAClaimPreventsIt(t *testing.T) {
	t.Parallel()

	f, out := heldForwarder(t)
	holdInSend(t, f, retrying("blocker", 1))
	f.enqueue(0, retrying("a", 1))
	f.enqueue(1, retrying("b", 1))

	// The send it is waiting in is released, and nothing more is claimed.
	require.Equal(t, 2, f.abandonDelivery())
	<-f.exited

	select {
	case ev := <-out:
		t.Fatalf("delivered %v after abandonment", ev)
	default:
	}
}

func TestMergingIsPerRegistrationIndex(t *testing.T) {
	t.Parallel()

	f, _ := heldForwarder(t)
	holdInSend(t, f, retrying("blocker", 1))

	f.enqueue(0, retrying("twin", 1))
	f.enqueue(1, retrying("twin", 1))
	f.enqueue(0, retrying("twin", 2))
	f.enqueue(0, ServiceEvent{Service: "twin", Kind: EventUnableToStart})
	f.enqueue(0, retrying("twin", 3))

	f.mu.Lock()
	queued := make([]ServiceEvent, 0, len(f.queue))

	for _, q := range f.queue {
		queued = append(queued, q.event)
	}
	f.mu.Unlock()

	require.Equal(t, []ServiceEvent{
		retrying("twin", 2),
		retrying("twin", 1),
		{Service: "twin", Kind: EventUnableToStart},
		retrying("twin", 3),
	}, queued, "a waiting retry is replaced in place; a terminal event never merges")
}

func TestNoForwarderWithoutAConsumer(t *testing.T) {
	t.Parallel()

	c := NewController(context.Background(), WithLogger(slog.New(slog.DiscardHandler)))
	c.Register("svc", WithStart(func(context.Context) error { return errors.New("boom") }))
	c.Start()
	c.Stop()
	<-c.Done()

	c.subMu.Lock()
	defer c.subMu.Unlock()

	require.Nil(t, c.errsFwd)
	require.Nil(t, c.eventsFwd)
}

func TestErrorsAfterDoneStartsNothing(t *testing.T) {
	t.Parallel()

	c := NewController(context.Background(), WithLogger(slog.New(slog.DiscardHandler)))
	c.Start()
	c.Stop()
	<-c.Done()

	_, open := <-c.Errors()
	require.False(t, open)

	c.subMu.Lock()
	defer c.subMu.Unlock()

	require.Nil(t, c.errsFwd)
}

// An installed channel is subscribed from Start; a later Errors() starting a
// second forwarder would orphan the first, which never exits.
func TestErrorsAfterStartReusesTheInstalledChannelsForwarder(t *testing.T) {
	t.Parallel()

	c := NewController(context.Background(), WithLogger(slog.New(slog.DiscardHandler)))
	c.SetErrorsChannel(make(chan error))
	c.Start()

	c.subMu.Lock()
	started := c.errsFwd
	c.subMu.Unlock()

	c.Errors()

	c.subMu.Lock()
	again := c.errsFwd
	c.subMu.Unlock()

	c.Stop()
	<-c.Done()

	require.NotNil(t, started)
	require.Same(t, started, again)
}

// stopFor's result says who won the transition, so the recorded cause can be
// checked against the winner. A cause recorded outside the transition is
// caught only when the scheduler lands in that window.
func TestTheRecordedCauseIsTheWinners(t *testing.T) {
	t.Parallel()

	causes := []StopCause{CauseStop, CauseSignal, CauseParent}

	for range 100 {
		c := NewController(context.Background(), WithLogger(slog.New(slog.DiscardHandler)))
		c.Start()

		var (
			wg     sync.WaitGroup
			mu     sync.Mutex
			winner StopCause
		)

		for _, cause := range causes {
			wg.Go(func() {
				if c.stopFor(cause, nil, nil) {
					mu.Lock()
					winner = cause
					mu.Unlock()
				}
			})
		}

		wg.Wait()

		select {
		case <-c.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("shutdown did not complete")
		}

		o, ok := c.Outcome()
		require.True(t, ok)
		require.Equal(t, winner, o.Cause)
	}
}

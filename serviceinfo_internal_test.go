package controls

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Two writers update one ServiceInfo at once: the supervisor recording how a
// run ended, and the stop goroutine recording how the stop ended. Each is a
// load, a change and a store, and if one loads before the other stores, the
// last store wins with a stale copy. That lost StopErr is issue 13.
func TestInfoUpdatesFromTwoWritersAreBothKept(t *testing.T) {
	t.Parallel()

	var q Services
	q.add(Service{Name: "svc"})

	errRun := errors.New("run ended")
	errStop := errors.New("listener still open")

	// The first writer loads, then is held between its load and its store
	// while the second writer runs to completion.
	entered := make(chan struct{})
	release := make(chan struct{})

	var wg sync.WaitGroup

	wg.Go(func() {
		q.mutateInfo("svc", func(i *ServiceInfo) {
			close(entered)
			<-release
			i.Error = errRun
		})
	})

	<-entered

	second := make(chan struct{})

	wg.Go(func() {
		q.recordStopErr("svc", errStop)
		close(second)
	})

	// Serialised, the second writer cannot finish until the first is released,
	// and this wait times out. Unserialised, it finishes now, and releasing the
	// first writer then overwrites what it stored.
	select {
	case <-second:
		t.Log("second writer completed while the first was mid-update")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	wg.Wait()

	info, ok := q.info.Load("svc")
	require.True(t, ok)

	got, _ := info.(ServiceInfo)
	require.ErrorIs(t, got.Error, errRun, "the first writer's field must survive")
	require.ErrorIs(t, got.StopErr, errStop, "the second writer's field must survive")
}

// The same two writers, many times, with no choreography: a lost update shows
// as a field that should be set and is not.
func TestInfoUpdatesUnderContentionAreNeverLost(t *testing.T) {
	t.Parallel()

	var q Services
	q.add(Service{Name: "svc"})

	errRun := errors.New("run ended")
	errStop := errors.New("listener still open")

	for round := range 2000 {
		q.mutateInfo("svc", func(i *ServiceInfo) { i.Error, i.StopErr = nil, nil })

		var wg sync.WaitGroup

		wg.Go(func() { q.mutateInfo("svc", func(i *ServiceInfo) { i.Error = errRun }) })
		wg.Go(func() { q.recordStopErr("svc", errStop) })
		wg.Wait()

		v, _ := q.info.Load("svc")

		got, _ := v.(ServiceInfo)
		if got.Error == nil || got.StopErr == nil {
			t.Fatalf("round %d: an update was lost: Error=%v StopErr=%v", round, got.Error, got.StopErr)
		}
	}
}

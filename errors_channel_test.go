package controls_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.com/phpboyscout/go/controls"
)

// drainAll ranges errs until it closes, failing if it does not within d.
func drainAll(t *testing.T, errs <-chan error, d time.Duration) []error {
	t.Helper()

	var got []error

	timeout := time.After(d)

	for {
		select {
		case err, ok := <-errs:
			if !ok {
				return got
			}

			got = append(got, err)
		case <-timeout:
			t.Fatalf("the error channel did not close within %s", d)
		}
	}
}

func requireClosed(t *testing.T, errs <-chan error) {
	t.Helper()

	select {
	case err, ok := <-errs:
		require.False(t, ok, "expected a closed channel, received %v", err)
	default:
		t.Fatal("the error channel is still open")
	}
}

func TestErrorsSubscribesWhenCalled(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")

	t.Run("before Start", func(t *testing.T) {
		t.Parallel()

		c := newQuietController(t)
		errs := c.Errors()

		c.Register("svc", controls.WithStart(failsWith(errBoom)))
		c.Start()

		require.ErrorIs(t, <-errs, errBoom)
		c.Stop()
		require.Empty(t, drainAll(t, errs, 2*time.Second))
		requireDone(t, c, 2*time.Second)
	})

	t.Run("after Start", func(t *testing.T) {
		t.Parallel()

		gate := make(chan struct{})

		c := newQuietController(t)
		c.Register("svc", controls.WithStart(gatedFailure(gate)))
		c.Start()

		errs := c.Errors()
		close(gate)

		require.EqualError(t, <-errs, "run 1")
		c.Stop()
		require.Empty(t, drainAll(t, errs, 2*time.Second))
	})

	t.Run("concurrently with Start, from many goroutines", func(t *testing.T) {
		t.Parallel()

		for range 50 {
			gate := make(chan struct{})

			c := newQuietController(t)
			c.Register("svc", controls.WithStart(gatedFailure(gate)))

			var wg sync.WaitGroup

			wg.Go(c.Start)

			for range 10 {
				wg.Go(func() { c.Errors() })
			}

			wg.Wait()
			close(gate)

			errs := c.Errors()
			require.EqualError(t, <-errs, "run 1")
			c.Stop()
			require.Empty(t, drainAll(t, errs, 2*time.Second), "one subscription, one forwarder, one copy")
		}
	})
}

// A subscription made while Stopping, before the cutoff, still sees its channel
// close before Done.
func TestErrorsFirstCalledWhileStopping(t *testing.T) {
	t.Parallel()

	inStop := make(chan struct{})
	release := make(chan struct{})

	c := newQuietController(t)
	c.Register("svc", controls.WithStop(func(context.Context) { close(inStop); <-release }))
	c.Start()

	go c.Stop()

	<-inStop

	errs := c.Errors()
	close(release)

	requireDone(t, c, 2*time.Second)
	requireClosed(t, errs)
}

// From the cutoff on nothing subscribes: during the drain and after Done,
// Errors returns the controller's channel, closed by the time Done is.
func TestErrorsAfterTheCutoffIsClosed(t *testing.T) {
	t.Parallel()

	t.Run("during the drain", func(t *testing.T) {
		t.Parallel()

		held := make(chan struct{})
		release := make(chan struct{})

		var once sync.Once

		c := newQuietController(t, controls.WithOnEvent(func(controls.ServiceEvent) {
			once.Do(func() { close(held); <-release })
		}))
		c.Register("svc", controls.WithStart(failsWith(errors.New("boom"))))
		c.Start()

		<-held
		c.Stop()
		awaitTrue(t, 2*time.Second, "Stopped", c.IsStopped)

		errs := c.Errors()
		close(release)

		requireDone(t, c, 2*time.Second)
		requireClosed(t, errs)
	})

	t.Run("after Done", func(t *testing.T) {
		t.Parallel()

		c := newQuietController(t)
		c.Start()
		c.Stop()
		requireDone(t, c, 2*time.Second)

		requireClosed(t, c.Errors())
	})
}

func TestErrorsThenSetErrorsChannelForwardsToTheInstalledOne(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	installed := make(chan error, 1)

	c := newQuietController(t)
	own := c.Errors()
	c.SetErrorsChannel(installed)
	c.Register("svc", controls.WithStart(failsWith(errBoom)))
	c.Start()

	require.ErrorIs(t, <-installed, errBoom)

	c.Stop()
	requireDone(t, c, 2*time.Second)
	requireClosed(t, own)
}

// The race the spike found: a reader that keeps reading loses nothing when
// shutdown lands mid-burst. Every failure here is terminal, so none can merge,
// and the callback is a second consumer of the same decided failures.
func TestNoFailureIsLostToAShutdownMidBurst(t *testing.T) {
	t.Parallel()

	const services = 40

	for range 100 {
		var delivered atomic.Int64

		c := newQuietController(t, controls.WithOnEvent(func(controls.ServiceEvent) { delivered.Add(1) }))
		errs := c.Errors()

		for i := range services {
			c.Register(fmt.Sprintf("svc-%d", i), controls.WithStart(failsWith(fmt.Errorf("svc-%d", i))))
		}

		got := make(chan int)

		go func() {
			n := 0
			for range errs {
				n++
			}
			got <- n
		}()

		c.Start()
		time.Sleep(200 * time.Microsecond)
		c.Stop()
		requireDone(t, c, 2*time.Second)

		select {
		case n := <-got:
			require.Equal(t, delivered.Load(), int64(n), "the channel reader and the callback saw different failures")
		case <-time.After(2 * time.Second):
			t.Fatal("the range over Errors() did not end")
		}
	}
}

func TestASlowReaderGetsEverythingWithinTheBudget(t *testing.T) {
	t.Parallel()

	const services = 20

	c := newQuietController(t)
	errs := c.Errors()

	for i := range services {
		c.Register(fmt.Sprintf("svc-%d", i), controls.WithStart(failsWith(fmt.Errorf("svc-%d", i))))
	}

	c.Start()

	awaitTrue(t, 2*time.Second, "every service to fail", func() bool {
		for i := range services {
			if findInfo(t, c, fmt.Sprintf("svc-%d", i)).Error == nil {
				return false
			}
		}

		return true
	})

	go c.Stop()

	n := 0

	for range errs {
		n++

		time.Sleep(2 * time.Millisecond)
	}

	require.Equal(t, services, n)
	requireDone(t, c, 2*time.Second)
}

func TestAReaderThatStopsHoldsDoneForTheBudget(t *testing.T) {
	t.Parallel()

	logs := &recordingHandler{}

	c := controls.NewController(context.Background(), controls.WithLogger(slog.New(logs)),
		controls.WithShutdownTimeout(200*time.Millisecond))
	errs := c.Errors()

	for i := range 5 {
		c.Register(fmt.Sprintf("svc-%d", i), controls.WithStart(failsWith(fmt.Errorf("svc-%d", i))))
	}

	c.Start()
	<-errs
	awaitTrue(t, 2*time.Second, "every failure to be queued", func() bool {
		total := 0
		for i := range 5 {
			total += logs.failuresLogged(fmt.Sprintf("svc-%d", i))
		}

		return total == 5
	})

	triggered := time.Now()

	c.Stop()
	requireDone(t, c, 2*time.Second)

	require.GreaterOrEqual(t, time.Since(triggered), 150*time.Millisecond)
	require.True(t, logs.warnMentioning("events undelivered", "Errors"))
	require.Equal(t, int64(4), logs.undelivered("Errors"), "three queued and one abandoned mid-send")
}

func TestRetriesMergeOnTheChannel(t *testing.T) {
	t.Parallel()

	gate := make(chan struct{})
	logs := &recordingHandler{}

	c := controls.NewController(context.Background(), controls.WithLogger(slog.New(logs)))
	errs := c.Errors()

	c.Register("blocker", controls.WithStart(failsWith(errors.New("blocker"))))
	c.Register("retrier", controls.WithStart(gatedFailure(gate)),
		controls.WithRestartPolicy(controls.RestartPolicy{MaxRestarts: 3, InitialBackoff: time.Millisecond}))
	c.Start()

	awaitTrue(t, 2*time.Second, "the blocker to fail", func() bool { return logs.failuresLogged("blocker") == 1 })
	close(gate)
	awaitTrue(t, 2*time.Second, "the retrier to exhaust", func() bool { return logs.failuresLogged("retrier") == 4 })

	require.EqualError(t, <-errs, "blocker")
	require.EqualError(t, <-errs, "run 3", "the newest retry replaces those waiting")
	require.ErrorIs(t, <-errs, controls.ErrRestartsExhausted)

	c.Stop()
	require.Empty(t, drainAll(t, errs, 2*time.Second))
}

// An installed channel nobody reads holds back no restart loop, holds Done for
// the budget only, and is not closed: it is the consumer's.
func TestAnUnreadInstalledChannelHoldsNothingBack(t *testing.T) {
	t.Parallel()

	installed := make(chan error)

	c := newQuietController(t, controls.WithShutdownTimeout(100*time.Millisecond))
	c.SetErrorsChannel(installed)
	c.Register("retrier", controls.WithStart(failsWith(errors.New("again"))),
		controls.WithRestartPolicy(controls.RestartPolicy{InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}))
	c.Start()

	awaitTrue(t, 2*time.Second, "restarts to carry on", func() bool {
		return findInfo(t, c, "retrier").RestartCount >= 5
	})

	c.Stop()
	requireDone(t, c, 2*time.Second)

	select {
	case err, ok := <-installed:
		t.Fatalf("nothing should arrive after Done (received %v, open %v)", err, ok)
	default:
	}
}

// A send completes when the value lands in the buffer, so a reader that stops
// at Done still has to take what is buffered (spec 0008 D9).
func TestABufferedInstalledChannelHoldsItsErrorsAfterDone(t *testing.T) {
	t.Parallel()

	const services = 4

	installed := make(chan error, services)

	c := newQuietController(t)
	c.SetErrorsChannel(installed)

	for i := range services {
		c.Register(fmt.Sprintf("svc-%d", i), controls.WithStart(failsWith(fmt.Errorf("svc-%d", i))))
	}

	c.Start()
	awaitTrue(t, 2*time.Second, "every error to be buffered", func() bool { return len(installed) == services })
	c.Stop()
	requireDone(t, c, 2*time.Second)

	close(installed)

	n := 0
	for range installed {
		n++
	}

	require.Equal(t, services, n)
}

// The channel's forwarder is waited for before Done, so a consumer closing its
// channel on Done never meets a late send, which would panic here. A forwarder
// descheduled between claiming an event and sending it as the deadline passes
// is the case; this finds it only when the scheduler lands there.
func TestNothingIsSentOnAnInstalledChannelAfterDone(t *testing.T) {
	t.Parallel()

	for range 300 {
		installed := make(chan error)

		c := newQuietController(t, controls.WithShutdownTimeout(2*time.Millisecond))
		c.SetErrorsChannel(installed)

		for i := range 10 {
			c.Register(fmt.Sprintf("svc-%d", i), controls.WithStart(failsWith(fmt.Errorf("svc-%d", i))))
		}

		readerDone := make(chan struct{})

		go func() {
			defer close(readerDone)

			for range installed {
				time.Sleep(time.Millisecond)
			}
		}()

		c.Start()
		c.Stop()
		<-c.Done()
		close(installed)
		<-readerDone
	}
}

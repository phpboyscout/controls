package controls_test

import (
	"context"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/phpboyscout/go/controls"
)

// recordingHandler is a minimal slog.Handler that captures records into a
// mutex-guarded slice so tests can assert on emitted log messages.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.records = append(h.records, r.Clone())

	return nil
}

func (h *recordingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(_ string) slog.Handler      { return h }

// warnMentioning reports whether a WARN-level record whose message contains
// msgFragment carries an attribute with the given value.
func (h *recordingHandler) warnMentioning(msgFragment, attrValue string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, r := range h.records {
		if r.Level != slog.LevelWarn || !strings.Contains(r.Message, msgFragment) {
			continue
		}

		found := false
		r.Attrs(func(a slog.Attr) bool {
			if a.Value.String() == attrValue {
				found = true
				return false
			}
			return true
		})

		if found {
			return true
		}
	}

	return false
}

// TestWaitContext_StuckStartFuncReturnsDeadlineExceeded is the D10 regression:
// a StartFunc that ignores cancellation entirely (bare select {}) pins its
// supervisor goroutine, so the wait group can never drain. The shutdown
// sequence itself must still complete (state reaches Stopped), and WaitContext
// must return context.DeadlineExceeded promptly instead of hanging forever the
// way a bare Wait() would.
func TestWaitContext_StuckStartFuncReturnsDeadlineExceeded(t *testing.T) {
	t.Parallel()

	startEntered := make(chan struct{})
	// The StartFunc ignores its ctx and blocks on this channel instead. It is
	// only released at test cleanup, AFTER all assertions — so for the duration
	// of the test the supervisor goroutine is genuinely abandoned (D10), but the
	// deliberate leak is bounded and does not skew the goroutine baselines of
	// sibling parallel tests.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	c := newQuietController(t, controls.WithShutdownTimeout(100*time.Millisecond))
	c.Register("stuck",
		controls.WithStart(func(_ context.Context) error {
			close(startEntered)
			<-release // ignore ctx entirely

			return nil
		}),
		controls.WithStop(func(_ context.Context) {}),
	)

	c.Start()

	select {
	case <-startEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("start func was never entered")
	}

	c.Stop()

	// The shutdown sequence must complete despite the stuck StartFunc.
	require.Eventually(t, func() bool {
		return c.GetState() == controls.Stopped
	}, 5*time.Second, 10*time.Millisecond, "shutdown must complete despite a ctx-ignoring StartFunc")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- c.WaitContext(ctx) }()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded,
			"WaitContext must report the abandoned wait via ctx.Err()")
	case <-time.After(2 * time.Second):
		t.Fatal("WaitContext hung: ctx-ignoring StartFunc was not abandoned at the deadline")
	}

	assert.True(t, c.IsStopped())
	// No goroutine-baseline assertion here: until the cleanup releases it, the
	// stuck supervisor (and the WaitContext helper watching the undrainable
	// wait group) are deliberately leaked — the documented D10
	// abandon-at-deadline tradeoff.
}

// TestWaitContext_CleanDrainReturnsNil verifies the clean path: with
// context-respecting services WaitContext returns nil and, per the existing
// leak guards, the controller goroutines unwind to baseline.
//
// # It must not call t.Parallel()
//
// runtime.NumGoroutine is process-global. Run in parallel, the count includes
// every other test's goroutines, so the assertion cannot tell a leak here from
// another test being busy, and symmetrically a real leak can hide behind another
// test finishing. A Go test that does not call t.Parallel() runs while every
// parallel test in the package is paused, so the count means what it says.
func TestWaitContext_CleanDrainReturnsNil(t *testing.T) {
	before := runtime.NumGoroutine()

	c := newQuietController(t)
	c.Register("conforming",
		controls.WithStart(func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}),
		controls.WithStop(func(_ context.Context) {}),
	)

	c.Start()
	c.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, c.WaitContext(ctx), "clean drain must return nil")
	assert.True(t, c.IsStopped())

	require.Eventually(t, func() bool {
		return runtime.NumGoroutine() <= before+1
	}, 5*time.Second, 20*time.Millisecond, "no goroutines may leak on the clean WaitContext path")
}

// TestHandleStop_WarnsOnStuckStartFunc verifies the shutdown-path diagnostic:
// after the internal post-stop supervisor wait expires, the stuck service is
// named in a WARN record — a silent hang becomes a diagnosable message.
func TestHandleStop_WarnsOnStuckStartFunc(t *testing.T) {
	t.Parallel()

	h := &recordingHandler{}
	// Released at cleanup so the deliberate supervisor leak is bounded (see
	// TestWaitContext_StuckStartFuncReturnsDeadlineExceeded).
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	c := newQuietController(t,
		controls.WithLogger(slog.New(h)),
		controls.WithShutdownTimeout(100*time.Millisecond),
	)
	c.Register("wedged",
		controls.WithStart(func(_ context.Context) error {
			<-release // ignore ctx entirely

			return nil
		}),
		controls.WithStop(func(_ context.Context) {}),
	)

	c.Start()
	c.Stop()

	require.Eventually(t, func() bool {
		return c.GetState() == controls.Stopped
	}, 5*time.Second, 10*time.Millisecond)

	assert.True(t, h.warnMentioning("StartFunc", "wedged"),
		"the stuck service must be named in a WARN record after the post-stop wait expires")
}

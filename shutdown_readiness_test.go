package controls_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"gitlab.com/phpboyscout/go/controls"
)

// TestReadiness_RespondsWhileStopping proves F3: the services mutex must not be
// held for the whole stop sequence. A slow StopFunc must not block health/
// readiness probes — a load balancer needs a prompt not-ready answer exactly
// when shutdown is in flight.
func TestReadiness_RespondsWhileStopping(t *testing.T) {
	t.Parallel()

	stopStarted := make(chan struct{})
	releaseStop := make(chan struct{})

	t.Cleanup(func() { close(releaseStop) })

	c := controls.NewController(context.Background(),
		controls.WithLogger(slog.New(slog.DiscardHandler)),
		controls.WithShutdownTimeout(500*time.Millisecond),
	)

	c.Register("slow-stopper",
		controls.WithStart(func(ctx context.Context) error {
			<-ctx.Done()

			return ctx.Err()
		}),
		controls.WithStop(func(_ context.Context) {
			close(stopStarted)
			<-releaseStop // ignores its context; abandoned at the shutdown deadline
		}),
	)

	c.Start()

	go c.Stop()

	// Wait until the stop sequence is genuinely in flight (StopFunc entered).
	select {
	case <-stopStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("stop sequence never started")
	}

	// Now time a readiness probe. If stop holds the services mutex for the whole
	// sequence, this blocks until the shutdown deadline; otherwise it returns at
	// once.
	start := time.Now()
	c.Readiness()
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 150*time.Millisecond,
		"readiness must respond promptly while a slow stop is in flight")

	c.Wait()
}

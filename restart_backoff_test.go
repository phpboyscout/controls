package controls_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gitlab.com/phpboyscout/go/errors"

	"gitlab.com/phpboyscout/go/controls"
)

// TestRestartBackoff_ResetsAfterHealthyRun proves F6(a): after a service runs
// healthily for at least the reset interval, the backoff must be reset to
// InitialBackoff alongside the restart counter. Otherwise a service healthy for
// hours still waits the accumulated MaxBackoff before its next restart.
//
// The service fails fast several times so backoff climbs toward MaxBackoff, then
// runs long enough to trip the reset, then fails again. The gap before that last
// restart reveals whether backoff was reset.
func TestRestartBackoff_ResetsAfterHealthyRun(t *testing.T) {
	t.Parallel()

	const (
		fastFails    = 6
		healthyRunMS = 120
	)

	var (
		mu     sync.Mutex
		starts []time.Time
	)

	c := controls.NewController(context.Background(),
		controls.WithLogger(slog.New(slog.DiscardHandler)),
	)

	c.Register("climber",
		controls.WithStart(func(ctx context.Context) error {
			mu.Lock()
			n := len(starts)
			starts = append(starts, time.Now())
			mu.Unlock()

			switch {
			case n < fastFails:
				// Fail fast: climbs backoff toward MaxBackoff.
				return errors.New("fast fail")
			case n == fastFails:
				// Healthy run: lasts longer than the reset interval, then fails.
				time.Sleep(healthyRunMS * time.Millisecond)

				return errors.New("fail after healthy run")
			default:
				// Stay up until shutdown.
				<-ctx.Done()

				return ctx.Err()
			}
		}),
		controls.WithStop(func(_ context.Context) {}),
		controls.WithRestartPolicy(controls.RestartPolicy{
			MaxRestarts:          1000,
			InitialBackoff:       10 * time.Millisecond,
			MaxBackoff:           500 * time.Millisecond,
			RestartResetInterval: 80 * time.Millisecond,
		}),
	)

	c.Start()
	t.Cleanup(func() {
		c.Stop()
		c.Wait()
	})

	// Wait until the restart after the healthy run has happened.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(starts) >= fastFails+2
	}, 10*time.Second, 10*time.Millisecond, "service must restart after its healthy run")

	mu.Lock()
	healthyStart := starts[fastFails]
	postHealthyStart := starts[fastFails+1]
	mu.Unlock()

	gap := postHealthyStart.Sub(healthyStart)

	// gap = healthy run duration (~120ms) + backoff. With the reset, backoff is
	// InitialBackoff (10ms) => ~130ms. Without it, backoff is pinned at
	// MaxBackoff (500ms) => ~620ms.
	assert.Less(t, gap, 350*time.Millisecond,
		"backoff must be reset to InitialBackoff after a healthy run (gap=%s)", gap)
}

package controls

import (
	"context"
	"errors"
	"testing"
)

// TestAFinishedReleaseIsNeverATimeout is issue 22: once the budget has expired
// both cases are ready, and a select picks one at random.
func TestAFinishedReleaseIsNeverATimeout(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	close(done)

	for range 1000 {
		if err := releaseResult(ctx, done); err != nil {
			t.Fatalf("a release that had finished was reported as %v", err)
		}
	}
}

func TestAnUnfinishedReleaseIsATimeout(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := releaseResult(ctx, make(chan struct{})); !errors.Is(err, ErrStopTimeout) {
		t.Fatalf("an unfinished release past the budget returned %v, want ErrStopTimeout", err)
	}
}

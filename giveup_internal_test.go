package controls

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// Every shutdown records its cause before the cancel, so a completion after it
// always loses the race, and only the rule itself can show it is not one.
func TestACompletionAfterTheCancelIsCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	require.Equal(t, outcomeCleanStart, completionOutcome(ctx, nil))

	cancel()
	require.Equal(t, outcomeCancelled, completionOutcome(ctx, nil))
	require.Equal(t, outcomeCancelled, completionOutcome(ctx, errors.New("boom")))
}

// A declared service giving up ends the controller anyway, so the count it
// must not touch is checked directly (spec 0010 D10).
func TestAGiveUpDoesNotCountAsACompletion(t *testing.T) {
	t.Parallel()

	var causes []StopCause

	q := &Services{
		completeOnAll: true,
		publish:       func(int, ServiceEvent) {},
		endFor:        func(cause StopCause, _ error, _ string) { causes = append(causes, cause) },
	}
	q.outstanding.Store(2)

	q.terminal(0, Service{Name: "job", runToCompletion: true}, ServiceEvent{Kind: EventUnableToStart})

	require.Equal(t, int32(2), q.outstanding.Load())
	require.Equal(t, []StopCause{CauseGaveUp}, causes)
}

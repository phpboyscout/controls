package controls_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.com/phpboyscout/go/controls"
)

// budgetRecorder is a Release that notes how much budget each call was given
// and fails the first n calls.
type budgetRecorder struct {
	mu        sync.Mutex
	remaining []time.Duration
	failFirst int
}

func (b *budgetRecorder) release(ctx context.Context, _ *run) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		return errors.New("release given no deadline")
	}

	b.mu.Lock()
	b.remaining = append(b.remaining, time.Until(deadline))
	calls := len(b.remaining)
	b.mu.Unlock()

	if calls <= b.failFirst {
		return errors.New("not yet")
	}

	return nil
}

func (b *budgetRecorder) budgets() []time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]time.Duration(nil), b.remaining...)
}

// requireBudgetsAbout asserts every recorded budget is the expected one, less
// whatever the scheduler took between the deadline being set and Release
// reading it.
func requireBudgetsAbout(t *testing.T, got []time.Duration, want time.Duration) {
	t.Helper()

	for i, d := range got {
		require.LessOrEqual(t, d, want, "attempt %d was given more than the budget", i+1)
		require.Greater(t, d, want-want/2, "attempt %d was given far less than the budget", i+1)
	}
}

func build(context.Context) (*run, error) { return &run{}, nil }

func startAndStop(t *testing.T, g *controls.Generational[*run]) error {
	t.Helper()

	require.NoError(t, g.Start(context.Background()))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return g.Stop(ctx)
}

func TestReleaseAttemptBoundsEachDisposerCall(t *testing.T) {
	t.Parallel()

	rec := &budgetRecorder{failFirst: 3}
	g := &controls.Generational[*run]{
		Build:          build,
		Release:        rec.release,
		ReleaseAttempt: 40 * time.Millisecond,
	}

	require.NoError(t, startAndStop(t, g))

	got := rec.budgets()
	require.Len(t, got, 4, "three failures and a success")
	requireBudgetsAbout(t, got, 40*time.Millisecond)
}

func TestReleaseAttemptZeroIsTheDocumentedDefault(t *testing.T) {
	t.Parallel()

	rec := &budgetRecorder{failFirst: 1}
	g := &controls.Generational[*run]{Build: build, Release: rec.release}

	require.NoError(t, startAndStop(t, g))
	requireBudgetsAbout(t, rec.budgets(), 250*time.Millisecond)
}

func TestReleaseAttemptNegativeIsTheDefaultToo(t *testing.T) {
	t.Parallel()

	rec := &budgetRecorder{failFirst: 1}
	g := &controls.Generational[*run]{Build: build, Release: rec.release, ReleaseAttempt: -time.Second}

	require.NoError(t, startAndStop(t, g), "a negative budget must not leave Release with an expired context for ever")
	requireBudgetsAbout(t, rec.budgets(), 250*time.Millisecond)
}

// A Start that lost a race releases what it built on its own goroutine, four
// attempts of the configured budget, then gives up.
func TestALosingStartMakesFourAttemptsOfTheBudget(t *testing.T) {
	t.Parallel()

	var (
		inBuild sync.WaitGroup
		proceed = make(chan struct{})
	)

	inBuild.Add(2)

	rec := &budgetRecorder{failFirst: 1 << 20}
	g := &controls.Generational[*run]{
		Build: func(context.Context) (*run, error) {
			inBuild.Done()
			<-proceed

			return &run{}, nil
		},
		Release:        rec.release,
		ReleaseAttempt: 30 * time.Millisecond,
	}

	errs := make(chan error, 2)

	for range 2 {
		go func() { errs <- g.Start(context.Background()) }()
	}

	inBuild.Wait()
	close(proceed)

	var lost int

	for range 2 {
		if err := <-errs; errors.Is(err, controls.ErrGenerationRunning) {
			lost++
		}
	}

	require.Equal(t, 1, lost, "exactly one Start loses")

	got := rec.budgets()
	require.Len(t, got, 4, "the loser makes four attempts and gives up")
	requireBudgetsAbout(t, got, 30*time.Millisecond)
}

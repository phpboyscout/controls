package controls_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitlab.com/phpboyscout/go/controls"
)

// run is a test double for whatever a consumer builds per generation.
type run struct {
	id       int
	released atomic.Bool
}

// newFactory returns a Build that hands out a fresh run each time and records
// every run it made, which is how the tests assert freshness.
func newFactory() (build func(context.Context) (*run, error), made *[]*run, mu *sync.Mutex) {
	var (
		m    sync.Mutex
		runs []*run
		next int
	)

	return func(context.Context) (*run, error) {
		m.Lock()
		defer m.Unlock()

		next++
		r := &run{id: next}
		runs = append(runs, r)

		return r, nil
	}, &runs, &m
}

func release(ctx context.Context, r *run) error {
	r.released.Store(true)

	return nil
}

// D10, the liveness half: a fresh R per Start. Three of the five estate
// instances of this defect class were a captured single-use object.
func TestBuildsAFreshRunPerStart(t *testing.T) {
	t.Parallel()

	build, made, mu := newFactory()
	g := &controls.Generational[*run]{Build: build, Release: release}

	for i := 0; i < 3; i++ {
		if err := g.Start(context.Background()); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}

		if err := g.Stop(context.Background()); err != nil {
			t.Fatalf("stop %d: %v", i, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()

	if len(*made) != 3 {
		t.Fatalf("built %d runs across 3 cycles, want 3", len(*made))
	}

	seen := map[*run]bool{}

	for _, r := range *made {
		if seen[r] {
			t.Error("the same run was handed out twice")
		}

		seen[r] = true

		if !r.released.Load() {
			t.Errorf("run %d was never released", r.id)
		}
	}
}

// D10, the safety half: a stopped generation refuses further use, loudly.
// Never a zero R, never a stale one.
func TestUseRefusesLoudlyWhenNotRunning(t *testing.T) {
	t.Parallel()

	build, _, _ := newFactory()
	g := &controls.Generational[*run]{Build: build, Release: release}

	// Before any Start.
	if err := g.Use(func(*run) error { return nil }); !errors.Is(err, controls.ErrNoGeneration) {
		t.Errorf("Use before Start returned %v, want ErrNoGeneration", err)
	}

	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	var got *run

	if err := g.Use(func(r *run) error { got = r; return nil }); err != nil {
		t.Fatalf("Use while running: %v", err)
	}

	if got == nil {
		t.Fatal("Use handed the callback a nil run")
	}

	if err := g.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// After Stop.
	if err := g.Use(func(*run) error { return nil }); !errors.Is(err, controls.ErrNoGeneration) {
		t.Errorf("Use after Stop returned %v, want ErrNoGeneration", err)
	}
}

// Use must return the callback's own error unchanged, or a caller cannot tell
// a refusal from a failure.
func TestUsePropagatesTheCallbackError(t *testing.T) {
	t.Parallel()

	build, _, _ := newFactory()
	g := &controls.Generational[*run]{Build: build, Release: release}

	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	sentinel := errors.New("the callback said no")

	if err := g.Use(func(*run) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Errorf("Use returned %v, want the callback's own error", err)
	}
}

// D7: Release must not begin while a lease is held, or a caller can be inside
// a run that is being torn down underneath it.
func TestReleaseWaitsForOutstandingLeases(t *testing.T) {
	t.Parallel()

	build, _, _ := newFactory()

	releasedAt := make(chan time.Time, 1)
	g := &controls.Generational[*run]{
		Build: build,
		Release: func(ctx context.Context, r *run) error {
			releasedAt <- time.Now()

			return nil
		},
	}

	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	inUse := make(chan struct{})
	leaseDone := make(chan time.Time, 1)

	go func() {
		_ = g.Use(func(*run) error {
			close(inUse)
			time.Sleep(80 * time.Millisecond)
			leaseDone <- time.Now()

			return nil
		})
	}()

	<-inUse

	if err := g.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	left, rel := <-leaseDone, <-releasedAt

	if rel.Before(left) {
		t.Errorf("Release ran %v before the lease returned", left.Sub(rel))
	}
}

// D7: admission closes BEFORE the drain, so a caller publishing continuously
// cannot starve Stop.
func TestStopTerminatesUnderContinuousUse(t *testing.T) {
	t.Parallel()

	build, _, _ := newFactory()
	g := &controls.Generational[*run]{Build: build, Release: release}

	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	var (
		stop    = make(chan struct{})
		refused atomic.Int64
		wg      sync.WaitGroup
	)

	for i := 0; i < 8; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for {
				select {
				case <-stop:
					return
				default:
				}

				if err := g.Use(func(*run) error { return nil }); errors.Is(err, controls.ErrNoGeneration) {
					refused.Add(1)
				}
			}
		}()
	}

	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := g.Stop(ctx); err != nil {
		t.Fatalf("Stop did not terminate under continuous Use: %v", err)
	}

	close(stop)
	wg.Wait()

	if refused.Load() == 0 {
		t.Error("no Use was refused after Stop; admission never closed")
	}
}

// D1: a second concurrent Start must not build a rival generation.
func TestSecondStartIsRefused(t *testing.T) {
	t.Parallel()

	build, made, mu := newFactory()
	g := &controls.Generational[*run]{Build: build, Release: release}

	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("first start: %v", err)
	}

	if err := g.Start(context.Background()); !errors.Is(err, controls.ErrGenerationRunning) {
		t.Errorf("second Start returned %v, want ErrGenerationRunning", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(*made) != 1 {
		t.Errorf("built %d runs, want 1: a rival generation was constructed", len(*made))
	}
}

// D1: a failed Build installs nothing and leaves the value startable again.
func TestFailedBuildInstallsNothing(t *testing.T) {
	t.Parallel()

	boom := errors.New("build failed")
	var attempts atomic.Int64

	g := &controls.Generational[*run]{
		Build: func(context.Context) (*run, error) {
			if attempts.Add(1) == 1 {
				return nil, boom
			}

			return &run{id: 2}, nil
		},
		Release: release,
	}

	if err := g.Start(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Start returned %v, want the build error", err)
	}

	if err := g.Use(func(*run) error { return nil }); !errors.Is(err, controls.ErrNoGeneration) {
		t.Errorf("after a failed Start, Use returned %v, want ErrNoGeneration", err)
	}

	if err := g.Start(context.Background()); err != nil {
		t.Errorf("a failed Start left the value unstartable: %v", err)
	}
}

// D7: Stop is idempotent and a Stop with nothing running is not an error.
func TestStopIsIdempotent(t *testing.T) {
	t.Parallel()

	build, made, mu := newFactory()
	g := &controls.Generational[*run]{Build: build, Release: release}

	if err := g.Stop(context.Background()); err != nil {
		t.Errorf("Stop with nothing running: %v", err)
	}

	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := g.Stop(context.Background()); err != nil {
			t.Errorf("Stop %d: %v", i, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()

	if len(*made) != 1 {
		t.Fatalf("built %d runs", len(*made))
	}

	if !(*made)[0].released.Load() {
		t.Error("the run was never released")
	}
}

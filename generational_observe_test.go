package controls_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"gitlab.com/phpboyscout/go/controls"
)

// D8 and the safety property the whole spec rests on: a stopped generation is
// never reported healthy. Four of the five estate instances of this defect class
// failed by a torn-down thing continuing to look fine.
func TestHealthyNeverReportsAStoppedGeneration(t *testing.T) {
	t.Parallel()

	build, _, _ := newFactory()

	var probeErr error

	g := &controls.Generational[*run]{
		Build:   build,
		Release: release,
		Probe:   func(*run) error { return probeErr },
	}

	if err := g.Healthy(); !errors.Is(err, controls.ErrNoGeneration) {
		t.Errorf("Healthy before Start returned %v, want ErrNoGeneration", err)
	}

	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	if err := g.Healthy(); err != nil {
		t.Errorf("Healthy while running returned %v, want nil", err)
	}

	// A live generation's own opinion is passed through unchanged.
	sentinel := errors.New("the run says it is unwell")
	probeErr = sentinel

	if err := g.Healthy(); !errors.Is(err, sentinel) {
		t.Errorf("Healthy returned %v, want the probe's own error", err)
	}

	probeErr = nil

	if err := g.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// The whole point: a healthy probe on a released run must not make a
	// stopped generation look alive.
	if err := g.Healthy(); !errors.Is(err, controls.ErrNoGeneration) {
		t.Errorf("Healthy after Stop returned %v, want ErrNoGeneration", err)
	}
}

// A nil Probe means healthy while running, and still not healthy when stopped.
func TestHealthyWithoutAProbe(t *testing.T) {
	t.Parallel()

	build, _, _ := newFactory()
	g := &controls.Generational[*run]{Build: build, Release: release}

	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	if err := g.Healthy(); err != nil {
		t.Errorf("Healthy with no probe returned %v, want nil", err)
	}

	if err := g.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if err := g.Healthy(); !errors.Is(err, controls.ErrNoGeneration) {
		t.Errorf("Healthy after Stop returned %v, want ErrNoGeneration", err)
	}
}

// D8: the generation number increases by one per successful Start, never
// repeats, and reads zero when nothing is live. A caller uses it to tell a
// reactivation gap from a dead service.
func TestGenerationCountsUpAndReadsZeroWhenStopped(t *testing.T) {
	t.Parallel()

	build, _, _ := newFactory()
	g := &controls.Generational[*run]{Build: build, Release: release}

	if n := g.Generation(); n != 0 {
		t.Errorf("Generation before Start is %d, want 0", n)
	}

	for want := uint64(1); want <= 3; want++ {
		if err := g.Start(context.Background()); err != nil {
			t.Fatalf("start %d: %v", want, err)
		}

		if n := g.Generation(); n != want {
			t.Errorf("Generation is %d, want %d", n, want)
		}

		if err := g.Stop(context.Background()); err != nil {
			t.Fatalf("stop %d: %v", want, err)
		}

		if n := g.Generation(); n != 0 {
			t.Errorf("Generation after Stop is %d, want 0", n)
		}
	}
}

// D1: concurrent Starts must produce exactly one generation. An overwritten
// generation is wholly live and unowned, which is the same disease as a partial
// install, so the loser disposes of what it built rather than abandoning it.
func TestConcurrentStartsBuildExactlyOneLiveGeneration(t *testing.T) {
	t.Parallel()

	var (
		mu      sync.Mutex
		made    []*run
		next    int
		refused atomic.Int64
		started atomic.Int64
	)

	g := &controls.Generational[*run]{
		Build: func(context.Context) (*run, error) {
			mu.Lock()
			defer mu.Unlock()

			next++
			r := &run{id: next}
			made = append(made, r)

			return r, nil
		},
		Release: release,
	}

	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			switch err := g.Start(context.Background()); {
			case err == nil:
				started.Add(1)
			case errors.Is(err, controls.ErrGenerationRunning):
				refused.Add(1)
			default:
				t.Errorf("Start returned %v", err)
			}
		}()
	}

	wg.Wait()

	if started.Load() != 1 {
		t.Errorf("%d Starts succeeded, want exactly 1", started.Load())
	}

	if refused.Load() != 7 {
		t.Errorf("%d Starts were refused, want 7", refused.Load())
	}

	if g.Generation() == 0 {
		t.Fatal("no generation is live after a successful Start")
	}

	// Any generation built by a losing Start must have been disposed of rather
	// than leaked, so exactly one — the installed one — may be unreleased.
	mu.Lock()
	built, unreleased := len(made), countUnreleased(made)
	mu.Unlock()

	if unreleased != 1 {
		t.Errorf("%d of %d built generations are unreleased, want exactly 1 (the live one)",
			unreleased, built)
	}
}

func countUnreleased(runs []*run) int {
	n := 0

	for _, r := range runs {
		if !r.released.Load() {
			n++
		}
	}

	return n
}

// A Start that loses the install race releases what it built BEFORE returning.
//
// Nothing records a loser, so nothing would ever wait for it: an asynchronous
// release here means Start returns an error while the resources it built are
// still live, and no later Start is gated on them. A Start that fails must
// leave nothing behind.
//
// Forced rather than raced. TestConcurrentStartsBuildExactlyOneLiveGeneration
// only reaches this path when two goroutines both pass the first check before
// either installs, which is a narrow window it hits some runs and not others.
// Here both builds are held open until they are certain to be in flight.
func TestALosingStartReleasesWhatItBuiltBeforeReturning(t *testing.T) {
	t.Parallel()

	var (
		mu      sync.Mutex
		made    []*run
		next    int
		inBuild sync.WaitGroup
		proceed = make(chan struct{})
	)

	inBuild.Add(2)

	g := &controls.Generational[*run]{
		Build: func(context.Context) (*run, error) {
			mu.Lock()
			next++
			r := &run{id: next}
			made = append(made, r)
			mu.Unlock()

			// Both builds are in flight before either can install, so exactly
			// one of them must lose.
			inBuild.Done()
			<-proceed

			return r, nil
		},
		Release: release,
	}

	errs := make(chan error, 2)

	for range 2 {
		go func() { errs <- g.Start(context.Background()) }()
	}

	inBuild.Wait()
	close(proceed)

	var won, lost int

	for range 2 {
		switch err := <-errs; {
		case err == nil:
			won++
		case errors.Is(err, controls.ErrGenerationRunning):
			lost++
		default:
			t.Errorf("Start returned an unexpected error: %v", err)
		}
	}

	if won != 1 || lost != 1 {
		t.Fatalf("%d Starts won and %d lost, want exactly one of each", won, lost)
	}

	mu.Lock()
	built, unreleased := len(made), countUnreleased(made)
	mu.Unlock()

	if built != 2 {
		t.Fatalf("built %d generations, want 2", built)
	}

	// The assertion that matters: the loser's generation is ALREADY released
	// when its Start returned, with no waiting and no polling.
	if unreleased != 1 {
		t.Errorf("%d of %d generations are unreleased immediately after Start returned, want 1; "+
			"a failed Start left something behind", unreleased, built)
	}

	_ = g.Stop(context.Background())
}

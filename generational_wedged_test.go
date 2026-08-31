package controls_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"gitlab.com/phpboyscout/go/controls"
)

// D7: a Release that ignores its context blocks reactivation, loudly, rather
// than letting a second generation duplicate resources the first still holds.
// This is the only case that produces ErrPredecessorLive, and it is estate-owned
// code rather than consumer code — which is the whole point of where the
// boundary was drawn.
func TestWedgedReleaseBlocksReactivationLoudly(t *testing.T) {
	t.Parallel()

	unwedge := make(chan struct{})

	var built atomic.Int64

	g := &controls.Generational[*run]{
		Build: func(context.Context) (*run, error) {
			return &run{id: int(built.Add(1))}, nil
		},
		Release: func(ctx context.Context, r *run) error {
			// Ignores ctx entirely: nothing but the test frees it.
			<-unwedge
			r.released.Store(true)

			return nil
		},
	}

	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	start := time.Now()
	err := g.Stop(stopCtx)
	took := time.Since(start)

	stopCancel()

	// Bounded even against a Release that will never return.
	if !errors.Is(err, controls.ErrStopTimeout) {
		t.Errorf("Stop returned %v, want ErrStopTimeout", err)
	}

	if took > 300*time.Millisecond {
		t.Errorf("Stop took %v against a 100ms budget: it is not bounded", took)
	}

	// And the generation is not replaced while it still holds resources.
	startCtx, startCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err = g.Start(startCtx)

	startCancel()

	if !errors.Is(err, controls.ErrPredecessorLive) {
		t.Fatalf("Start returned %v, want ErrPredecessorLive", err)
	}

	if n := built.Load(); n != 1 {
		t.Errorf("built %d generations while the first held resources, want 1", n)
	}

	// The gate opens only once the resources are ACTUALLY released, not when
	// somebody gave up waiting for them.
	close(unwedge)

	deadline := time.Now().Add(2 * time.Second)

	for {
		if err := g.Start(context.Background()); err == nil {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("the gate never opened after the release completed")
		}

		time.Sleep(10 * time.Millisecond)
	}

	if n := built.Load(); n != 2 {
		t.Errorf("built %d generations, want 2 after the release completed", n)
	}
}

// D7's contract term, asserted: Release must be safe against a lease that
// outlived the stop budget, because such a lease is disowned rather than waited
// for. A consumer whose Release assumed exclusivity would corrupt here.
func TestReleaseRunsSafelyBesideADisownedLease(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	defer close(release)

	var (
		releaseRan atomic.Bool
		inUse      = make(chan struct{})
	)

	g := &controls.Generational[*run]{
		Build: func(context.Context) (*run, error) { return &run{id: 1}, nil },
		Release: func(ctx context.Context, r *run) error {
			releaseRan.Store(true)

			return nil
		},
	}

	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	go func() {
		_ = g.Use(func(*run) error {
			close(inUse)
			<-release // ignores cancellation, exactly like a wedged handler

			return nil
		})
	}()

	<-inUse

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	// Stop returns within budget even though the lease is still held.
	if err := g.Stop(ctx); err != nil && !errors.Is(err, controls.ErrStopTimeout) {
		t.Fatalf("Stop returned %v", err)
	}

	deadline := time.Now().Add(time.Second)

	for !releaseRan.Load() {
		if time.Now().After(deadline) {
			t.Fatal("Release never ran while a lease was disowned; the stop is not bounded")
		}

		time.Sleep(5 * time.Millisecond)
	}

	// And the disowned lease's generation is not reachable any more.
	if err := g.Use(func(*run) error { return nil }); !errors.Is(err, controls.ErrNoGeneration) {
		t.Errorf("Use returned %v after the generation was released, want ErrNoGeneration", err)
	}
}

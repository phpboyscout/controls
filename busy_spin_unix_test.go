//go:build unix

package controls_test

import (
	"context"
	"log/slog"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.com/phpboyscout/go/controls"
)

// cpuUsed returns the CPU time this process has consumed, user plus system.
func cpuUsed(t *testing.T) time.Duration {
	t.Helper()

	var ru syscall.Rusage

	require.NoError(t, syscall.Getrusage(syscall.RUSAGE_SELF, &ru))

	return time.Duration(ru.Utime.Nano()) + time.Duration(ru.Stime.Nano())
}

// TestErrorHandler_SpinBurnsNoCPUDuringShutdown asserts the half its sibling
// cannot see.
//
// TestErrorHandler_NoBusySpinAfterStop measures goroutine counts, which catches
// a handler that never RETURNS. It does not catch the defect it is named for:
// the D4 busy-spin is a handler that loops on a permanently-ready ctx.Done()
// case and then exits perfectly well when shutdown completes. Reinstating that
// bug leaves the sibling green.
//
// A spin is CPU, so this measures CPU, and it holds shutdown open long enough
// for a spin to show. The window is the gap between the parent context being
// cancelled and shutdownComplete closing: with `done` left ready, the handler
// spins for the whole of it; with `done = nil`, it blocks.
//
// # It must not call t.Parallel()
//
// Getrusage measures the PROCESS. A parallel test doing real work alongside
// this one is indistinguishable from a spin. A Go test that does not call
// t.Parallel() runs while every parallel test in the package is paused.
func TestErrorHandler_SpinBurnsNoCPUDuringShutdown(t *testing.T) {
	const window = 500 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := controls.NewController(ctx,
		controls.WithLogger(slog.New(slog.DiscardHandler)),
		controls.WithShutdownTimeout(5*time.Second),
	)

	// A stop that takes its time is what widens the window. Without it the gap
	// between cancelling the parent and shutdown completing is too short for a
	// spin to accumulate measurable CPU, and the test would pass against the bug.
	c.Register("slow-stop",
		controls.WithStart(func(runCtx context.Context) error { <-runCtx.Done(); return nil }),
		controls.WithStop(func(context.Context) { time.Sleep(window) }),
	)

	c.Start()
	time.Sleep(50 * time.Millisecond)

	before := cpuUsed(t)
	cancel() // the handler's `done` case becomes permanently ready here

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()

	require.NoError(t, c.WaitContext(waitCtx), "shutdown did not complete")

	used := cpuUsed(t) - before

	// A spinning select consumes a whole core for the window. Sleeping consumes
	// almost nothing. A tenth of the window is far above the real cost of a
	// shutdown and far below a spin, so this discriminates without being tight.
	limit := window / 10

	require.Less(t, used, limit,
		"the error handler burned %s of CPU while shutting down over %s; a select on a permanently-ready case is spinning",
		used, window)
}

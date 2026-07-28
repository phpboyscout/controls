package controls_test

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.com/phpboyscout/go/controls"
)

// TestSignalsNotSwallowedBeforeStart proves F5: constructing a controller (with
// signals enabled) but never calling Start must not register an OS-signal handler
// that has no reader — which would leave the process permanently ignoring
// SIGINT/SIGTERM.
//
// It re-executes the test binary as a child that constructs a controller and
// blocks without starting it, then sends SIGINT. With the fix, no handler is
// registered before Start, so SIGINT keeps its default disposition and the child
// terminates. With the bug, signal.Notify was called at construction with no
// reader, so SIGINT is swallowed and the child never exits.
func TestSignalsNotSwallowedBeforeStart(t *testing.T) {
	if os.Getenv("CONTROLS_SIGNAL_CHILD") == "1" {
		// Child process: construct a controller with signals enabled, never Start.
		controls.NewController(context.Background())
		time.Sleep(30 * time.Second) // block; SIGINT should terminate us

		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	//nolint:gosec // G204: deliberate self-exec of the test binary for a
	// signal-disposition subprocess; args are the fixed test name, not user input.
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSignalsNotSwallowedBeforeStart$", "-test.v")
	cmd.Env = append(os.Environ(), "CONTROLS_SIGNAL_CHILD=1")

	require.NoError(t, cmd.Start())

	// Give the child time to construct the controller before signalling.
	time.Sleep(500 * time.Millisecond)

	require.NoError(t, cmd.Process.Signal(syscall.SIGINT))

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	select {
	case err := <-waitErr:
		// Terminated by the signal (non-zero exit) — SIGINT kept its default
		// disposition. That is the correct, fixed behaviour.
		require.Error(t, err, "child should be terminated by SIGINT")
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("controller constructed-but-not-started swallowed SIGINT; process did not terminate")
	}
}

package controls

import (
	"context"
	"testing"
	"time"
)

// TestStop_ReturnsAfterShutdownComplete proves F2: if the shutdown sequence has
// already completed (shutdownComplete closed) between the CAS and the channel
// send, Stop() must not block forever on the unbuffered message channel whose
// receiver has gone. This white-box test forces exactly that interleaving.
func TestStop_ReturnsAfterShutdownComplete(t *testing.T) {
	t.Parallel()

	c := NewController(context.Background(), WithoutSignals())

	// Simulate: caller A won the CAS (Running), then the full shutdown ran and
	// the message processor exited before A's send.
	c.SetState(Running)
	close(c.shutdownComplete)

	done := make(chan struct{})

	go func() {
		c.Stop()
		close(done)
	}()

	select {
	case <-done:
		// Stop returned promptly — the send was guarded on shutdownComplete.
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() hung on a message-channel send after shutdown completed")
	}
}

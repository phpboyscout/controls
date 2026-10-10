package controls_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"gitlab.com/phpboyscout/go/controls"
)

// A consumer reading the controller's own channel receives every failure: the
// controller no longer reads it too (issue 19). Counted rather than ranged, so
// that against a controller that never closes the channel it fails on
// delivery, not on a hang.
func TestEveryFailureReachesTheReaderOfErrors(t *testing.T) {
	t.Parallel()

	const services = 20

	for range 50 {
		c := controls.NewController(context.Background())
		errs := c.Errors()

		for i := range services {
			err := fmt.Errorf("svc-%d failed", i)
			c.Register(fmt.Sprintf("svc-%d", i), controls.WithStart(func(context.Context) error { return err }))
		}

		c.Start()

		received := 0
		timeout := time.After(2 * time.Second)

	read:
		for received < services {
			select {
			case err := <-errs:
				if err == nil {
					t.Fatal("received nil from Errors()")
				}

				received++
			case <-timeout:
				break read
			}
		}

		c.Stop()

		if err := c.WaitContext(waitBudget(t)); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}

		if received != services {
			t.Fatalf("received %d of %d failures; something else is reading Errors()", received, services)
		}
	}
}

func waitBudget(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	return ctx
}

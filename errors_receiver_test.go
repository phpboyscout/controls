package controls_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.com/phpboyscout/go/controls"
)

// A consumer that replaces the error channel before Start is the receiver, the
// only one. Twenty services each fail once into a channel nothing reads until
// shutdown is over: every error must still be there. When the controller's own
// handler competes for the channel, it eats a share and logs them instead,
// and the consumer never sees those (issue 14).
func TestAReplacedErrorChannelHasOneReceiver(t *testing.T) {
	t.Parallel()

	const services = 20

	errs := make(chan error, services)

	c := controls.NewController(context.Background())
	c.SetErrorsChannel(errs)

	for i := range services {
		err := fmt.Errorf("svc-%d failed", i)
		c.Register(fmt.Sprintf("svc-%d", i), controls.WithStart(func(context.Context) error { return err }))
	}

	c.Start()

	require.Eventually(t, func() bool {
		for i := range services {
			info, _ := c.GetServiceInfo(fmt.Sprintf("svc-%d", i))
			if info.Error == nil {
				return false
			}
		}

		return true
	}, 5*time.Second, 5*time.Millisecond, "every service should have failed")

	c.Stop()
	c.Wait()

	require.Len(t, errs, services, "every forwarded error must reach the consumer's channel")
}

// A controller whose channel was not replaced still drains and logs its own,
// so a failing service never blocks its supervisor on a channel nobody reads.
func TestTheDefaultErrorChannelIsStillDrainedByTheController(t *testing.T) {
	t.Parallel()

	c := controls.NewController(context.Background())
	c.Register("svc", controls.WithStart(func(context.Context) error { return errors.New("boom") }))
	c.Start()

	require.Eventually(t, func() bool {
		info, _ := c.GetServiceInfo("svc")

		return info.Error != nil
	}, 5*time.Second, 5*time.Millisecond)

	done := make(chan struct{})

	go func() {
		c.Stop()
		c.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown hung: the unreplaced channel was not drained")
	}
}

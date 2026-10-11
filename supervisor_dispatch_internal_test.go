package controls

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestSupervisorFailureDeliveryEndsAtStop — Stop never joins the delivery
// goroutine, so nothing public shows it leaking; its exit is checked here.
func TestSupervisorFailureDeliveryEndsAtStop(t *testing.T) {
	t.Parallel()

	delivered := make(chan struct{}, 1)

	s := NewSupervisor(WithOnFailure(func(Failure) { delivered <- struct{}{} }))
	_ = s.Attach(Child{Name: "doomed", Start: func(context.Context) error { return errors.New("nope") }})
	_ = s.Start(t.Context())

	<-delivered
	s.Stop(t.Context())

	select {
	case <-s.failuresFwd.finished():
	case <-time.After(3 * time.Second):
		t.Fatal("the failure delivery goroutine outlived Stop")
	}
}

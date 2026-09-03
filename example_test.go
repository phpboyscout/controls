package controls_test

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"gitlab.com/phpboyscout/go/errors"

	"gitlab.com/phpboyscout/go/controls"
)

func ExampleNewController() {
	ctx := context.Background()

	// Create a controller. No OS signal handler is installed by default; a
	// standalone daemon that owns signals adds controls.WithSignals().
	controller := controls.NewController(ctx)

	// Register an HTTP service
	controller.Register("http-api",
		controls.WithStart(func(ctx context.Context) error {
			fmt.Println("HTTP server starting")
			return nil
		}),
		controls.WithStop(func(ctx context.Context) {
			fmt.Println("HTTP server stopping")
		}),
		controls.WithStatus(func() error {
			return nil // healthy
		}),
	)

	// Start all services
	controller.Start()

	// Graceful shutdown
	time.Sleep(10 * time.Millisecond)
	controller.Stop()
	controller.Wait()
}

func ExampleWithRestartPolicy() {
	controller := controls.NewController(context.Background())

	controller.Register("worker",
		controls.WithStart(func(ctx context.Context) error {
			return nil
		}),
		controls.WithRestartPolicy(controls.RestartPolicy{
			MaxRestarts:    3,
			InitialBackoff: time.Second,
			MaxBackoff:     30 * time.Second,
		}),
	)

	_ = controller
}

func ExampleWithLiveness() {
	controller := controls.NewController(context.Background())

	controller.Register("api",
		controls.WithStart(func(ctx context.Context) error { return nil }),
		controls.WithLiveness(func() error {
			// Check if the service can respond
			resp, err := http.Get("http://localhost:8080/healthz")
			if err != nil {
				return err
			}

			_ = resp.Body.Close()

			return nil
		}),
	)

	_ = controller
}

// A stop that reports how it ended lands on ServiceInfo.StopErr, and the
// controller changes nothing else on the strength of it.
func ExampleWithStopErr() {
	controller := controls.NewController(context.Background())

	controller.Register("holder",
		controls.WithStart(func(ctx context.Context) error {
			<-ctx.Done()

			return ctx.Err()
		}),
		controls.WithStopErr(func(context.Context) error {
			return errors.New("listener still open")
		}),
	)

	controller.Start()
	controller.Stop()
	controller.Wait()

	info, _ := controller.GetServiceInfo("holder")
	fmt.Println("stop error:", info.StopErr)
	// Output:
	// stop error: listener still open
}

// A Supervisor accepts children before and after Start. A child that returns
// nil has finished; one with no restart policy that returns an error is
// reported to the consumer once, as a Failure.
func ExampleSupervisor() {
	sup := controls.NewSupervisor(
		controls.WithOnFailure(func(f controls.Failure) {
			fmt.Println("failed:", f.Name, "after", f.Restarts, "restarts")
		}),
	)

	_ = sup.Attach(controls.Child{
		Name: "once",
		Start: func(context.Context) error {
			fmt.Println("once ran")

			return nil
		},
	})

	if err := sup.Start(context.Background()); err != nil {
		fmt.Println("start:", err)

		return
	}

	// Attaching after Start is the point: the child is supervised identically.
	_ = sup.Attach(controls.Child{
		Name: "worker",
		Start: func(ctx context.Context) error {
			<-ctx.Done()

			return ctx.Err()
		},
	})

	_ = sup.Attach(controls.Child{
		Name:  "broken",
		Start: func(context.Context) error { return errors.New("no such queue") },
	})

	for {
		h := sup.Health()
		if h["once"].State == controls.ChildStopped &&
			h["worker"].State == controls.ChildRunning &&
			h["broken"].State == controls.ChildFailed {
			break
		}

		time.Sleep(time.Millisecond)
	}

	// The failed child does not make the supervisor unready.
	fmt.Println("ready:", sup.Readiness() == nil)
	fmt.Println("check:", sup.HealthCheck("children").Check(context.Background()).Message)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	sup.Stop(ctx)
	fmt.Println("after stop:", errors.Is(sup.Readiness(), controls.ErrSupervisorStopped))
	// Unordered output:
	// once ran
	// failed: broken after 0 restarts
	// ready: true
	// check: 1 of 3 supervised children have failed
	// after stop: true
}

type conn struct{ id int }

// A Generational hands each Start a value built fresh, refuses a second Start
// while one is live, and refuses Use once stopped rather than serving a stale
// handle.
func ExampleGenerational() {
	built := 0

	g := &controls.Generational[*conn]{
		Build: func(context.Context) (*conn, error) {
			built++

			return &conn{id: built}, nil
		},
		Release: func(_ context.Context, c *conn) error {
			fmt.Println("released", c.id)

			return nil
		},
	}

	ctx := context.Background()
	show := func(c *conn) error {
		fmt.Println("using", c.id, "in generation", g.Generation())

		return nil
	}

	fmt.Println("use before start:", errors.Is(g.Use(show), controls.ErrNoGeneration))

	_ = g.Start(ctx)
	_ = g.Use(show)
	fmt.Println("second start:", errors.Is(g.Start(ctx), controls.ErrGenerationRunning))

	_ = g.Stop(ctx)
	fmt.Println("use after stop:", errors.Is(g.Use(show), controls.ErrNoGeneration), "generation", g.Generation())

	_ = g.Start(ctx)
	_ = g.Use(show)
	_ = g.Stop(ctx)
	// Output:
	// use before start: true
	// using 1 in generation 1
	// second start: true
	// released 1
	// use after stop: true generation 0
	// using 2 in generation 2
	// released 2
}

// Package clean holds shapes the analyzer must stay silent on.
package clean

import (
	"context"
	"net"
	"net/http"

	"gitlab.com/phpboyscout/go/controls"
	"google.golang.org/grpc"
)

// The server is built inside the run: a restart builds a new one.
func builtInsideTheRun(c *controls.Controller) {
	c.Register("grpc",
		controls.WithStart(func(ctx context.Context) error {
			srv := grpc.NewServer()

			lis, err := net.Listen("tcp", ":0")
			if err != nil {
				return err
			}

			return srv.Serve(lis)
		}),
	)
}

// A Start method that assigns the field it reads builds fresh per run.
type rebuilt struct {
	sup *controls.Supervisor
}

func (r *rebuilt) Start(ctx context.Context) error {
	r.sup = controls.NewSupervisor()

	return r.sup.Start(ctx)
}

// The Generational shape: the supervisor is built by Build, which is not a
// Start method and takes the value it needs by return.
type generational struct {
	build func(ctx context.Context) (*controls.Supervisor, error)
}

func (g *generational) Start(ctx context.Context) error {
	sup, err := g.build(ctx)
	if err != nil {
		return err
	}

	return sup.Start(ctx)
}

// Nothing single-use anywhere.
func nothingSingleUse(c *controls.Controller, addr string) {
	c.Register("worker",
		controls.WithStart(func(ctx context.Context) error {
			<-ctx.Done()

			return ctx.Err()
		}),
		controls.WithStop(func(ctx context.Context) {}),
	)

	_ = addr
}

// A method named Start with the wrong shape is not a StartFunc.
type notAStartFunc struct {
	srv *http.Server
}

func (n *notAStartFunc) Start() error { return n.srv.ListenAndServe() }

// A StartFunc built by a call whose arguments hold nothing single-use.
func startWith(addr string) controls.StartFunc {
	return func(ctx context.Context) error { return nil }
}

func builtByCleanCall(c *controls.Controller) {
	c.Register("http", controls.WithStart(startWith(":0")))
}

// ---- shapes added after review ----

// Naming a single-use TYPE inside the closure is not a capture.
func namesTheType(c *controls.Controller) {
	c.Register("l", controls.WithStart(func(ctx context.Context) error {
		var l net.Listener

		l, err := net.Listen("tcp", ":0")
		if err != nil {
			return err
		}

		if _, ok := any(l).(net.Listener); !ok {
			return nil
		}

		return l.Close()
	}))
}

// A closure that rebuilds the captured variable builds per run.
func rebuildsInClosure(c *controls.Controller) {
	var srv *http.Server

	c.Register("http",
		controls.WithStart(func(ctx context.Context) error {
			srv = &http.Server{Addr: ":0"}

			return srv.ListenAndServe()
		}),
		controls.WithStop(func(ctx context.Context) { _ = srv.Close() }),
	)
}

// A Start method that rebuilds a nested field builds per run.
type inner struct {
	sup *controls.Supervisor
}

type outer struct {
	in inner
}

func (o *outer) Start(ctx context.Context) error {
	o.in.sup = controls.NewSupervisor()

	return o.in.sup.Start(ctx)
}

// A method value on a value built inside the closure.
func methodValueBuiltInside(c *controls.Controller) {
	c.Register("sup", controls.WithStart(func(ctx context.Context) error {
		sup := controls.NewSupervisor()

		return sup.Start(ctx)
	}))
}

// A local alias for a single-use type, named as a TYPE inside the closure.
type L = net.Listener

func namesALocalTypeAlias(c *controls.Controller) {
	c.Register("l", controls.WithStart(func(ctx context.Context) error {
		var l L

		l, err := net.Listen("tcp", ":0")
		if err != nil {
			return err
		}

		return l.Close()
	}))
}

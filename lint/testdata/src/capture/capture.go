// Package capture holds the four shapes the analyzer must flag, each reduced
// from an instance measured in the estate.
package capture

import (
	"context"
	"net"
	"net/http"

	"gitlab.com/phpboyscout/go/controls"
	"google.golang.org/grpc"
	"wiring"
)

// A closure that names a server built at wiring.
func closureNamesServer(c *controls.Controller) {
	srv := grpc.NewServer()

	c.Register("grpc",
		controls.WithStart(func(ctx context.Context) error {
			lis, err := net.Listen("tcp", ":0")
			if err != nil {
				return err
			}

			return srv.Serve(lis) // want `StartFunc closure captures srv \(\*google.golang.org/grpc.Server\) built outside the run; a restart reuses it`
		}),
	)
}

func start(srv *http.Server) controls.StartFunc {
	return func(ctx context.Context) error { return srv.ListenAndServe() }
}

// A StartFunc built by a call whose argument is the server: go/transport HTTP.
func builtByCall(c *controls.Controller) {
	srv := &http.Server{Addr: ":0"}

	c.Register("http",
		controls.WithStart(start(srv)), // want `StartFunc is built by a call that captures srv \(\*net/http.Server\); a restart reuses it`
	)
}

func startGRPC(srv *grpc.Server) controls.StartFunc {
	return func(ctx context.Context) error { return nil }
}

// A closure capturing a StartFunc variable, followed back to the call that
// built it: go/transport gRPC.
func capturedStartFunc(c *controls.Controller) {
	srv := grpc.NewServer()
	startServer := startGRPC(srv) // want `StartFunc closure uses startServer, built by a call that captures srv \(\*google.golang.org/grpc.Server\); a restart reuses it`

	c.Register("grpc",
		controls.WithStart(func(ctx context.Context) error {
			return startServer(ctx)
		}),
	)
}

// A closure reaching a server through a captured struct.
type holder struct {
	srv *http.Server
}

func reachedThroughStruct(c *controls.Controller, h *holder) {
	c.Register("http",
		controls.WithStart(func(ctx context.Context) error {
			return h.srv.ListenAndServe() // want `StartFunc closure reaches h.srv \(\*net/http.Server\) through captured h; a restart reuses it`
		}),
	)
}

// A Start method reading a supervisor field built in the constructor:
// go/messaging before its rebuild, and scoutdm's Tables.
type tables struct {
	sup *controls.Supervisor
}

func newTables() *tables { return &tables{sup: controls.NewSupervisor()} }

func (t *tables) Start(ctx context.Context) error {
	return t.sup.Start(ctx) // want `Start reads receiver field sup \(\*gitlab.com/phpboyscout/go/controls.Supervisor\) it did not build; a restart reuses it`
}

// The same shapes on a Child.
func childCapturesServer(sup *controls.Supervisor) {
	srv := grpc.NewServer()

	_ = sup.Attach(controls.Child{
		Name: "api",
		Start: func(ctx context.Context) error {
			return srv.Serve(nil) // want `Child.Start closure captures srv \(\*google.golang.org/grpc.Server\) built outside the run; a restart reuses it`
		},
	})
}

var _ = newTables

// ---- shapes added after review ----

// The method value the tutorial recommends: WithStart(sup.Start).
func methodValue(c *controls.Controller) {
	sup := controls.NewSupervisor()

	c.Register("sup", controls.WithStart(sup.Start)) // want `StartFunc is the Start method of sup \(\*gitlab.com/phpboyscout/go/controls.Supervisor\) built outside the run; a restart reuses it`
}

func childMethodValue(parent *controls.Supervisor) {
	sup := controls.NewSupervisor()

	_ = parent.Attach(controls.Child{Name: "n", Start: sup.Start}) // want `Child.Start is the Start method of sup \(\*gitlab.com/phpboyscout/go/controls.Supervisor\) built outside the run; a restart reuses it`
}

// A named function reaching a package-level server.
var pkgSrv = &http.Server{Addr: ":0"}

func serve(ctx context.Context) error {
	return pkgSrv.ListenAndServe() // want `StartFunc serve reaches pkgSrv \(\*net/http.Server\) built outside the run; a restart reuses it`
}

func namedFunc(c *controls.Controller) {
	c.Register("http", controls.WithStart(serve))
}

// A conversion around the closure changes nothing.
func converted(c *controls.Controller) {
	srv := &http.Server{Addr: ":0"}

	c.Register("http", controls.WithStart(controls.StartFunc(func(ctx context.Context) error {
		return srv.ListenAndServe() // want `StartFunc closure captures srv \(\*net/http.Server\) built outside the run; a restart reuses it`
	})))
}

// A StartFunc variable declared with var, and one assigned after declaration.
func varDeclared(c *controls.Controller) {
	srv := &http.Server{Addr: ":0"}

	var startServer = start(srv) // want `StartFunc uses startServer, built by a call that captures srv \(\*net/http.Server\); a restart reuses it`

	c.Register("http", controls.WithStart(startServer))
}

func assignedLater(c *controls.Controller) {
	srv := &http.Server{Addr: ":0"}

	var startServer controls.StartFunc

	startServer = start(srv) // want `StartFunc uses startServer, built by a call that captures srv \(\*net/http.Server\); a restart reuses it`

	c.Register("http", controls.WithStart(startServer))
}

// An alias, a value-typed field, an embedded server, a Unix listener.
type S = http.Server

func aliased(c *controls.Controller) {
	srv := &S{Addr: ":0"}

	c.Register("http", controls.WithStart(func(ctx context.Context) error {
		return srv.ListenAndServe() // want `StartFunc closure captures srv \(\*net/http.Server\) built outside the run; a restart reuses it`
	}))
}

type byValue struct {
	srv http.Server
}

func valueField(c *controls.Controller, h *byValue) {
	c.Register("http", controls.WithStart(func(ctx context.Context) error {
		return h.srv.ListenAndServe() // want `StartFunc closure reaches h.srv \(net/http.Server\) through captured h; a restart reuses it`
	}))
}

type embedded struct {
	*http.Server
}

func promoted(c *controls.Controller, e *embedded) {
	c.Register("http", controls.WithStart(func(ctx context.Context) error {
		return e.ListenAndServe() // want `StartFunc closure reaches e.Server \(\*net/http.Server\) through captured e; a restart reuses it`
	}))
}

func unixListener(c *controls.Controller, ul *net.UnixListener) {
	c.Register("unix", controls.WithStart(func(ctx context.Context) error {
		_, err := ul.Accept() // want `StartFunc closure captures ul \(\*net.UnixListener\) built outside the run; a restart reuses it`

		return err
	}))
}

// A Start method reaching the supervisor through a nested field.
type deps struct {
	sup *controls.Supervisor
}

type nested struct {
	deps deps
}

func (n *nested) Start(ctx context.Context) error {
	return n.deps.sup.Start(ctx) // want `Start reads receiver field deps.sup \(\*gitlab.com/phpboyscout/go/controls.Supervisor\) it did not build; a restart reuses it`
}

// A cross-package package-level server, reported once.
func crossPackage(c *controls.Controller) {
	c.Register("http", controls.WithStart(func(ctx context.Context) error {
		return wiring.Srv.ListenAndServe() // want `StartFunc closure captures wiring.Srv \(\*net/http.Server\) built outside the run; a restart reuses it`
	}))
}

// An indexed argument is named as written.
func indexedArg(c *controls.Controller, servers []*http.Server) {
	c.Register("http", controls.WithStart(start(servers[0]))) // want `StartFunc is built by a call that captures servers\[0\] \(\*net/http.Server\); a restart reuses it`
}

// A StartFunc variable referenced twice is reported once.
func referencedTwice(c *controls.Controller) {
	srv := &http.Server{Addr: ":0"}
	startServer := start(srv) // want `StartFunc closure uses startServer, built by a call that captures srv \(\*net/http.Server\); a restart reuses it`

	c.Register("http", controls.WithStart(func(ctx context.Context) error {
		if err := startServer(ctx); err != nil {
			return err
		}

		return startServer(ctx)
	}))
}

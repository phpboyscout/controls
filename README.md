# Controls package

A simple controller to allow for managing the starting and stopping of multiple services running concurrently.

Create a controller and register one or more services.
These services will be started and managed simultaneously.
Sharing control channels for teh handling of `Errors`, `Context`, `Signals` and also an internal `Message` to trigger Starting and stopping services.

This includes a `HealthMessage` construct and channel that can be accessed if required to request a `status` of a registered service. N.B. This is not demonstrated in the example below.


```go
func start(srv http.Server) func() error {
    return func() {
        err = srv.ListenAndServe()
        if err != nil && err != http.ErrServerClosed {
            return err
        }
    }
}

func stop(ctx context.Context, srv *HTTPServer) func() {
	return func() {
		if err := srv.Shutdown(ctx); err != nil {
			log.Logger.Error(err)
		}
	}
}

func main() {
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    controller := controls.NewController(ctx,
        controls.WithLogger(props.Logger),
    )

    srv := http.Server{
        Addr:    ":8080"
        Handler:  http.NewServeMux()
    },

    controller.Register(id, start(srv), stop(ctx, srv), func(){})

    controller.Start()
    controller.Wait()
}
```

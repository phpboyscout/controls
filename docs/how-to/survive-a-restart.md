# Survive a restart

A service that holds something (a listener, a connection, a supervisor, a
session) has to hand back a *new* one every time it starts, and refuse access to
the old one once it has stopped. `Generational[R]` does both, so you write what
to build and what to release and nothing else.

If your service captures nothing single-use, you do not need this. Reach for it
when a restart would otherwise hand your second run the first run's corpse; the
[restart supervisor](../explanation/restart-supervisor.md#what-a-restart-shares-and-what-it-must-not)
explains why that happens.

## Wire it

```go
type run struct {
    lis net.Listener
    srv *grpc.Server
}

g := &controls.Generational[*run]{
    // Called once per Start. Everything acquired here must be reachable from
    // the value you return, or it leaks.
    Build: func(ctx context.Context) (*run, error) {
        lis, err := net.Listen("tcp", addr)
        if err != nil {
            return nil, err
        }

        srv := grpc.NewServer()
        registerServices(srv) // replayed per run, not captured once

        go func() { _ = srv.Serve(lis) }()

        return &run{lis: lis, srv: srv}, nil
    },

    // Called once per Stop. A nil error means everything is released, and that
    // is the only thing that permits the next Start.
    Release: func(ctx context.Context, r *run) error {
        r.srv.GracefulStop()

        return nil
    },
}

controller.Register("api",
    controls.WithStart(g.Start),
    controls.WithStopErr(g.Stop),
    controls.WithStatus(g.Healthy),
)
```

`WithStopErr` rather than `WithStop`, because `g.Stop` returns an error: a
`StopErrFunc` reports whether every resource was released, and the controller
records the answer on `ServiceInfo.StopErr`. A plain `WithStop` cannot say
either way.

## Reach the live run

There is deliberately no method handing you the value to keep. Ask for it inside
a callback instead:

```go
err := g.Use(func(r *run) error {
    return r.doSomething()
})
```

`Use` holds a lease for the duration, so a stop cannot release the run underneath
you. With no live generation it returns `ErrNoGeneration`: never a zero value
and never a stale one.

!!! warning "A handle you keep is a handle nobody can protect"

    `Use` is the only accessor on purpose. A value stashed outside it survives the
    generation that owned it, and a stale handle that still answers is one of the
    two failures this type exists to prevent. If you must hand something out, wrap
    it so each operation re-checks, and make `Release` leave the value refusing
    further use.

## What `Stop` promises, and what it does not

`Stop` returns within your budget, reporting `ErrStopTimeout` if it expired, and
two generations never overlap.

What it does **not** promise is that everything has finished running. A callback
that ignores its own cancellation cannot be ended in Go, so it is disowned rather
than waited for: its resources are released and it can no longer reach the
generation, but its goroutine may still be alive. That is why `Stop` is bounded
rather than honest-but-hanging, and it is the deliberate trade.

## When the next start is refused

`Release` is retried until it succeeds, because a resource left live while its
successor duplicates it is worse than being unavailable. So a `Release` that
never returns blocks the next `Start` with `ErrPredecessorLive`, loudly. A
`Start` while a generation is already live is refused too, with
`ErrGenerationRunning`, rather than building a rival alongside it.

`ErrPredecessorLive` is a bug in the release path rather than something to work
around: if you see it, something your service holds is not honouring its
context.

## Related

- [Generational reference](../reference/generational.md): every field, method
  and sentinel, and what each call does out of order.
- [The restart supervisor](../explanation/restart-supervisor.md): when a restart
  happens at all.
- [Register services](register-services.md): the options this composes with.
- [Find a capture a restart cannot reuse](lint-single-use-captures.md): the
  analyzer that names the line this type exists to fix.

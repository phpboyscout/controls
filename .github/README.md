# controls

**Service-lifecycle supervisor for Go.** Concurrent startup, health probes,
ordered shutdown in reverse registration order, and self-healing restarts, so a
process made of several services starts and stops predictably.

> **This is a read-only mirror. The canonical repository is on GitLab:**
> **https://gitlab.com/phpboyscout/go/controls**
>
> Issues and merge requests are handled there.

## Installing

```
go get gitlab.com/phpboyscout/go/controls
```

The module path is the GitLab one. `go get github.com/phpboyscout/controls` will
not work: that path was an older, separate module, and this repository no longer
declares it.

## Documentation

Full documentation: **https://controls.go.phpboyscout.uk**

Written up in [Building a web service in Go](https://phpboyscout.uk/topics/building-a-web-service-in-go/),
which covers graceful shutdown and service lifecycle in practice.

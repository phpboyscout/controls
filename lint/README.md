# singleuse

An advisory analyzer for the capture a restart cannot reuse. A `StartFunc` is
called again on every restart, so anything its closure captured is shared
between runs: a server, a listener or a supervisor built once at wiring is the
previous run's by the second call. This pass names the line where that happens.

```sh
go run gitlab.com/phpboyscout/go/controls/lint/cmd/singleuse@latest ./...
```

It is a nested module of `go/controls` with its own `go.mod`, so `x/tools`
never enters the root module's dependency graph. The rule it enforces is on
[`StartFunc`'s doc comment](https://pkg.go.dev/gitlab.com/phpboyscout/go/controls#StartFunc)
and in [What a restart shares](https://controls.go.phpboyscout.uk/explanation/restart-supervisor/#what-a-restart-shares-and-what-it-must-not).
The guide is [Find a capture a restart cannot reuse](https://controls.go.phpboyscout.uk/how-to/lint-single-use-captures/).

## What it flags

Two rules, each measured against the instances that motivated it (spec 0005,
D3):

- At `controls.WithStart(x)` and `controls.Child{Start: x}`: `x` reaches a
  value of a single-use type declared outside it, whether as a closure or a
  named function that names it, a method value on it (`sup.Start`), a
  `StartFunc` built by a call that takes it, or a `StartFunc` variable followed
  back to the call that built it.
- A method `Start(ctx context.Context) error` that reads a receiver field of
  single-use type, at any depth, that it does not assign.

The single-use types are `grpc.Server`, `http.Server`, `controls.Supervisor`,
`net.Listener`, `net.TCPListener` and `net.UnixListener`, matched as named
types, so a pointer to one or an alias of one counts. The list is the
heuristic; a type is added by merge request when a capture of it is found.

## Advisory

Findings are advice, not a build failure. The command exits 3 on a finding and
1 when it could not run, and the `cicd` component `go-singleuse` tolerates exit
code 3 alone, so a finding is a warning and a broken run is not. A registration
with no restart policy is flagged all the same, because the analyzer cannot see
the policy from the registration site.

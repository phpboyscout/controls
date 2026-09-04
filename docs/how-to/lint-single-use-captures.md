---
title: Find a capture a restart cannot reuse
description: Run the singleuse analyzer, read what a finding means, and fix the capture it names.
---

# Find a capture a restart cannot reuse

A restart calls your `StartFunc` again but reuses its closure, so a server, a
listener or a supervisor built once at wiring is the previous run's by the
second call. `singleuse` is an analyzer that names the line where that
happens. It ships as a nested module of this repository and runs like `go vet`.

## Run it

```sh
go run gitlab.com/phpboyscout/go/controls/lint/cmd/singleuse@latest ./...
```

`@latest` resolves to the tip of `main` until the module carries a
`lint/vX.Y.Z` tag, which it will from the first release cut after colophon
learns to tag nested modules. Pin a version once one exists.

The command exits 3 on a finding, as `go vet` exits non-zero, and 1 when it
could not load the packages at all. It is **advisory** by design: the `cicd`
component that runs it in the estate, `go-singleuse`, tolerates exit code 3
alone, so a finding shows as a warning and a broken run still fails. A job
you write yourself needs the same `allow_failure: exit_codes: [3]`. Advisory
because the analyzer cannot see whether a restart policy is attached, so a
capture that is safe today because nothing restarts it is flagged all the
same.

## Read a finding

Each finding names the value, its type, and how the `StartFunc` reached it:

```text
grpc/server.go:640:31: StartFunc closure uses startServer, built by a call that captures srv (*google.golang.org/grpc.Server); a restart reuses it
http/server.go:571:36: StartFunc is built by a call that captures srv (*net/http.Server); a restart reuses it
tables.go:97:60: Start reads receiver field sup (*gitlab.com/phpboyscout/go/controls.Supervisor) it did not build; a restart reuses it
```

Those three are real: the first two are go/transport's servers, the third is a
supervisor held in a field and started from a `Start` method. The shapes it
recognises:

| Finding says | The shape |
|---|---|
| `closure captures X` | the closure passed to `WithStart` or `Child.Start` names a single-use value from outside, including a package-level one |
| `closure reaches a.b through captured a` | it reaches one through a struct it captured, by field, by an embedded field's promoted method, or by value |
| `is the Start method of X` | a method value, `WithStart(sup.Start)`, on a single-use value built at wiring |
| `f reaches X` | a named function handed to `WithStart` uses a package-level single-use value |
| `is built by a call that captures X` | the `StartFunc` came from a call whose argument is the value |
| `uses f, built by a call that captures X` | the `StartFunc` variable, declared with `:=`, `var` or a later `=`, was built by such a call |
| `Start reads receiver field X it did not build` | a `Start(ctx) error` method uses a field, at any depth, assigned elsewhere, usually the constructor |

An alias of a listed type, and a pointer to one, count as the type.

The single-use types are `*grpc.Server`, `*http.Server`, `*controls.Supervisor`,
`net.Listener` and `*net.TCPListener`.

## Fix it

Build the thing inside the run. The second run then gets a second one:

```go
controls.WithStart(func(ctx context.Context) error {
	srv := grpc.NewServer()        // per run, not at wiring
	registerServices(srv)

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	return srv.Serve(lis)
})
```

When the value has to outlive the `StartFunc`, because a `Stop` or a `Status`
probe needs it too, wrap it in a [`Generational`](survive-a-restart.md), which
builds one per generation and refuses access to a stopped one. That is what
go/messaging did for its supervisor, and the analyzer is silent on it because
the supervisor is built by `Build`, not read from a field a constructor filled.

For a `Start` method reading a field, either assign the field inside `Start`
(the analyzer treats a field the method assigns as built per run, and the same
holds for a variable a closure assigns) or move the construction into a
`Generational` `Build`. For `WithStart(sup.Start)` on a supervisor, wrap the
supervisor in a `Generational` if the service can be restarted; if it never
is, the finding is the advisory kind and says so.

## What it cannot see

- A single-use value captured through an interface type of your own. The list
  matches named types, so a `Server` interface wrapping `*http.Server` is
  invisible.
- A `StartFunc` built two helper calls deep, or assigned from a multi-value
  call (`start, err := build(srv)`). One level and one value are followed.
- A named function or `StartFunc` variable defined in another package.
- A `Start` method whose field is rebuilt by a helper it calls rather than by
  an assignment in its own body, or a closure that rebuilds the whole struct it
  captured. Those are the first false positives to expect.

The rule the analyzer enforces, and the two failures it prevents, are in
[What a restart shares](../explanation/restart-supervisor.md#what-a-restart-shares-and-what-it-must-not).
A type missing from its list is a merge request to this repository, with the
capture that motivated it in the commit message.

## Related

- [Survive a restart](survive-a-restart.md): the recipe for a value that has
  to outlive the `StartFunc`.
- [The restart supervisor](../explanation/restart-supervisor.md): why a
  restart shares the closure at all.

# AGENTS.md

This file provides guidance to AI coding agents (Claude Code, agy, codex, etc.) when working with code in this repository.

Ways of working are deliberately not repeated here. They live in the phpboyscout
skills, and naming a skill tends to age better than restating it, since the
restatement goes stale while the skill does not.

## What this is

`gitlab.com/phpboyscout/go/controls` is a module in the
[phpboyscout Go toolkit](https://go.phpboyscout.uk), and it is a
**service-lifecycle supervisor**. Register long-running services with a
`Controller` (HTTP and gRPC servers, background workers, schedulers) and it starts
them concurrently, aggregates the health probes they supply, and drives a bounded
graceful shutdown in reverse registration order, with an optional restart policy.

The name is worth pinning down, because "controls" invites the wrong guess. Not
UI controls. Not access control: request authentication is `go/authn` and
credentials are `go/credentials`. Not flow control or rate limiting either, which
are `go/transit`. It means process control in the init-system sense: what starts,
what stops, in what order, and what gets restarted when it dies, all inside a
single process.

Two boundaries it holds on purpose. It **serves no health endpoints**: `Status()`,
`Liveness()` and `Readiness()` return plain `HealthReport` values and the module
opens no port, which is `go/transport`'s job. And it **does not order or gate
startup**: services launch concurrently, there is no `DependsOn` and no readiness
barrier, only shutdown is ordered. Read [What controls does not
do](https://controls.go.phpboyscout.uk/explanation/limitations/) before planning
around a capability that turns out not to be there. Framework-free is enforced
rather than aspirational: `depfootprint_test.go` fails the build if go-tool-base,
cobra/viper/charm, OpenTelemetry or a cloud SDK reaches the graph, and the one
external dependency is `go/errors`.

## Who depends on it

Five projects require it directly and import it: `go/transport` (whose `gateway`
package takes a `controls.Controllable`), `go-tool-base`, `keryx`, `krites` and
`phpbotscout`. Five more carry it only as an `// indirect` line and never import
it: `go/transport-metrics` and `go/transport-openapi` through `go/transport`, and
`sigillum`, `scoutdm` and `skillup` through `go-tool-base`. Check which side a
project is on before assuming a change reaches its code. So this is a toolkit
dependency, not a leaf: no `-provider` siblings, but an exported symbol is a
compile-time contract for everything above, which makes widening or repointing one
a release-train question.

## Where it has got to

Pre-1.0 at `v0.3.2`, with `Supervisor` on `feat/supervisor` (spec 0002) and not
yet released. Before it, no Go source had changed since `v0.3.0` adopted
`go/errors` in early August; behaviour last moved at `v0.2.0` (single-owner
signal handling) and `v0.1.4` (a run of concurrency fixes, each with its own
regression test file). So `Controller` is settled and `Supervisor` is new, which
is the split to keep in mind before treating any part of the surface as proven.
The docs are a full Diátaxis set at
[controls.go.phpboyscout.uk](https://controls.go.phpboyscout.uk), but issue #3
tracks a prose pass over them and the README: parts of both still describe an
older dependency footprint, so check before repeating either.

## Traps

**The controller does not inherit the caller's context, and that is the
contract.** `NewController` wraps the parent in `context.WithoutCancel` and owns
its own cancellation, so `context.Cause` is `controls.ErrShutdown` for every stop
it drives, whatever triggered it. Deriving `c.ctx` from the parent is the
obvious-looking simplification and it silently voids that guarantee, since the
parent's own cause wins the race. The parent is still watched: its completion, by
cancel or deadline, triggers a graceful `Stop`. Wiki spec
[0001](https://gitlab.com/phpboyscout/go/controls/-/wikis/specs/0001-single-owner-signal-handling)
and `signal_ownership_test.go` hold it in place.

**The `D` and `F` numbers in the comments are real decision IDs, and most are not
defined in this repo.** `D8` to `D12` are in `docs/explanation/concurrency.md`,
`D1` to `D3` in wiki spec 0001, and `D4` to `D7`, `F5` and `F6a` only in the merge
requests that introduced them. Cite an existing one when extending its decision;
do not mint the next number for your own change.

**The Release MR carries `squash: true` on purpose, on a fast-forward project.**
This repo is `merge_method: ff` with squash off by default, and a merge producing
neither a merge commit nor a squash commit sends releaser-pleaser to a fallback
that tags the MR's recorded head, which GitLab's rebase leaves stale, dropping
whatever landed while the MR was open. Turning that flag off looks like tidying an
inconsistency. It is not: [phpboyscout/cicd#7](https://gitlab.com/phpboyscout/cicd/-/issues/7).

## The quality gate

`just ci` runs the repo's own checks (tidy, test, race, lint). Run it before
raising a merge request, so CI confirms rather than discovers. The suite times
things against the wall clock rather than a fake one, so re-run anything that
touches shutdown or restart timing. `just mocks` and `just test-e2e` are inherited
template recipes with nothing here to run: no mockery config, no `test/e2e/` tree,
no published mocks, and `enable_e2e: false` in CI.

**Test the wrong-order pairs before the happy path.** Both types here have a
lifecycle, and the tests that get written walk it forwards: register, start, let
something fail, stop. The supervisor's pre-merge review found five defects that
needed no concurrency at all, only a call in an order nothing had tried. `Stop`
before `Start` hung forever on a `<-c.done` no goroutine would ever close;
`Detach` before `Start` burned the caller's whole budget and then reported
`ErrDetachTimeout` for a child that never ran; `Attach` after `Stop` was accepted
and ran the child against a dead context; `Readiness` reported ready after a
completed `Stop`. So for any `Start`/`Stop`, `Attach`/`Detach` or `Open`/`Close`
pair, write the wrong-order case first. Each has a right answer, usually "refuse
and say which rule was broken", and writing them first is what forces the
terminal state to be designed rather than inherited from whichever flag was left
set.

**`-race` finds only what a test actually performs.** It is in `just ci` and it
was green on the supervisor branch while two genuine races sat in `Stop`, one of
them on `c.cancel` between `launch` and `stopChild`. The detector was working;
no test had ever called `Start` and `Stop` concurrently, so the conflicting
accesses never happened in the same run. A hundred-iteration loop doing exactly
that flagged it in 0.07s. Anything whose methods may be called concurrently
needs a test that calls them that way before a green `-race` means anything.

**Mutate the changed code before raising the MR.** The supervisor branch reached
96.7% coverage, and fourteen separate edits to `supervisor_run.go` left the full
suite green: deleting the entire backoff `select`, deleting the clean-exit
branch, inverting `exhausted`'s nil-policy rule, dropping the recover around the
failure callback. Coverage counts lines that ran, not behaviour anything holds.
Break the changed code on purpose, one edit at a time, re-run, and treat
anything still green as untested. See **test-first-discipline**.

## The lifecycle state is load-bearing for health

`Readiness()` is true **only while `Running`** (spec 0003 D2), stated positively so a state added
later is unready by default. `Liveness()` is deliberately not gated the same way: a liveness failure
during a graceful shutdown invites the orchestrator to kill a correct drain. `go/transport` maps
`OverallHealthy` straight onto HTTP 200/503 and gRPC SERVING/NOT_SERVING, so a change to the verdict
is a change to what every service in the estate puts on the wire without any of them changing a
line.

**`Unknown` is not "constructed but not started".** That is `NeverStarted`, and it is what the
registration guards check. `Unknown` means the state could not be determined, which `GetState`
returns for a `Controller` built without `NewController` — `State` is a string type, so such a
controller holds `""`, otherwise undetectable.

**`UnableToStart` needs both halves.** A registered service that has never started cleanly *and* has
exhausted its restart policy. One without the other is an ordinary failure: a service that fails at
boot and recovers on a restart is what restart policies are for, and tripping on the first error
would flap readiness through a slow boot. `markStarted` in `services.go` cannot be used for the
"started cleanly" half, whatever its name suggests: it is a `wg.Done` guard fired by a `defer` on
every exit, including a failing one.

**`Stop` must transition from `UnableToStart` as well as `Running`** (`beginShutdown`). Forgetting it
is quiet and bad: `Stop` refuses, and the services that *are* running are never told to stop.

**`SetState` is public on purpose, and the docs said otherwise three times.** `StateAccessor` is
meant to be consumed and implemented outside this module, so a consumer driving a controller it owns
needs the setter. Its own doc comment said "read access", `docs/reference/interfaces.md` routed
readers to it for reading, and `docs/reference/controller.md` said it existed "for fakes". A review
proposed removing it on exactly that reading. Spec 0003 D7 records why it stays.

## A regression guard nobody has seen fail is indistinguishable from one that cannot

Three of this repo's guards passed against the defect they were written for. Each was found by
reinstating the bug rather than by reading the test, which is the only check that settles it.

- **`TestSignalsNotSwallowedBeforeStart`** re-executed the test binary as a child described as
  having "signals enabled" and never passed `WithSignals`. Signals are opt-in, so `signal.Notify`
  was never called by any path and SIGINT kept its default disposition whatever the constructor
  did. It passed identically with F5 reinstated (issue 5).
- **`TestErrorHandler_NoBusySpinAfterStop`** measured goroutine counts, which catch a handler that
  never RETURNS. The D4 busy-spin is a handler that loops on a permanently-ready `ctx.Done()` case
  and then exits perfectly well. Reinstating it left the test green: named for a defect it could
  not see (issue 8). A spin is CPU, so the test for it measures CPU, with a slow `WithStop` holding
  shutdown open wide enough for one to accumulate.
- **`TestSupervisorAndServiceRestartAlike`** compared only invocation counts, so deleting the whole
  backoff `select` left it green.

**`runtime.NumGoroutine` must never be asserted from a `t.Parallel()` test.** It counts the
process, so in parallel it cannot tell a leak in the controller under test from another test being
busy, and symmetrically a real leak hides behind another test finishing. Four tests here use a
process-global measure and all four are sequential, each saying why: a Go test that does not call
`t.Parallel()` runs while every parallel test in the package is paused. Adding `t.Parallel()` back
to match the tests around them is the obvious tidy-up and silently breaks them.

## Which skills apply here

| When | Skill |
|---|---|
| Considering a new dependency (`depfootprint_test.go` holds the veto, and the usual answer is none at all) | `use-the-go-toolkit` |
| Adding to or widening the public surface, given the deliberately narrow role interfaces and the modules compiled against them | `deep-modules`, `release-train` |
| Chasing a shutdown, restart or health-timing bug, or a test that only fails sometimes | `diagnose-with-a-red-loop` |
| Writing tests for anything with a lifecycle, or before raising an MR on a change with new tests | `test-first-discipline` |
| Adding a test that fakes a service's start, stop or probe (they go through the functional options, never a package-level var) | `race-safe-test-injection` |
| Writing a doc comment or a docs page here, where the prose is checked against the tests | `checkable-claims`, `diataxis-docs` |
| Before `glab mr create` on this repo | `verify-before-pr` |
| Writing a commit message or a merge request description | `conventional-commits`, `pre-1-0-release-safety` |
| Committing, branching, merging, or opening a merge request | `forge-publish-workflow` |
| Anything touching the Release MR | `releaser-pleaser-releases` |

> Skills are a Claude Code mechanism, shipped by the
> [phpboyscout marketplace](https://gitlab.com/phpboyscout/claude-code-plugins).
> An agent without them should treat a named skill as a topic to ask about
> rather than a file it can load.

## House rules

- Linear history. Rebase and fast-forward; never squash-merge from the UI. The
  Release MR is the documented exception, above.
- Conventional Commits, and the type decides whether a release is cut. Only
  `feat` and `fix` release, and `fix(deps)` counts. A change that repoints or
  removes a public interface is `feat`, not `refactor`, or it never ships.
- No AI attribution in anything published, and never at-mention anyone.
- Never cut a release yourself. That is the maintainer's call, every time.

## Controller and Supervisor

`Controller` manages a **fixed** set registered before `Start`, stopped in reverse registration
order. `Supervisor` manages a set that **changes**, stopped concurrently. Spec 0002.

**Registered means required; attached does not.** A registered service whose probe fails sets
`OverallHealthy: false` for the whole process (`services.go`, `readiness`). `Supervisor.Readiness`
must therefore **never** fail because a child failed — a Supervisor is registered with a Controller,
so one dead child would otherwise take the process out of rotation through that registration. A
failed child is reported as `CheckDegraded` and delivered to the consumer as a `Failure`.

**`RestartPolicy` is shared, and its rules are shared for real.** `restartsExhausted` and
`classifyOutcome` in `services.go` have two callers each: a `Service`'s restart loop and a `Child`'s.
`MaxRestarts <= 0` means **unlimited** for both, because it is one line of code rather than two
that agree. The first draft of the supervisor read it as *never* — same field, opposite meaning,
both visible from one screen. Two fields stay inert for a `Child`: `HealthFailureThreshold` and
`HealthCheckInterval` need a `Status` probe, and a `Child` has none. That is documented on the
field rather than left to be discovered.

**A Supervisor is a state machine, and every call has an out-of-order answer.** `supNew`,
`supRunning`, `supStopping`, `supStopped`, one direction only. `Attach` and `Start` return
`ErrSupervisorStopped` once shutdown begins, because accepting a child that will never be
supervised is worse than refusing it. `Stop` before `Start` returns at once rather than waiting on
a done channel no goroutine will close. `Stop` and `Detach` are bounded by their context across
`Child.Stop` as well as the child's own goroutine.

**A child's `Start` is recovered; a service's is not.** Deliberate asymmetry: a child is
caller-supplied code the supervisor launched, so the same line `callStop` and `callProbe` already
draw applies. A panic is still a defect — counted separately from an error return.

**Failure notification runs on its own goroutine, and nothing joins it.** Not for throughput: it
keeps failures ordered and means a callback calling back into the supervisor cannot deadlock. That
includes `Stop` — an earlier version waited for the dispatch goroutine during shutdown, so a
callback calling `Stop` waited for itself. The queue behind the callback is unbounded where the
`Failures()` channel is bounded, because the channel is opt-in and the callback is not: a consumer
that registered one did not agree to lose notifications.

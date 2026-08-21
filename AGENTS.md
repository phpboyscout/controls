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

Pre-1.0 at `v0.3.1`, with a deps-only Release MR open for `v0.3.2`. No Go source
has changed since `v0.3.0` adopted `go/errors` in early August; behaviour last
moved at `v0.2.0` (single-owner signal handling) and `v0.1.4` (a run of
concurrency fixes, each with its own regression test file). Everything since has
been Renovate and docs, so treat the public surface as settled. Those docs are a
full Diátaxis set at [controls.go.phpboyscout.uk](https://controls.go.phpboyscout.uk),
but issue #3 tracks a prose pass over them and the README: parts of both still
describe an older dependency footprint, so check before repeating either.

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

## Which skills apply here

| When | Skill |
|---|---|
| Considering a new dependency (`depfootprint_test.go` holds the veto, and the usual answer is none at all) | `use-the-go-toolkit` |
| Adding to or widening the public surface, given the deliberately narrow role interfaces and the modules compiled against them | `deep-modules`, `release-train` |
| Chasing a shutdown, restart or health-timing bug, or a test that only fails sometimes | `diagnose-with-a-red-loop` |
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

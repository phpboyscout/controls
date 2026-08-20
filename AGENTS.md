# AGENTS.md

This file provides guidance to AI coding agents (Claude Code, agy, codex, etc.) when working with code in this repository.

**This file is a seed.** It carries only what could be derived from the
repository itself. What is specific to this module, what it is for, where it has
got to, and the traps it sets, is not here yet. Issue #4 tracks filling
that in.

Ways of working are deliberately not repeated here. They live in the phpboyscout
skills, and naming a skill tends to age better than restating it, since the
restatement goes stale while the skill does not.

## What this is

`gitlab.com/phpboyscout/go/controls` is a module in the
[phpboyscout Go toolkit](https://go.phpboyscout.uk).

It is a standalone module: no `-provider` siblings depend on a shape it owns,
and it carries its own documentation.

## The quality gate

`just ci` runs the repo's own checks. Run it before raising a merge
request, so CI confirms rather than discovers.

## Which skills apply here

| When | Skill |
|---|---|
| Reaching for a dependency for config, HTTP, CLI, logging, forge or credentials | `use-the-go-toolkit` |
| Checking this module against the estate's shape | `create-a-go-module` |
| Before `glab mr create` on this repo | `verify-before-pr` |
| Faking exec, `time.Now`, network or filesystem in a test | `race-safe-test-injection` |
| Adding a test that hits the network, a real API, or real git or disk | `env-gated-integration-tests` |
| Writing or restructuring this module's documentation | `diataxis-docs` |
| Writing a commit message or a merge request description | `conventional-commits`, `pre-1-0-release-safety` |
| Committing, branching, merging, or opening a merge request | `forge-publish-workflow` |

> Skills are a Claude Code mechanism, shipped by the
> [phpboyscout marketplace](https://gitlab.com/phpboyscout/claude-code-plugins).
> An agent without them should treat a named skill as a topic to ask about
> rather than a file it can load.

## House rules

- Linear history. Rebase and fast-forward; never squash-merge from the UI.
- Conventional Commits, and the type decides whether a release is cut. Only
  `feat` and `fix` release. A change that repoints or removes a public interface
  is `feat`, not `refactor`, or it lands and never ships.
- No AI attribution in anything published, and never at-mention anyone.
- Never cut a release yourself. That is the maintainer's call, every time.


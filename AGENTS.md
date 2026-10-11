# AGENTS.md

Repo-specific notes for coding agents working on agent-fleet.

## Language

Everything in this repo is English: code, comments, docs, commit messages,
PR text.

## Layout

- `cmd/fleet/`: the `fleet` binary. `main` dispatches on the first argument;
  each subcommand gets its own `flag.FlagSet` (standard library only).
- `internal/`: all other packages go here. This repo offers no Go API.
- `tools/`: one module file per pinned tool (`golangci-lint`, `govulncheck`),
  run with `go tool -modfile=tools/<tool>.mod <tool>`.
- `scripts/`: the check script and its helpers.
- `judge/`: the black-box end-to-end suite (bash, in docker with a real
  herdr and a fake `claude`). It observes only the public surface, so it
  runs unchanged against any implementation of the CLI; see `judge/run.sh`
  for the parameters.
- `docs/design.md`: the one design doc. Keep it under its 200-line cap; a PR
  that changes behavior updates the section or invariant it touches.

## Toolchain

`go.mod` pins the Go version with a single `go` line and no `toolchain` line.
Checks run with `GOTOOLCHAIN=local`, so a mismatched local Go fails instead of
downloading another one. Bump the installed Go before bumping `go.mod`.

## Checks

Before a push, run the pre-commit hooks and those of the checks below that
the change touches. The full set runs in CI; on a PR, the run of the required
check is what counts.

- `scripts/check.sh`: gofmt, `go vet`, golangci-lint, `go test`, govulncheck,
  a static build, the design doc's line cap, and that the judge's CI shards
  run every suite once. Run it before pushing a change outside the docs; for
  `docs/design.md`, `scripts/check-design-cap.sh` is enough.
  On a shared machine, set `FLEET_CHECK_CORES=N` to limit it to N cores.
- `go test -race ./...`: CI runs it at default parallelism. A limited local
  run is not a substitute.
- `pre-commit run --all-files`: gitleaks on the staged diff, then gofmt.
- `judge/run.sh <binary>`: the end-to-end suite. Needs docker; takes a few
  minutes. Green against the Go binary. `JUDGE_SUITES="<suite> ..."` runs
  only those suites.

## CI

`.github/workflows/ci.yml` runs on pull requests and on pushes to `main`. Its
jobs run in parallel, except that `race` and `judge` wait for `changes`, which
decides from the changed paths whether they have to run (the path groups are
in `ci.yml`); a change only to the docs runs neither.

- `lint`, on every change: gitleaks on the commits of the PR or push,
  pre-commit (gitleaks hook skipped, since the scan before it covers the same
  commits), and `scripts/check.sh`. `.github/workflows/gitleaks.yml` scans the
  whole history once a week.
- `race`: `go test -race ./...`.
- `judge`: the judge against a fresh Go build, a matrix of three shards set
  by `JUDGE_SUITES`: lifecycle, thread, and the other suites together.
  Each shard prints its own `judge:` line. The judge image is cached, keyed
  on `judge/Dockerfile` and `judge/bin/`.

`check` needs every job above and passes only when each of them succeeded, or
was skipped because `changes` said it need not run; it is the required status
check on `main`, so no other job may be named `check`.
Each judge suite lays out what it needs, so any suite runs alone; a new
suite does the same and gets a place in a shard (`scripts/check.sh` fails
until it has one). Runners are pinned to `ubuntu-24.04`, and third-party
actions by commit SHA.

## Test data

Use only made-up values: `example.test` domains, documentation IP ranges
(192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24, 2001:db8::/32). Tests use
`t.TempDir()` and never read the real home directory or the network.

## Fleet

For leads and workers that `fleet` starts in this repo.

- Open your own worktree with `fleet worktree agent-fleet --branch <type>/<short-desc>`, or work in the one your lead names. A reviewer's checkout of a PR head is `fleet worktree agent-fleet --name <name> --detach <head-sha>`, never a bare `git worktree add`: fleet removes what it recorded at `fleet job end`.
- Worker report (`fleet done --report-file`): what was done, the PR and its merge commit, the judge lines (`judge: N ok, M failed`, one per shard; none when CI skipped the judge) and the checks run, why done or abandoned, and follow-ups left.
- Done means the PR is merged with the required check green. Abandoned means you stop without a merge and say why. The lead reads the reports and decides what comes next.
- This repo has no natural key for a job. Before starting one, look at `fleet job list` and the open issues, and ask in the thread when unsure.

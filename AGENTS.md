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

- `scripts/check.sh`: gofmt, `go vet`, golangci-lint, `go test`, govulncheck,
  a static build, and the design doc's line cap. Must pass before every push.
  On a shared machine, set `FLEET_CHECK_CORES=N` to limit it to N cores.
- `go test -race ./...`: CI runs it at default parallelism. A limited local
  run is not a substitute.
- `pre-commit run --all-files`: gitleaks on the staged diff, then gofmt.
- `judge/run.sh <binary>`: the end-to-end suite. Needs docker; takes a few
  minutes. Green against the Go binary. `JUDGE_SUITES="<suite> ..."` runs
  only those suites.

## CI

`.github/workflows/ci.yml` runs on pull requests and on pushes to `main`. These
jobs run in parallel:

- `lint`: a gitleaks scan of the full history, pre-commit (gitleaks hook
  skipped, since the full scan covers it), and `scripts/check.sh`.
- `race`: `go test -race ./...`.
- `judge`: the judge against a fresh Go build, a matrix of three shards set
  by `JUDGE_SUITES`: lifecycle, thread, and the other four suites together.
  Each shard prints its own `judge:` line. The judge image is cached, keyed
  on `judge/Dockerfile` and `judge/bin/`.

`check` needs all of them and passes only when each of them succeeded; it is
the required status check on `main`, so no other job may be named `check`.
Each judge suite lays out what it needs, so any suite runs alone; a new
suite does the same and gets a place in a shard. Runners are pinned to
`ubuntu-24.04`, and third-party actions by commit SHA.

## Test data

Use only made-up values: `example.test` domains, documentation IP ranges
(192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24, 2001:db8::/32). Tests use
`t.TempDir()` and never read the real home directory or the network.

## Fleet

For leads and workers that `fleet` starts in this repo.

- Open your own worktree with `fleet worktree agent-fleet --branch <type>/<short-desc>`, or work in the one your lead names. A reviewer's checkout of a PR head is `fleet worktree agent-fleet --name <name> --detach <head-sha>`, never a bare `git worktree add`: fleet removes what it recorded at `fleet job end`.
- Worker report (`fleet done --report-file`): what was done, the PR and its merge commit, the judge line (`judge: N ok, M failed`) and the checks run, why done or abandoned, and follow-ups left.
- Done means the PR is merged with the required check green. Abandoned means you stop without a merge and say why. The lead reads the reports and decides what comes next.
- This repo has no natural key for a job. Before starting one, look at `fleet job list` and the open issues, and ask in the thread when unsure.

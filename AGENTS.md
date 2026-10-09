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

## CI

`.github/workflows/ci.yml` has one job, `check`, which is the required status
check on `main`. It runs on pull requests and on pushes to `main`: a gitleaks
scan of the full history, pre-commit (gitleaks hook skipped, since the full
scan covers it), `scripts/check.sh`, and `go test -race ./...`. Third-party
actions are pinned by commit SHA.

## Test data

Use only made-up values: `example.test` domains, documentation IP ranges
(192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24, 2001:db8::/32). Tests use
`t.TempDir()` and never read the real home directory or the network.

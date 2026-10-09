# agent-fleet

`fleet` is a command-line tool that runs and coordinates coding agents. It
starts agent sessions and jobs in [herdr](https://github.com/herdrdev/herdr),
passes messages between agents, keeps a local SQLite ledger of what happened,
and spots agents that look stuck.

**Status:** being ported from an earlier tool; not usable yet.

## Build

Requires the Go version named in `go.mod`.

```sh
go build -o fleet ./cmd/fleet
./fleet --version
```

## Check

```sh
scripts/check.sh
```

This runs gofmt, `go vet`, golangci-lint, the tests, govulncheck, a static
build, and the design doc's line cap. The linters are pinned in `tools/` and
run through `go tool`, so nothing needs installing beyond Go. Set
`FLEET_CHECK_CORES=N` to limit the run to N cores.

The design is in [docs/design.md](docs/design.md).

## License

Apache-2.0. See [LICENSE](LICENSE).

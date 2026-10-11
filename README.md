# agent-fleet

`fleet` is a command-line tool that runs and coordinates coding agents. It
starts jobs (a lead and its workers) in [herdr](https://github.com/herdrdev/herdr),
passes messages between agents, keeps a local SQLite ledger of what happened,
and spots agents that look stuck.

**Status:** under construction; the command surface is still changing.

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

## Local conventions

fleet assumes a layout for each machine: the repos' main checkouts under
`~/dev`, the checkouts of cross-repo initiatives under `~/x-repo`, worktrees
under `~/wt`, agents' files under `~/scratch`, and Slack channels named
`repo-<repo>` and `x-repo-<initiative>`, with direct messages going to the
initiative `general`. These are only defaults: set `paths` and `channels` in
the scope's settings file, `$XDG_CONFIG_HOME/fleet/<scope>.json`
(`~/.config` when unset). The keys are listed
in [docs/design.md](docs/design.md).

## Changing the prompts

fleet starts each agent with a built-in prompt. To add to one without a new
release of fleet, name a Markdown file for it in the scope's settings file:

```json
{"prompts": {"worker": {"append": "prompts/worker.append.md"}}}
```

The path is relative to the settings file, so this one is
`~/.config/fleet/prompts/worker.append.md`; its text follows the built-in
worker prompt for every worker started from then on. After each change, run
`fleet prompt show`: it lists each prompt's parts and every file that would
stop an agent from starting (`fleet prompt show worker` prints the whole
text). Run it again after upgrading fleet, to see whether a prompt you
replaced is behind the new built-in one. To replace one, start from a copy, in `~/.config/fleet`:
`fleet prompt show lead --builtin > prompts/lead.base.md`, copy that to
`prompts/lead.md`, edit it, and set `"lead": {"replace": "prompts/lead.md",
"base": "prompts/lead.base.md"}`. The model settings and the rest are in
[docs/design.md](docs/design.md).

The design is in [docs/design.md](docs/design.md).

## License

Apache-2.0. See [LICENSE](LICENSE).

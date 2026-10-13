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

fleet starts each thread agent, lead and worker with the built-in prompt of
its role, followed by `common`, the text the three roles share. Each of
`common`, `thread`, `lead` and `worker` can be changed on its own; a change
to `common` reaches all three roles. To add to one without a new release of
fleet, name a Markdown file for it in the scope's settings file:

```json
{"prompts": {"worker": {"append": "prompts/worker.append.md"}}}
```

The path is relative to the settings file, so this one is
`~/.config/fleet/prompts/worker.append.md`; its text follows the built-in
worker prompt and `common` for every worker started from then on. After each
change, run `fleet prompt show`: it lists each prompt's parts, where each
watch's prompt (below) comes from, and every file that would stop an agent
from starting or a watch from running (`fleet prompt show worker` prints the
whole text, `fleet prompt show watch:<name>` a watch's prompt). Run it again
after upgrading fleet, to see whether a prompt you replaced is behind the new
built-in one.

To replace a prompt, start from a copy of the built-in one:

```sh
mkdir -p ~/.config/fleet/prompts && cd ~/.config/fleet
fleet prompt show lead --builtin > prompts/lead.base.md
cp prompts/lead.base.md prompts/lead.md   # then edit prompts/lead.md
```

and add it under `"prompts"`: `"lead": {"replace": "prompts/lead.md", "base":
"prompts/lead.base.md"}`. The model settings and the rest are in
[docs/design.md](docs/design.md).

## Watches

`fleet watch`, run from a timer, checks on the scope's agents and acts
through its watches. Three are there by default: `stale-workers` tells a lead
when its workers look stuck, `quiet-threads` asks a thread's agent about a
thread gone quiet, and `unblock` starts a worker for an agent stopped at a
screen. Each can be given its own `prompt`, an `every`, or turned `off`.

Add your own under `watches.<name>`. Its source is a command that prints one
JSON line per subject: a `key` of its own, a `text`, and the `agent`, `job`
or `thread` it concerns. With `do: tell`, each new or changed subject's text
is sent, followed by the watch's prompt, to that agent (a job's lead, a
thread's agent); with `do: worker`, a worker is started for it. A
`do: worker` watch without a source is a timer: each time it runs (see
`every`), it starts a worker for itself, unless the last one is still live.

```json
{"watches": {"open-prs": {"source": {"command": ["/usr/local/bin/open-prs"]},
  "do": "tell", "every": "30m", "prompt": "watches/open-prs.md"}}}
```

Here `open-prs` might print
`{"key": "pr-12", "text": "PR 12 has waited two days for review.", "job": "fix-login"}`,
and the prompt file can use `{{key}}` and `{{text}}`. The other fields
(`per_run`, `again`, `max`, `env`, `model`, `effort`) and what the command
reads on stdin are in [docs/design.md](docs/design.md).

The design is in [docs/design.md](docs/design.md).

## License

Apache-2.0. See [LICENSE](LICENSE).

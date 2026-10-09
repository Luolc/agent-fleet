# fleet design

## What this is and is not

A CLI that runs and coordinates coding agents in herdr; not an agent itself, not a service, not a Go library.

## Parts and how they connect

Ported (the port follows `porting.md`):

- `cmd/fleet`: parses the command line (standard `flag`, one `FlagSet` per command, `--session` global) and dispatches to `internal/cmd`; turns a failure into `fleet: <message>` on stderr and the exit code.
- `internal/cmd`: one file per command. `send` reads the body, adds the `[FROM: <agent>]` header and delivers it through `herdr.Prompt`; `status` joins the ledger's live rows with `herdr agent list`; `spawn` runs the checks, writes the `starting` row, makes the worktree (lead) and the workspace or tab, starts Claude Code through `herdr agent start`, gets it past the folder-trust prompt to its input box, delivers the task and marks the row `active`. Its helpers (`Home`, `RepoName`, `Git`, `WorkspacesLabelled`, `HerdrAgentList`) are shared with `close`. `done` delivers the report through `send`'s path to `FLEET_PARENT` and ends the caller's row only when herdr reports it delivered. `close` (orchestra only, refused while the job has live rows unless `--force`) closes the job's workspace, refuses while an agent's cwd is inside the worktree, removes the worktree and deletes its branch, and ends the job's rows only when nothing is left. `watch` (for cron) reads each live lead and worker's herdr status, `state_change_seq` and a hash of its screen with Claude Code's spinner, input box and footer stripped, records them in the ledger, counts an agent unchanged for the stale limit (or gone from herdr) as a suspect, and tells the orchestra as `cron` through `send`'s path only when the set of suspects changes.
- `internal/herdr`: runs the `herdr` CLI with a bounded environment and a deadline, parses its one-object JSON reply, and maps `agent prompt` outcomes to the exit codes.
- `internal/db`: the ledger (`modernc.org/sqlite`, WAL, 5 s busy timeout, immediate transactions) and its schema migration.
- `internal/identity`: the caller's identity from the `FLEET_*` variables.
- `internal/exit`: the exit-code contract and the failure type every command returns.
- `internal/cliargs`: the clap behaviors `flag` lacks (flags after positionals, a string flag that records whether it was given).

## Invariants

Each invariant links to the test that guards it, or is marked untested.

- The CLI behaves as the judge (`judge/`) says: same argv, exit codes, stdout/stderr, ledger rows, herdr state and screens as the earlier implementation. Guarded by `judge/run.sh`; every command's cases are in `judge/usage.sh`, `send.sh`, `lifecycle.sh` and `watch.sh`.

## Interfaces

What the judge pins down (the CLI reference and `--json` output are added as they are ported):

- Commands `spawn`, `send`, `done`, `status`, `watch`, `close`, with `--session <name>` global and `--help` on each; `--version`.
- Exit codes, shared by every command: 0 ok; 1 usage error or precondition refused; 2 no clear signal from herdr (timeout, stalled); 3 target blocked or spawn stopped at an unknown screen; 4 target not found; 5 environment error (herdr, git, database).
- Identity from `FLEET_AGENT`, `FLEET_ROLE`, `FLEET_PARENT`, `FLEET_REPO`, `FLEET_JOB`, injected into every pane `spawn` creates; `FLEET_WATCH_STALE_SECS` overrides watch's 30-minute limit.
- The ledger at `~/scratch/<repo name>/fleet.db`, SQLite in WAL mode, table `agents` with the columns the judge reads: name, role, job, parent, report_to, task, worktree, pane_id, state (`starting`, `active`, `ended`), started_at, ended_at, last_status, last_seq, last_screen_hash, last_change_at, suspect.
- Messages carry a first line `[FROM: <agent>]`; watch sends as `cron`; a lead's worktree is `~/wt/<repo name>/<job>` on branch `data/<job>` from origin/HEAD of `~/dev/<repo name>`.

## Known issues and next steps

Every command is ported; the judge is green against the Go binary and is a blocking CI check. `watch`'s screen filter knows only Claude Code's screen, and its judge arm runs with a 5 s stale limit, so it is sensitive to a loaded machine.

## Decision log


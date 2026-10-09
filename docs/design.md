# fleet design

## What this is and is not

A CLI that runs and coordinates coding agents in herdr; not an agent itself, not a service, not a Go library.

## Parts and how they connect

Only `cmd/fleet` exists so far; parts are added here as they are ported.

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

The judge is red against the Go binary, which only has `--version`. Next: port the commands one by one until it is green, then make the judge a blocking CI check (one line in `.github/workflows/ci.yml`).

## Decision log


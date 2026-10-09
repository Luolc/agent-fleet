# fleet design

## What this is and is not

A CLI that runs and coordinates coding agents in herdr; not an agent itself, not a service, not a Go library.

## Parts and how they connect

Ported (the port follows `porting.md`):

- `cmd/fleet`: parses the command line (standard `flag`, one `FlagSet` per command, `--session` global) and dispatches to `internal/cmd`; turns a failure into `fleet: <message>` on stderr and the exit code.
- `internal/cmd`: one file per command. `send` reads the body, adds the `[FROM: <agent>]` header and delivers it through `herdr.Prompt`; `status` joins the ledger's live rows with `herdr agent list`; `spawn` runs the checks (settings from `internal/config`), creates and claims the agent's work order when the repo uses Linear, writes the `starting` row, makes the worktree (lead) and the workspace or tab, starts Claude Code through `herdr agent start`, gets it past the folder-trust prompt to its input box, delivers the task and marks the row `active`. Its helpers (`Home`, `RepoName`, `Git`, `WorkspacesLabelled`, `HerdrAgentList`) are shared with `close`. `done` delivers the report through `send`'s path to `FLEET_PARENT` and ends the caller's row only when herdr reports it delivered; with a worker report it first writes the report to the Linear issue and releases the issue through `internal/atb`. `worktree` opens `~/wt/<repo>/<job>[-<name>]` for the caller's `FLEET_JOB` with `spawn`'s `originHead` and records it in the ledger's `worktrees` table; a path that exists is returned unchanged only when the ledger records it for the same job. `close` (orchestra only, refused while the job has live rows unless `--force`) closes the job's workspace, refuses while an agent's cwd is inside the job's worktree or one `worktree` recorded for it, removes each and deletes its branch, and ends the job's rows and marks its worktrees removed only when nothing is left. `watch` (for cron) reads each live lead and worker's herdr status, `state_change_seq` and a hash of its screen with Claude Code's spinner, input box and footer stripped, records them in the ledger, counts an agent unchanged for the stale limit (or gone from herdr) as a suspect, and tells the orchestra as `cron` through `send`'s path only when the set of suspects changes.
- `internal/herdr`: runs the `herdr` CLI with a bounded environment and a deadline, parses its one-object JSON reply, and maps `agent prompt` outcomes to the exit codes.
- `internal/atb`: runs `atb linear create` (only an identifier and an https URL are taken from its `--json` output), `claim`, `comment` and `release` through herdr's runner (deadline, process group, allow-list plus `LINEAR_API_KEY`, `LINEAR_API_KEY_CMD`, `LINEAR_API_URL`, `ATB_HOME`); the key travels only in the environment.
- `internal/config`: a repo's `.fleet/config.json`, read from its main checkout `~/dev/<repo>`; standard library only. Unknown keys and wrong types are refused, naming the key.
- `internal/db`: the ledger (`modernc.org/sqlite`, WAL, 5 s busy timeout, immediate transactions) and its schema migration.
- `internal/identity`: the caller's identity from the `FLEET_*` variables.
- `internal/exit`: the exit-code contract and the failure type every command returns.
- `internal/cliargs`: the clap behaviors `flag` lacks (flags after positionals, a string flag that records whether it was given).

## Invariants

Each invariant links to the test that guards it, or is marked untested.

- The CLI behaves as the judge (`judge/`) says: same argv, exit codes, stdout/stderr, ledger rows, herdr state and screens as the earlier implementation. Guarded by `judge/run.sh`; every command's cases are in `judge/usage.sh`, `send.sh`, `lifecycle.sh`, `watch.sh` and `worktree.sh`.
- With Linear on, spawn creates nothing before the agent's work order is created and claimed, and the key never appears in an argument, output or error. Guarded by `cmd/fleet/workorder_test.go`.

## Interfaces

What the judge pins down (the CLI reference and `--json` output are added as they are ported):

- Commands `spawn`, `send`, `done`, `status`, `watch`, `close`, `worktree`, with `--session <name>` global and `--help` on each; `--version`.
- `done --report-file <file> --issue <issue> [--abandon]`: `atb linear comment`, then `atb linear release --agent $FLEET_AGENT --reason done --done` (or `--abandon`), then delivery naming the issue. An atb step exiting non-zero stops it: nothing delivered, row live, exit 5 naming the step. Without `--report-file`, `done` is unchanged.
- `.fleet/config.json` in `~/dev/<repo>`: `max_agents_per_job` (default 4, counting the lead), `resource_check` (default true), `linear` (`team` and `project`; absent means Linear is off). No file means all defaults; an unreadable or invalid file is exit 1.
- `spawn --parent-issue <ISSUE>`: a lead's parent issue, required with Linear on and refused with it off; a worker takes its lead's. With Linear on, after every check and before anything else: `atb linear create --team --project --parent <parent> --title <first non-empty task line without leading #, at most 80 characters> --description-file <task file> --json`, then `atb linear claim <new> --agent <new agent> --source <caller> --scope '<repo>: job <job>'`. The agent gets `FLEET_ISSUE=<new>`; its task starts with `Work order: <url>`. An atb step exiting non-zero is exit 5 naming the step with nothing else created; an issue created but not claimed is listed. Without Linear, spawn is unchanged.
- Exit codes, shared by every command: 0 ok; 1 usage error or precondition refused; 2 no clear signal from herdr (timeout, stalled); 3 target blocked or spawn stopped at an unknown screen; 4 target not found; 5 environment error (herdr, atb, git, database).
- Identity from `FLEET_AGENT`, `FLEET_ROLE`, `FLEET_PARENT`, `FLEET_REPO`, `FLEET_JOB`, `FLEET_ISSUE`, injected into every pane `spawn` creates; `FLEET_WATCH_STALE_SECS` overrides watch's 30-minute limit.
- The ledger at `~/scratch/<repo name>/fleet.db`, SQLite in WAL mode, table `agents` with the columns the judge reads: name, role, job, parent, report_to, task, worktree, pane_id, state (`starting`, `active`, `ended`), started_at, ended_at, last_status, last_seq, last_screen_hash, last_change_at, suspect; table `worktrees` with path, repo, branch, job, created_by, created_at, removed_at (null while the worktree is the job's); schema 4 adds `agents.issue` (the work order) and `agents.parent_issue` (the job's parent issue).
- Messages carry a first line `[FROM: <agent>]`; watch sends as `cron`; a lead's worktree is `~/wt/<repo name>/<job>` on branch `data/<job>` from origin/HEAD of `~/dev/<repo name>`.
- `worktree <repo> [--name <name>] [--branch <branch>]`: needs `FLEET_JOB`; makes `~/wt/<repo>/<job>[-<name>]` on a new branch `<job>[-<name>]` (or `--branch`) from origin/HEAD of `~/dev/<repo>`, without tracking, and prints only the path. The same job calling again gets the path, exit 0, nothing changed; a path that exists otherwise is refused, exit 1.

## Known issues and next steps

Every command is ported; the judge is green against the Go binary and is a blocking CI check. `watch`'s screen filter knows only Claude Code's screen, and its judge arm runs with a 5 s stale limit, so it is sensitive to a loaded machine.

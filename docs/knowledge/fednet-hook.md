# Running `fleet inbox` as the fednet client's hook

The fednet client calls one command for every message a person posts in a thread this machine owns, with the event file as the last argument. On a machine that runs fleet, that command is `fleet inbox`.

## Client arguments

Start the client with the hook and a timeout long enough for a thread agent to start (Claude Code's start-up, the folder-trust prompt and the delivery take well under a minute; the margin covers a loaded machine):

```
fednet client -hub <URL> -db <PATH> -credential <PATH> -socket <SOCKET> \
    -hook-timeout 5m \
    -hook-env XDG_STATE_HOME -hook-env XDG_CONFIG_HOME -hook-env XDG_RUNTIME_DIR \
    -hook-env LINEAR_API_KEY_CMD -hook-env ATB_HOME \
    fleet inbox
```

The hook runs with only `PATH`, `HOME` and the variables named by `-hook-env`. `fleet` and `atb` must be on that `PATH`; the `LINEAR_API_KEY_CMD` (or `LINEAR_API_KEY`) that atb needs reaches it only through `-hook-env`. Leave out `XDG_STATE_HOME` and `XDG_CONFIG_HOME` when the client user does not set them; `XDG_RUNTIME_DIR` is required (see machine setup).

## Scopes and channels

A scope is one fleet on the machine: the herdr session `fleet-<scope>`, the ledger `$XDG_STATE_HOME/fleet/<scope>.db` (`~/.local/state/fleet/<scope>.db`) and the settings `$XDG_CONFIG_HOME/fleet/<scope>.json` (`~/.config/fleet/<scope>.json`). The hook takes the scope from the message's `scope`, `main` when the hub sends none, so a machine with one fleet needs only `main`:

```json
{"linear": {"team": "ABC"}, "fednet": {"socket": "/run/fednet/client.sock", "slack_url": "https://<workspace>.slack.com"}}
```

`linear.team` is the team thread tickets are created in; without it, threads run without tickets. `fednet.socket` is the client's `-socket`. fleet posts through it whenever something goes to a thread: the line telling a thread why no agent was started, a question from `fleet ask-human`, a thread agent's own posts and progress card, which go through `fleet thread post --body-file <file>` and `fleet thread progress`, and the closing line `会话已结束 · <ticket>` that `fleet thread end` posts as small grey text. `fednet.slack_url`, optional, is the Slack workspace's address with no path: `thread handoff` links each thread it names in Slack under it (`<slack_url>/archives/<channel>/p<ts without its dot>`), and without it names the threads in plain words, with no link. `thread handoff --mention <user>` posts with `fednet client post -mention`, which the hub accepts only for a user on its user list. Thread agents are never told the socket or their thread's key; without the setting, nothing can be posted and sessions end without the line. The card and the grey line need a fednet with `client progress` and `client post -footer` (merged into fednet on 2026-10-09); an older client refuses them, which comes back to the agent as fednet's exit code.

Which messages start agents depends on the channel, through two optional payload fields that newer fednet versions send: `channel_name` (absent in a direct message) and `trigger` (`dm` for a direct message to the bot). Until the client's fednet sends them, every message is ignored except in threads the ledger already knows.

| Channel | Thread agents run in |
| -- | -- |
| `<repo prefix><R>` | `<checkouts>/<R>`, the repo's main checkout |
| `<initiative prefix><I>` | `<initiatives>/<I>/`, the checkout of the initiative's repo `<initiative prefix><I>` |
| `<initiative prefix><general initiative>`, direct messages | `<initiatives>/<general initiative>/`, the checkout of that repo |
| anything else | nothing: ignored, exit 0, no reply |

The prefixes, the general initiative and the directories are defaults that the scope's settings file can change, in `channels` (`repo_prefix`, `initiative_prefix`, `general_initiative`) and `paths` (`checkouts`, `initiatives`); [docs/design.md](../design.md) lists the keys and their defaults. With none set, `repo-<R>` runs in `~/dev/<R>`, `x-repo-<I>` in `~/x-repo/<I>/`, and direct messages in `~/x-repo/general/`.

## Machine setup

What the machine needs before the hook can start agents; fleet itself creates none of it:

- The checkouts in the table above: each repo whose channel points at this machine cloned to `<checkouts>/<R>`, and the initiatives' repos (including the general initiative's) cloned to `<initiatives>/<I>/`. When a checkout is missing, the hook posts one line to the thread saying so, starts nothing and exits 0. A cross-repo job's lead runs in `<initiatives>/<I>/<job>/`, which fleet makes and removes; the repos' `.gitignore` should exclude these job directories.
- The scope's settings file, as above.
- The systemd user unit template `~/.config/systemd/user/fleet-scope@.service`, which runs a scope's herdr session, and linger for the user the client runs as (`loginctl enable-linger <user>`), so the user's systemd runs without a login. When the session `fleet-<scope>` is not running, the hook runs `systemctl --user start fleet-scope@<scope>` and waits up to 20 s for it to answer. The server has to belong to systemd rather than to the hook, because the client kills the hook's whole process group when the hook exits:

  ```ini
  [Unit]
  Description=fleet scope %i (herdr session fleet-%i)

  [Service]
  ExecStart=herdr --session fleet-%i server
  Restart=on-failure

  [Install]
  WantedBy=default.target
  ```

- `XDG_RUNTIME_DIR` passed to the hook (`-hook-env XDG_RUNTIME_DIR`, above): without it `systemctl --user` cannot reach the user's systemd. A client run as a system service does not have it, because only a login session sets it, so its unit sets it: `Environment=XDG_RUNTIME_DIR=/run/user/<uid>`.

Checked by hand on 2026-10-09 on a development machine where the client runs as a system service under the agent user, with linger on. In an environment like the hook's (only `PATH`, `HOME` and `XDG_RUNTIME_DIR`), `systemctl --user is-system-running` printed `running` (exit 0); without `XDG_RUNTIME_DIR` it failed with `Failed to connect to bus: No medium found` (exit 1). A transient unit started from such an environment (`systemd-run --user`) stayed active after its starter's process group was killed. Not covered: a run from inside the client's own service (it needs root), and the unit template itself, which was not installed at the time.

The judge has no systemd. Its fake `systemctl` starts the same server detached, so it covers what fleet does around the start, not the unit.

## What fleet presses on its own

Claude Code asks once per directory whether to trust it (the folder-trust dialog, "Quick safety check"), and remembers the answer per directory. Nobody is at the pane of an agent fleet starts, so the first start in a fresh checkout (a new repo or initiative, a fresh machine) would otherwise stay blocked at that dialog; it did, on 2026-10-09, for the first thread agent of a new initiative. So at every start (a thread agent, a lead, a worker, and the retry of a start an earlier run was interrupted in) fleet reads the visible screen and, when it is this dialog and the directory it names is exactly the agent's own (the one fleet chose: the channel's checkout, `~/dev/<repo>`, the cross-repo job's directory, a worker's `--cwd`), moves the cursor to "Yes, I trust this folder" and confirms, one key per press, reading where the cursor is before each press; then it waits for the input box. That directory is fleet's own choice, so trusting it adds nothing to what starting the agent there already does. A dialog naming any other directory, or any other screen, is left alone: the start fails with exit 3 and the screen in the message, and `fleet job end <job> --force` cleans up. A start whose agent exits before its input box (herdr then shows no agent in the pane), as when Claude Code's auto-updater has removed the `claude` it reinstalls for a few seconds, is made once more in the same pane after 3 s; if that fails too, the start fails as before with the pane's text. Each failed start waits out herdr's 30 s start timeout, so the worst case is about a minute.

## What to expect

- The hook exits 0 as soon as the thread's agent has the message, or when the message was ignored, already handled, or dropped because the thread's checkout is missing or the thread prompt is broken (the thread gets one line saying so). fednet then marks the message delivered. A Linear failure does not drop the message; see below.
- A non-zero exit makes fednet retry with growing delays, then move the message to its dead letters and alert. The hook keeps its reservation of the `msg_id` across retries, so a retry delivers to the agent an earlier run started rather than starting another.
- Every post runs `fednet client post` as the user fleet runs as, with only the environment fleet passes on (`PATH`, `HOME`, `USER`, `LOGNAME`, `TMPDIR`, `TERM`, `LANG`, `LC_*`, `XDG_*`, `HERDR_*`) and a 60 s deadline. That user must be able to connect to the socket. A thread agent's post that fails comes back to it with fednet's exit code and stderr; fleet does not retry it.
- Thread agents live in the herdr workspace `threads` of the scope's session, one tab per thread, in the directory of the thread's channel. A thread keeps the channel and directory recorded at its first message, even if the channel is renamed later.

## When Linear fails

Every Linear step of a thread's session (creating or claiming the thread ticket, the `Session <n> started` comment, reading the earlier summaries) runs `atb linear`. A step atb fails is tried twice more, after 3 s and after 7 s; a call that timed out (atb's 60 s) or could not run is not tried again, nor is a claim that atb refuses because another agent holds the ticket (exit 3), and no try begins once the Linear steps of the start have taken 90 s, so they end within 2.5 minutes and the hook stays inside `-hook-timeout 5m`. When a step still fails, the agent starts anyway with the person's message, and its first message says what is missing under `## Note from fleet`. If the ticket was not yet claimed, the session runs without it, and fleet posts one line to the thread: `Linear is unavailable (<step>: <exit>); this session runs without its ticket.`, or for a held claim `The thread ticket <ticket> is claimed by someone else; this session runs without it.` When that session ends, `fleet thread end` keeps its summary in `<scope>-summaries/<slug>/` next to the ledger and writes it to the thread's ticket (creating the ticket when the thread has none) if Linear answers by then; otherwise it stays kept, and the next session that reaches Linear writes it before its own `Session <n> started` comment. The next session claims the ticket as usual.

The hook's stderr is not kept, so each failed try is also appended, as one JSON object per line, to `<scope>-linear.log` next to the scope's ledger, readable only by the user fleet runs as. To read the latest failures of the scope `main` on the machine:

```
jq -c . ~/.local/state/fleet/main-linear.log | tail -n 20
```

Each line has `time` (UTC), `thread`, `agent`, `step` (such as `atb linear claim ABC-12`, or `fleet` for an error of fleet's own around the step, with its message in `stderr`), `try`, `exit` (atb's exit code, -1 when it timed out or could not run), `class`, `stderr` (atb's, cut to 2000 bytes, with anything shaped like a Linear key or a `Bearer` value, and the value of `LINEAR_API_KEY`, replaced by `[redacted]`) and `outcome` (`retried`, `gave up` or `held`). fleet never truncates or rotates the file. The redaction goes by shape, so read the file before you paste it anywhere.

## Main checkouts follow origin

Before a thread agent starts in its channel's checkout, and before a lead starts in its repo's checkout (or, for a cross-repo job, in the initiative's), fleet runs `git fetch origin` there and, when the checkout is on origin/HEAD's branch, has no change to a tracked file and is only behind, `git merge --ff-only --no-overwrite-ignore`. A checkout with local changes, or with an untracked or ignored file where the update brings in a tracked one, on another branch, ahead or diverged, or whose fetch fails (30 s at most), is left as it is; the start goes on, and the agent's first message says why under `## Note from fleet`. The fetch runs as the user fleet runs as, with the hook's environment, so a remote that needs credentials needs them to work without a prompt (`GIT_TERMINAL_PROMPT=0` is set).

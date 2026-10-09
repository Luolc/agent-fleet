# Running `fleet inbox` as the fednet client's hook

The fednet client calls one command for every message a person posts in a thread this machine owns, with the event file as the last argument. On a machine that runs fleet, that command is `fleet inbox`.

## Client arguments

Start the client with the hook and a timeout long enough for a thread agent to start (Claude Code's start-up, the folder-trust prompt and the delivery take well under a minute; the margin covers a loaded machine):

```
fednet client -hub <URL> -db <PATH> -credential <PATH> -socket <SOCKET> \
    -hook-timeout 5m \
    -hook-env XDG_STATE_HOME -hook-env XDG_CONFIG_HOME \
    -hook-env LINEAR_API_KEY_CMD -hook-env ATB_HOME \
    fleet inbox
```

The hook runs with only `PATH`, `HOME` and the variables named by `-hook-env`. `fleet` and `atb` must be on that `PATH`; the `LINEAR_API_KEY_CMD` (or `LINEAR_API_KEY`) that atb needs reaches it only through `-hook-env`. Leave out the `XDG_*` ones when the client user does not set them.

## Scopes and channels

A scope is one fleet on the machine: the herdr session `fleet-<scope>`, the ledger `$XDG_STATE_HOME/fleet/<scope>.db` (`~/.local/state/fleet/<scope>.db`) and the settings `$XDG_CONFIG_HOME/fleet/<scope>.json` (`~/.config/fleet/<scope>.json`). The hook takes the scope from the message's `scope`, `main` when the hub sends none, so a machine with one fleet needs only `main`:

```json
{"linear": {"team": "ABC"}, "fednet": {"socket": "/run/fednet/client.sock"}}
```

`linear.team` is the team thread tickets are created in; without it, threads run without tickets. `fednet.socket` is the client's `-socket`, which fleet needs to tell a thread why no agent was started and which the thread agent is told to post with.

Which messages start agents depends on the channel, through two optional payload fields that newer fednet versions send: `channel_name` (absent in a direct message) and `trigger` (`dm` for a direct message to the bot). Until the client's fednet sends them, every message is ignored except in threads the ledger already knows.

| Channel | Thread agents run in |
| -- | -- |
| `repo-<R>` | `~/dev/<R>`, the repo's main checkout |
| `x-repo-<I>` | `~/x-repo/<I>/`, the checkout of the initiative's repo `x-repo-<I>` |
| `x-repo-general`, direct messages | `~/x-repo/general/`, the checkout of `x-repo-general` |
| anything else | nothing: ignored, exit 0, no reply |

## Machine setup

What the machine needs before the hook can start agents; fleet itself creates none of it:

- The checkouts in the table above: each repo whose channel points at this machine cloned to `~/dev/<R>`, and the initiatives' repos (including `x-repo-general`) cloned to `~/x-repo/<I>/`. When a checkout is missing, the hook posts one line to the thread saying so, starts nothing and exits 0. A cross-repo job's lead runs in `~/x-repo/<I>/<job>/`, which fleet makes and removes; the repos' `.gitignore` should exclude these job directories.
- The scope's settings file, as above.
- The scope's herdr session running (`herdr --session fleet-main server` for `main`). The hook does not start it yet.

## What to expect

- The hook exits 0 as soon as the thread's agent has the message, or when the message was ignored, already handled, or dropped because the thread's checkout is missing or Linear was unavailable (the thread gets one line saying so). fednet then marks the message delivered.
- A non-zero exit makes fednet retry with growing delays, then move the message to its dead letters and alert. The hook keeps its reservation of the `msg_id` across retries, so a retry delivers to the agent an earlier run started rather than starting another.
- Thread agents live in the herdr workspace `threads` of the scope's session, one tab per thread, in the directory of the thread's channel. A thread keeps the channel and directory recorded at its first message, even if the channel is renamed later.

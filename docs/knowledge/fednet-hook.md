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

## Target config

The hook finds the ledger and the herdr session by the message's `target` (`default` when the hub sends none). The target's settings are in `$XDG_CONFIG_HOME/fleet/<target>.json` (`~/.config/fleet/<target>.json`):

```json
{"linear": {"team": "ABC"}, "fednet": {"socket": "/run/fednet/client.sock"}}
```

`linear.team` is the team thread tickets are created in; without it, threads run without tickets. `fednet.socket` is the client's `-socket`, which fleet needs to tell a thread that Linear is unavailable and which the thread agent is told to post with.

## What to expect

- The hook exits 0 as soon as the thread's agent has the message, or when the message was ignored, already handled, or dropped because Linear was unavailable (the thread gets one line saying so). fednet then marks the message delivered.
- A non-zero exit makes fednet retry with growing delays, then move the message to its dead letters and alert. The hook keeps its reservation of the `msg_id` across retries, so a retry delivers to the agent an earlier run started rather than starting another.
- Thread agents live in the herdr workspace `threads` of the target's session (`default` on a development machine), one tab per thread, in `~/cross-repo/threads/`.

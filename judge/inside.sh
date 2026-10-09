#!/bin/bash
# Runs inside the judge container: a headless herdr session, fake Claudes,
# and the binary under test driven from a plain shell (so --session is
# needed, as from cron). Every arm asserts an exit code and the visible
# effect that distinguishes it from the others: stdout/stderr, the ledger
# (read with sqlite3), herdr's state and the fake agents' screens.
#
# Parameters come from run.sh: JUDGE_NAME (the command), JUDGE_ENV_PREFIX
# (the identity variables) and JUDGE_LEDGER (the ledger file name).
set -u
export PATH=/home/agent/bin:$PATH
T=${JUDGE_NAME:?}
P=${JUDGE_ENV_PREFIX:?}
LEDGER=${JUDGE_LEDGER:?}
S=(herdr --session judge)
here=$(cd "$(dirname "$0")" && pwd)
passed=0
fail=0
check() { # <label> <want> <got>
  if [ "$2" = "$3" ]; then
    echo "ok   $1"; passed=$((passed + 1))
  else
    echo "FAIL $1: want [$2], got [$3]"; fail=1
  fi
}
has() { # <label> <text> <needle>...: every needle is in the text
  local label=$1 text=$2
  shift 2
  for needle in "$@"; do
    case "$text" in
      *"$needle"*) ;;
      *) check "$label" "contains $needle" "missing"; printf '%s\n' "$text" | tail -20; return ;;
    esac
  done
  check "$label" yes yes
}
lacks() { # <label> <text> <needle>: the needle is not in the text
  case "$2" in
    *"$3"*) check "$1" "without $3" "present"; printf '%s\n' "$2" | tail -20 ;;
    *) check "$1" yes yes ;;
  esac
}
# A target shows working as soon as the first line of a message is in, so
# the screen is read once the turn is over (no spinner line left).
screen() {
  local out
  for _ in $(seq 1 75); do
    out=$("${S[@]}" agent read "$1" --source visible)
    case "$out" in *"esc to interrupt"*) sleep 0.2 ;; *) break ;; esac
  done
  printf '%s\n' "$out"
}
agent_field() { "${S[@]}" agent get "$1" 2>&1 | jq -r ".result.agent.$2 // .error.code"; }
# After a turn herdr reports `done` until the pane is looked at, not `idle`.
settled() { "${S[@]}" agent wait "$1" --until idle --until done --timeout 20000 >/dev/null; }
# <name>: a workspace with a fake Claude in it; prints the agent's status.
start_fake() {
  local pane
  pane=$("${S[@]}" workspace create --cwd /home/agent --label "$1" --no-focus | jq -r '.result.root_pane.pane_id')
  sleep 1
  "${S[@]}" agent start "$1" --kind claude --pane "$pane" --timeout 20000 | jq -r '.result.agent.agent_status // .error.code'
}
# as <agent> <role> <parent> <job> -- <arguments...>: the binary with the
# caller's identity variables set, as its pane would have them. The work
# order comes from ISSUE, empty by default.
as() {
  local agent=$1 role=$2 parent=$3 job=$4
  shift 5
  env "${P}AGENT=$agent" "${P}ROLE=$role" "${P}PARENT=$parent" "${P}REPO=acme/$R" "${P}JOB=$job" \
    "${P}ISSUE=${ISSUE:-}" "$T" --session judge "$@"
}
R=example-dataset
DB=/home/agent/.local/state/$T/$R/$LEDGER
ledger() { sqlite3 "$DB" "$1" | tr '\n' ' '; }

herdr --session judge server >/home/agent/server.log 2>&1 &
for _ in $(seq 1 100); do "${S[@]}" status server >/dev/null 2>&1 && break; sleep 0.2; done
"${S[@]}" status server >/dev/null || { echo "FAIL herdr server did not start"; cat /home/agent/server.log; exit 1; }
check "fake claude starts idle" idle "$(start_fake fake)"

. "$here/usage.sh"
. "$here/send.sh"
. "$here/lifecycle.sh"
. "$here/watch.sh"
. "$here/worktree.sh"
"${S[@]}" server stop >/dev/null
echo "judge: $passed ok, $( [ "$fail" = 0 ] && echo "0 failed" || echo "some failed")"
exit $fail

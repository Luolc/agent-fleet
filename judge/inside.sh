#!/bin/bash
# Runs inside the judge container: two headless herdr sessions, one per
# scope (fleet-example for the jobs, fleet-main for the threads), fake
# Claudes, and the binary under test driven from a plain shell (so the
# scope comes from --scope or the identity variables, as from a timer). Every arm asserts an exit code and the visible
# effect that distinguishes it from the others: stdout/stderr, the ledger
# (read with sqlite3), herdr's state and the fake agents' screens.
#
# Parameters come from run.sh: JUDGE_NAME (the command), JUDGE_ENV_PREFIX
# (the identity variables) and JUDGE_SUITES (the suites to run).
set -u
export PATH=/home/agent/bin:$PATH
T=${JUDGE_NAME:?}
P=${JUDGE_ENV_PREFIX:?}
SCOPE=example
S=(herdr --session fleet-$SCOPE)
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
# order comes from ISSUE, empty by default. The scope is SCOPE, which is
# not the repo's name: the ledger is per scope, a job's repo is a job
# attribute.
as() {
  local agent=$1 role=$2 parent=$3 job=$4
  shift 5
  env "${P}AGENT=$agent" "${P}ROLE=$role" "${P}PARENT=$parent" "${P}SCOPE=$SCOPE" "${P}JOB=$job" \
    "${P}ISSUE=${ISSUE:-}" "$T" "$@"
}
R=example-dataset
DB=/home/agent/.local/state/$T/$SCOPE.db
# <scope>: the JSON on stdin as that scope's config, with the load check's
# multiplier raised far above any load. The container sees the host's load
# average, so on a busy shared host `job start` and `spawn` would otherwise
# be refused; no suite tests the load check, and the memory check still runs.
# Every scope config the suites write goes through this.
scope_config() {
  mkdir -p "/home/agent/.config/$T"
  jq -c '. + {"resources": {"load_multiplier": 1000}}' > "/home/agent/.config/$T/$1.json"
}
echo '{}' | scope_config "$SCOPE"
# The checkouts of the initiatives' repos: fleet never makes them.
mkdir -p /home/agent/x-repo/general /home/agent/x-repo/example-init
ledger() { sqlite3 "$DB" "$1" | tr '\n' ' '; }
WT=/home/agent/wt/$R
DEV=/home/agent/dev/$R
fake=/home/agent/.fake-claude
mkdir -p "$fake" /home/agent/tasks
thr() { as thread-1 thread "" "" -- "$@"; }
task() { printf '%s\n' "$2" > "/home/agent/tasks/$1.md"; echo "/home/agent/tasks/$1.md"; }
# The identity variables of the claude process in an agent's pane.
proc_env() {
  local pane pid
  pane=$(agent_field "$1" pane_id)
  pid=$("${S[@]}" pane process-info --pane "$pane" | jq -r '.result.process_info.foreground_processes[0].pid')
  tr '\0' '\n' < "/proc/$pid/environ" | grep "^$P" | sort | tr '\n' ' '
}
# <agent> <session> <records...>: the session file and the transcript
# Claude Code would write for the fake Claude of <agent> in herdr session S,
# under ~/.claude, found through the pid `herdr pane process-info` gives.
session() {
  local pid sid=$2
  pid=$("${S[@]}" pane process-info --pane "$(agent_field "$1" pane_id)" |
    jq -r '.result.process_info.foreground_processes[0].pid')
  mkdir -p /home/agent/.claude/sessions /home/agent/.claude/projects/-home-agent
  printf '{"pid":%s,"sessionId":"%s","cwd":"/home/agent","startedAt":1}\n' "$pid" "$2" \
    > "/home/agent/.claude/sessions/$pid.json"
  shift 2
  printf '%s\n' "$@" > "/home/agent/.claude/projects/-home-agent/$sid.jsonl"
}
# <uuid> <message id> <model> <stop_reason>: an assistant record, its text
# 0xREFUSEDTEXT, which nothing fleet writes may carry.
said() {
  printf '{"type":"assistant","uuid":"%s","timestamp":"2030-01-02T03:00:00.000Z","isSidechain":false,"message":{"id":"%s","model":"%s","stop_reason":"%s","content":[{"type":"text","text":"0xREFUSEDTEXT"}]}}' \
    "$1" "$2" "$3" "$4"
}
# The whole of an agent's first message, from the fake's log, read once the
# turn is over.
received() { screen "$1" >/dev/null; cat "$fake/received-$(agent_field "$1" pane_id)" 2>/dev/null; }

# Each suite lays out what it needs with these, so it also runs on its own;
# in a full run an earlier suite has already made it, and they do nothing.
# The job's repo: a local bare repository standing in for GitHub, cloned
# to ~/dev, and ~/seed to push later commits to it. origin gets a commit
# before ~/dev is cloned, so origin/HEAD is set as in a fresh clone.
need_repo() {
  [ -d "$DEV" ] && return
  git config --global user.name judge
  git config --global user.email judge@example.test
  git init -q --bare -b main "/home/agent/remote/$R.git"
  git clone -q "/home/agent/remote/$R.git" /home/agent/seed 2>/dev/null
  git -C /home/agent/seed commit -q --allow-empty -m init
  git -C /home/agent/seed push -q origin main
  git clone -q "/home/agent/remote/$R.git" "$DEV"
}
# The fake agent usage.sh starts.
need_fake() {
  [ "$(agent_field fake agent_status)" = agent_not_found ] && start_fake fake >/dev/null
}
# The thread agent lifecycle.sh starts, and its pane in tpane.
need_thread_agent() {
  [ "$(agent_field thread-1 agent_status)" = agent_not_found ] && start_fake thread-1 >/dev/null
  tpane=$(agent_field thread-1 pane_id)
}

# The suites in the order a full run takes; JUDGE_SUITES (space-separated
# names) picks some of them, still run in this order.
all="usage send lifecycle watch worktree thread watchthread watches unblock ci models"
suites=${JUDGE_SUITES:-$all}
case $suites in *[![:space:]]*) ;; *) echo "JUDGE_SUITES names no suite; the suites are: $all" >&2; exit 2 ;; esac
for suite in $suites; do
  case " $all " in *" $suite "*) ;; *) echo "unknown suite $suite; the suites are: $all" >&2; exit 2 ;; esac
done

# The judge uses the detection rules built into the pinned herdr. By default
# a server fetches them from herdr.dev at start; with no cache in a fresh
# container the fetch always counts as an update and resets every Claude
# pane's detection, so a delivery made just then can see no working state
# within herdr's 5 s and end with no clear signal (exit 2).
herdr_state=/home/agent/.local/state/herdr
mkdir -p /home/agent/.config/herdr
printf '[update]\nmanifest_check = false\n' > /home/agent/.config/herdr/config.toml

# <session>: a headless herdr server, waited for until it answers. `status
# server` exits 0 whether or not a server runs, so the wait reads `.running`.
start_server() {
  herdr --session "$1" server >"/home/agent/server-$1.log" 2>&1 &
  local pid=$!
  for _ in $(seq 1 100); do
    [ "$(herdr --session "$1" status server --json | jq .running)" = true ] && return
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.2
  done
  echo "FAIL herdr server $1 did not start"; cat "/home/agent/server-$1.log"; exit 1
}
for session in fleet-$SCOPE fleet-main; do start_server "$session"; done

for suite in $all; do
  case " $suites " in *" $suite "*) . "$here/$suite.sh" ;; esac
done
# herdr writes this file after every fetch of the rules, failed or not.
check "herdr fetched no detection rules" absent \
  "$([ -e "$herdr_state/agent-detection/status.toml" ] && echo present || echo absent)"
for session in fleet-$SCOPE fleet-main; do herdr --session "$session" server stop >/dev/null; done
echo "judge: $passed ok, $( [ "$fail" = 0 ] && echo "0 failed" || echo "some failed")"
exit $fail

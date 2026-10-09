# Sourced by inside.sh: the command-line surface that needs no herdr call,
# and the refusals every command makes before touching anything.
"$T" --version >/dev/null 2>&1; rc=$?
check "version: exit 0" 0 "$rc"
"$T" --help >/dev/null 2>&1; rc=$?
check "help: exit 0" 0 "$rc"
for c in spawn send done status watch close worktree; do
  out=$("$T" "$c" --help 2>&1); rc=$?
  case "$out" in *Exit*) states=yes ;; *) states=no ;; esac
  check "$c --help: exit 0 and states its exit codes" "0 yes" "$rc $states"
done
out=$("$T" send 2>&1); rc=$?
check "send: missing target is a usage error, exit 1" 1 "$rc"
printf '%s\n' "$out" | grep -qi usage && says=yes || says=no
check "send: usage error prints the usage" yes "$says"
"$T" frobnicate >/dev/null 2>&1; rc=$?
check "unknown command: exit 1" 1 "$rc"

out=$(echo hi | "$T" --session judge send fake 2>&1); rc=$?
check "send: exit 1 without an identity" 1 "$rc"
has "send: names the missing variable" "$out" "${P}AGENT"
out=$(printf '' | as me worker "" "" -- send fake 2>&1); rc=$?
check "send: exit 1 on an empty body" 1 "$rc"
out=$(printf '[FROM: forged]\nhi\n' | as me worker "" "" -- send fake 2>&1); rc=$?
check "send: exit 1 on a body that carries a header" 1 "$rc"
lacks "send: forged header never reaches the target" "$(screen fake)" "forged"

out=$("$T" status 2>&1); rc=$?
check "status: exit 1 without a repo" 1 "$rc"
out=$("$T" --session judge status --repo acme/no-such-dataset 2>&1); rc=$?
check "status: exit 5 without a ledger" 5 "$rc"
has "status: says where the ledger was looked for" "$out" "no ledger at" "no-such-dataset"
"$T" --session judge watch --repo acme/no-such-dataset >/dev/null 2>&1; rc=$?
check "watch: exit 5 without a ledger" 5 "$rc"

out=$(as orchestra orchestra "" "" -- done 2>&1); rc=$?
check "done: exit 1 without a parent" 1 "$rc"
has "done: names the empty variable" "$out" "${P}PARENT"
as item-1-a worker item-1-lead item-1 -- done --result-file /home/agent/missing.md >/dev/null 2>&1; rc=$?
check "done: exit 1 when the result file cannot be read" 1 "$rc"

out=$(as item-1-lead lead orchestra item-1 -- close item-1 2>&1); rc=$?
check "close: exit 1 from a lead" 1 "$rc"
has "close: says only the orchestra closes" "$out" "orchestra"

mkdir -p /home/agent/tasks
printf 'a task\n' > /home/agent/tasks/usage.md
: > /home/agent/tasks/empty.md
spawn_refused() { # <label> <needle> <agent> <role> <job> <spawn arguments...>
  local label=$1 needle=$2 agent=$3 role=$4 job=$5
  shift 5
  local out rc
  out=$(as "$agent" "$role" "" "$job" -- spawn "$@" 2>&1); rc=$?
  check "spawn: $label refused with exit 1" 1 "$rc"
  has "spawn: $label refusal says why" "$out" "$needle"
}
spawn_refused "a worker" "cannot spawn" item-1-a worker item-1 x --task-file /home/agent/tasks/usage.md
spawn_refused "human-interface" "cannot spawn" human-interface human-interface "" x --task-file /home/agent/tasks/usage.md
spawn_refused "an upper-case name" "[a-z0-9-]" orchestra orchestra "" Item_2 --task-file /home/agent/tasks/usage.md
spawn_refused "the name cron" "reserved" orchestra orchestra "" cron --task-file /home/agent/tasks/usage.md
spawn_refused "a name starting with a digit" "start with a letter" orchestra orchestra "" 1 --task-file /home/agent/tasks/usage.md
spawn_refused "a 33-character agent name" "at most 32" orchestra orchestra "" abcdefghij-abcdefghij-abcdef --task-file /home/agent/tasks/usage.md
spawn_refused "--branch from a lead" "leads only" item-1-lead lead item-1 d --branch x --task-file /home/agent/tasks/usage.md
spawn_refused "a missing task file" "cannot read" orchestra orchestra "" item-9 --task-file /home/agent/tasks/missing.md
spawn_refused "an empty task file" "empty" orchestra orchestra "" item-9 --task-file /home/agent/tasks/empty.md
check "spawn: refusals created no ledger" no "$([ -e "$DB" ] && echo yes || echo no)"

# Sourced by inside.sh: spawn, status, done and close against the real
# herdr. The dataset repo is a local bare repository cloned to ~/dev,
# standing in for GitHub. The binary runs from this plain shell with the
# caller's identity variables set as its pane would have them; the
# variables spawn injected are checked separately in the agents' processes.
WT=/home/agent/wt/$R
fake=/home/agent/.fake-claude
mkdir -p "$fake" /home/agent/tasks

git config --global user.name judge
git config --global user.email judge@example.test
# origin gets a commit before ~/dev is cloned, so origin/HEAD is set as in
# a fresh clone; ~/seed pushes later commits to it.
git init -q --bare -b main "/home/agent/remote/$R.git"
git clone -q "/home/agent/remote/$R.git" /home/agent/seed 2>/dev/null
git -C /home/agent/seed commit -q --allow-empty -m init
git -C /home/agent/seed push -q origin main
git clone -q "/home/agent/remote/$R.git" "/home/agent/dev/$R"

orch() { as orchestra orchestra "" "" -- "$@"; }
lead() { as item-1-lead lead orchestra item-1 -- "$@"; }
task() { printf '%s\n' "$2" > "/home/agent/tasks/$1.md"; echo "/home/agent/tasks/$1.md"; }
# The arguments the claude process in an agent's pane was started with.
proc_args() {
  "${S[@]}" pane process-info --pane "$(agent_field "$1" pane_id)" \
    | jq -r '.result.process_info.foreground_processes[0].argv[2:] | join(" ")'
}
# The identity variables of the claude process in an agent's pane.
proc_env() {
  local pane pid
  pane=$(agent_field "$1" pane_id)
  pid=$("${S[@]}" pane process-info --pane "$pane" | jq -r '.result.process_info.foreground_processes[0].pid')
  tr '\0' '\n' < "/proc/$pid/environ" | grep "^$P" | sort | tr '\n' ' '
}
status_flags() { # name:flags for every live agent, in the order status lists them
  orch status --json "$@" | jq -r '[.[] | "\(.name):\(.flags | join(","))"] | join(" ")'
}

# The orchestra is an agent here so the lead's `done` has someone to reach.
check "orchestra starts idle" idle "$(start_fake orchestra)"
opane=$(agent_field orchestra pane_id)

# Lead, through the folder-trust prompt, with ~/dev behind origin.
git -C /home/agent/seed commit -q --allow-empty -m "newer on origin"
git -C /home/agent/seed push -q origin main
stale=$(git -C "/home/agent/dev/$R" rev-parse HEAD)
touch "$fake/trust"
out=$(orch spawn item-1 --task-file "$(task lead 'lead task 0xLEAD1')" --model opus --effort medium 2>&1); rc=$?
check "spawn lead: exit 0 through the trust prompt" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
has "spawn lead: reports what started" "$out" "started item-1-lead in job item-1"
check "spawn lead: worktree on data/item-1" data/item-1 "$(git -C "$WT/item-1" branch --show-current 2>&1)"
check "spawn lead: branched from origin's latest, not the stale local HEAD" \
  "$(git -C "/home/agent/remote/$R.git" rev-parse main) not $stale" \
  "$(git -C "$WT/item-1" rev-parse HEAD) not $stale"
check "spawn lead: agent cwd is the worktree" "$WT/item-1" "$(agent_field item-1-lead cwd)"
lws=$(agent_field item-1-lead workspace_id)
check "spawn lead: workspace named after the job" item-1 \
  "$("${S[@]}" workspace get "$lws" | jq -r .result.workspace.label)"
check "spawn lead: pane renamed" item-1-lead \
  "$("${S[@]}" pane get "$(agent_field item-1-lead pane_id)" | jq -r .result.pane.label)"
check "spawn lead: Claude's fixed arguments" \
  '--dangerously-skip-permissions --disallowedTools AskUserQuestion --settings {"remoteControlAtStartup":false} --model opus --effort medium' \
  "$(proc_args item-1-lead)"
has "spawn lead: header and task on screen" "$(screen item-1-lead)" "[FROM: orchestra]" "0xLEAD1"
check "spawn lead: identity variables in its process" \
  "${P}AGENT=item-1-lead ${P}JOB=item-1 ${P}PARENT=orchestra ${P}REPO=acme/$R ${P}ROLE=lead " \
  "$(proc_env item-1-lead)"
check "spawn lead: ledger row active with its places" \
  "lead|item-1|orchestra|orchestra|active|$WT/item-1|/home/agent/tasks/lead.md|$(agent_field item-1-lead pane_id) " \
  "$(ledger "SELECT role, job, parent, report_to, state, worktree, task, pane_id FROM agents WHERE name = 'item-1-lead'")"
check "spawn lead: started_at is now" "1 " \
  "$(ledger "SELECT abs(started_at - strftime('%s', 'now')) < 120 FROM agents WHERE name = 'item-1-lead'")"
rm "$fake/trust"

# Workers, up to the cap of 4 including the lead.
settled item-1-lead
out=$(lead spawn a --task-file "$(task a 'worker task 0xWORKA')" 2>&1); rc=$?
check "spawn worker: exit 0" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
check "spawn worker: in the job's workspace" "$lws" "$(agent_field item-1-a workspace_id)"
check "spawn worker: cwd is the job's worktree" "$WT/item-1" "$(agent_field item-1-a cwd)"
check "spawn worker: pane renamed" item-1-a \
  "$("${S[@]}" pane get "$(agent_field item-1-a pane_id)" | jq -r .result.pane.label)"
has "spawn worker: header and task on screen" "$(screen item-1-a)" "[FROM: item-1-lead]" "0xWORKA"
check "spawn worker: identity variables in its process" \
  "${P}AGENT=item-1-a ${P}JOB=item-1 ${P}PARENT=item-1-lead ${P}REPO=acme/$R ${P}ROLE=worker " \
  "$(proc_env item-1-a)"
check "spawn worker: ledger row active in the job" "worker|item-1|item-1-lead|active|$WT/item-1 " \
  "$(ledger "SELECT role, job, parent, state, worktree FROM agents WHERE name = 'item-1-a'")"

out=$(lead spawn a --task-file "$(task a 'again')" 2>&1); rc=$?
check "spawn: duplicate name refused with exit 1" 1 "$rc"
has "spawn: duplicate refusal says why" "$out" "already live"

lead spawn b --task-file "$(task b 'worker b')" >/dev/null 2>&1; rc=$?
check "spawn worker b: exit 0" 0 "$rc"
lead spawn c --task-file "$(task c 'worker c')" >/dev/null 2>&1; rc=$?
check "spawn worker c: exit 0" 0 "$rc"
out=$(lead spawn d --task-file "$(task d 'worker d')" 2>&1); rc=$?
check "spawn: fifth agent of a job refused with exit 1" 1 "$rc"
has "spawn: cap refusal says why" "$out" "cap is 4"
check "spawn: refused worker was not started" agent_not_found "$(agent_field item-1-d agent_status)"
check "spawn: refused worker has no ledger row" "0 " "$(ledger "SELECT count(*) FROM agents WHERE name = 'item-1-d'")"

# Status while the job runs: every agent idle at its input box owes work.
for a in item-1-lead item-1-a item-1-b item-1-c; do settled "$a"; done
check "status: live agents in start order with the owes-work flag" \
  "item-1-lead:owes-work item-1-a:owes-work item-1-b:owes-work item-1-c:owes-work" "$(status_flags)"
check "status: --job on the only live job lists the same set" "$(status_flags)" "$(status_flags --job item-1)"
out=$(orch status 2>&1); rc=$?
check "status: table exit 0 with the repo from the environment" 0 "$rc"
case "$out" in NAME*) head=yes ;; *) head=no ;; esac
check "status: table starts with the header" yes "$head"
has "status: table lists the agents and their flags" "$out" "item-1-lead" "item-1-c" "lead" "worker" "owes-work"
fields=$(orch status --json | jq -r '.[] | select(.name == "item-1-a") | "\(.role) \(.job) \(.parent) \(.state) \(.herdr_status) \(.since_change_secs)"')
check "status: json fields of a worker" "worker item-1 item-1-lead active idle null" "${fields/ done / idle }"
check "status: --repo accepts the bare repo name" 4 "$("$T" --session judge status --repo "$R" --json | jq length)"

# Completion reports: workers to the lead, the lead to the orchestra.
for w in a b c; do
  settled item-1-lead
  as "item-1-$w" worker item-1-lead item-1 -- done --result-file "/home/agent/tasks/$w.md" >/dev/null; rc=$?
  check "done item-1-$w: exit 0" 0 "$rc"
done
has "done: report on the lead's screen" "$(screen item-1-lead)" "[FROM: item-1-c]" "item-1-c is done. Result: /home/agent/tasks/c.md"
check "done: the worker's row is ended" "ended|1 " \
  "$(ledger "SELECT state, ended_at IS NOT NULL FROM agents WHERE name = 'item-1-a'")"
settled item-1-lead
check "status: done workers are no longer listed" "item-1-lead:owes-work" "$(status_flags)"

out=$(orch close item-1 2>&1); rc=$?
check "close: refused with exit 1 while the lead is live" 1 "$rc"
has "close: refusal names the live agent" "$out" "item-1-lead"

lead done >/dev/null; rc=$?
check "done item-1-lead: exit 0 to the orchestra" 0 "$rc"
has "done: report on the orchestra's screen" "$(screen orchestra)" "[FROM: item-1-lead]" "item-1-lead is done."
check "done: a report without a result file names none" no \
  "$(case "$(screen orchestra)" in *"item-1-lead is done. Result"*) echo yes ;; *) echo no ;; esac)"

# An agent outside the job whose cwd is inside the worktree blocks close.
spane=$("${S[@]}" tab create --workspace "$("${S[@]}" pane get "$opane" | jq -r .result.pane.workspace_id)" \
  --cwd "$WT/item-1" --label squatter --no-focus | jq -r '.result.root_pane.pane_id')
"${S[@]}" agent start squatter --kind claude --pane "$spane" --timeout 20000 >/dev/null
out=$(orch close item-1 2>&1); rc=$?
check "close: exit 5 while an agent's cwd is in the worktree" 5 "$rc"
has "close: names the agent in the way" "$out" "agent squatter"
check "close: worktree kept while in use" yes "$([ -d "$WT/item-1" ] && echo yes || echo no)"
"${S[@]}" pane close "$spane" >/dev/null

out=$(orch close item-1 2>&1); rc=$?
check "close: exit 0 once nothing is in the way" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
has "close: reports 0 left" "$out" "0 left"
check "close: worktree removed" no "$([ -e "$WT/item-1" ] && echo yes || echo no)"
check "close: branch deleted" "" "$(git -C "/home/agent/dev/$R" branch --list data/item-1)"
check "close: job workspace gone" 0 \
  "$("${S[@]}" workspace list | jq '[.result.workspaces[] | select(.label == "item-1")] | length')"
check "close: job agents gone" 0 \
  "$("${S[@]}" agent list | jq '[.result.agents[] | select(.name | startswith("item-1-"))] | length')"
check "close: no live row left for the job" "0 " \
  "$(ledger "SELECT count(*) FROM agents WHERE job = 'item-1' AND state != 'ended'")"
check "status: no live agents after close" "no live agents" "$(orch status)"

# A custom branch name for the job's worktree.
out=$(orch spawn item-4 --branch feature/item-4 --task-file "$(task item-4 'custom branch')" 2>&1); rc=$?
check "spawn --branch: exit 0" 0 "$rc"
check "spawn --branch: worktree on the given branch" feature/item-4 "$(git -C "$WT/item-4" branch --show-current 2>&1)"
out=$(orch spawn item-4 --task-file "$(task item-4 'again')" 2>&1); rc=$?
check "spawn: a job whose lead is live refused with exit 1" 1 "$rc"
has "spawn: live job refusal says why" "$out" "already live"
orch close item-4 --force >/dev/null 2>&1; rc=$?
check "close --force: live lead closed, exit 0" 0 "$rc"
check "close --force: lead's row ended" "ended " "$(ledger "SELECT state FROM agents WHERE name = 'item-4-lead'")"

# A worktree left over from an earlier job blocks a new job of that name.
mkdir -p "$WT/item-5"
out=$(orch spawn item-5 --task-file "$(task item-5 'never started')" 2>&1); rc=$?
check "spawn: job with a leftover worktree refused with exit 1" 1 "$rc"
has "spawn: leftover refusal names the cleanup" "$out" "$T close item-5 --force"
check "spawn: leftover refusal wrote no row" "0 " "$(ledger "SELECT count(*) FROM agents WHERE name = 'item-5-lead'")"
rmdir "$WT/item-5"

# An unknown start-up screen stops the spawn with exit 3 and the screen.
touch "$fake/unknown-screen"
out=$(orch spawn item-2 --task-file "$(task item-2 'never delivered')" 2>&1); rc=$?
rm "$fake/unknown-screen"
check "spawn: exit 3 at an unknown screen" 3 "$rc"
has "spawn: unknown screen printed with the cleanup command" "$out" \
  "Choose the text style" "created so far" "$T close item-2 --force"
check "spawn: half-made lead's row stays starting" "starting " "$(ledger "SELECT state FROM agents WHERE name = 'item-2-lead'")"
orch close item-2 >/dev/null 2>&1; rc=$?
check "close: half-made job refused without --force" 1 "$rc"
orch close item-2 --force >/dev/null 2>&1; rc=$?
check "close --force: half-made job cleaned up, exit 0" 0 "$rc"
check "close --force: worktree removed" no "$([ -e "$WT/item-2" ] && echo yes || echo no)"
check "close --force: half-made lead's row ended" "ended " "$(ledger "SELECT state FROM agents WHERE name = 'item-2-lead'")"

# A gateway that refuses the session is a failed start: exit 5, no retry.
touch "$fake/gateway-full"
out=$(orch spawn item-3 --task-file "$(task item-3 'never delivered')" 2>&1); rc=$?
rm "$fake/gateway-full"
check "spawn: exit 5 when the gateway refuses the session" 5 "$rc"
has "spawn: gateway refusal printed with the cleanup command" "$out" \
  "machine example-1 is at its limit" "$T close item-3 --force"
orch close item-3 --force >/dev/null 2>&1; rc=$?
check "close --force: after a failed start, exit 0" 0 "$rc"
check "close: nothing left running" "orchestra fake " \
  "$("${S[@]}" agent list | jq -r '[.result.agents[].name] | sort | reverse | join(" ")') "

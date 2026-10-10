# Sourced by inside.sh: job start, worktree, spawn, status, done, job list
# and job end against the real herdr. The job's repo is a local bare
# repository cloned to ~/dev, standing in for GitHub. The binary runs from
# this plain shell with the caller's identity variables set as its pane
# would have them; the variables the starts injected are checked
# separately in the agents' processes.
WT=/home/agent/wt/$R
DEV=/home/agent/dev/$R
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
git clone -q "/home/agent/remote/$R.git" "$DEV"

thr() { as thread-1 thread "" "" -- "$@"; }
lead() { as item-1-lead lead thread-1 item-1 -- "$@"; }
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
  thr status --json "$@" | jq -r '[.agents[] | "\(.name):\(.flags | join(","))"] | join(" ")'
}
status_jobs() { # job:lead:workers for every open job, as status --json lists them
  thr status --json "$@" | jq -r '[.jobs[] | "\(.job):\(.lead):\(.workers | join(","))"] | join(" ")'
}
job_row() { ledger "SELECT parent_issue, key, repo, lead_cwd, state, outcome FROM jobs WHERE job = '$1' ORDER BY id DESC LIMIT 1"; }

# The thread agent is an agent here so a worker's `done` has a lead-shaped
# recipient to reach and `job end --force` a caller; nothing starts thread
# agents yet.
check "thread agent starts idle" idle "$(start_fake thread-1)"
tpane=$(agent_field thread-1 pane_id)

# A single-repo job's lead, through the folder-trust prompt, in ~/dev/$R.
touch "$fake/trust"
out=$(thr job start item-1 --repo "$R" --task-file "$(task lead 'lead task 0xLEAD1')" --model opus --effort medium 2>&1); rc=$?
check "job start: exit 0 through the trust prompt" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
has "job start: reports what started" "$out" "started item-1-lead in job item-1 ($DEV)"
check "job start: agent cwd is the main checkout" "$DEV" "$(agent_field item-1-lead cwd)"
check "job start: no worktree was made" no "$([ -e "$WT/item-1" ] && echo yes || echo no)"
lws=$(agent_field item-1-lead workspace_id)
check "job start: workspace named after the job" item-1 \
  "$("${S[@]}" workspace get "$lws" | jq -r .result.workspace.label)"
check "job start: pane renamed" item-1-lead \
  "$("${S[@]}" pane get "$(agent_field item-1-lead pane_id)" | jq -r .result.pane.label)"
check "job start: Claude's fixed arguments" \
  '--dangerously-skip-permissions --disallowedTools AskUserQuestion --settings {"remoteControlAtStartup":false} --model opus --effort medium' \
  "$(proc_args item-1-lead)"
# The first message is longer than the fake's 20 lines: its tail is on the
# screen, the whole of it in the fake's log, read once the turn is over.
has "job start: task on screen" "$(screen item-1-lead)" "## Your task" "0xLEAD1"
received() { screen "$1" >/dev/null; cat "$fake/received-$(agent_field "$1" pane_id)" 2>/dev/null; }
check "job start: the first message is the header, then the lead prompt" \
  "[FROM: thread-1]|You are a lead run by fleet: you run the job item-1 (scope $SCOPE) for the people in its home thread. The job is single-repo: its repo is $R, main checkout ~/dev/$R; \`<repo>\` below is $R. You run in $DEV and only read there: you change no repo yourself. You split the work, start workers, merge what they make, and end the job.|" \
  "$(received item-1-lead | head -2 | tr '\n' '|')"
has "job start: the lead prompt names its identity, cap and ending, then the thread's latest message, then the task" "$(received item-1-lead)" \
  "FLEET_AGENT=item-1-lead, FLEET_ROLE=lead, FLEET_JOB=item-1, FLEET_PARENT=thread-1" "FLEET_ISSUE= (empty: this job has no Linear work orders)" \
  "at most 4 live agents, you included" "\`fleet job end --report-file <file>\`" \
  "## Latest message from a person in the home thread" "no message from a person recorded for this job's home thread" "## Your task" "0xLEAD1"
lacks "job start: no placeholder left in the lead's first message" "$(received item-1-lead)" "{{"
lacks "job start: no work order line without Linear" "$(received item-1-lead)" "Work order:"
check "job start: identity variables in its process" \
  "${P}AGENT=item-1-lead ${P}ISSUE= ${P}JOB=item-1 ${P}PARENT=thread-1 ${P}ROLE=lead ${P}SCOPE=$SCOPE " \
  "$(proc_env item-1-lead)"
check "job start: ledger row active with its places" \
  "lead|item-1|thread-1|thread-1|active|$DEV|/home/agent/tasks/lead.md|$(agent_field item-1-lead pane_id) " \
  "$(ledger "SELECT role, job, parent, report_to, state, cwd, task, pane_id FROM agents WHERE name = 'item-1-lead'")"
check "job start: job row open with its repo and the lead's cwd" "||$R|$DEV|open| " "$(job_row item-1)"
check "job start: started_at is now" "1 " \
  "$(ledger "SELECT abs(started_at - strftime('%s', 'now')) < 120 FROM agents WHERE name = 'item-1-lead'")"
rm "$fake/trust"

# The lead opens a worktree (branch item-1, no prefix) and starts workers
# in it, up to the cap of 4 including the lead.
settled item-1-lead
wt=$(lead worktree "$R" 2>&1); rc=$?
check "worktree from the lead: exit 0 with the path" "0 $WT/item-1" "$rc $wt"
check "worktree: branch named after the job" item-1 "$(git -C "$WT/item-1" branch --show-current 2>&1)"
out=$(lead spawn a --cwd "$wt" --task-file "$(task a 'worker task 0xWORKA')" 2>&1); rc=$?
check "spawn worker: exit 0" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
has "spawn worker: reports what started" "$out" "started item-1-a in job item-1 ($WT/item-1)"
check "spawn worker: in the job's workspace" "$lws" "$(agent_field item-1-a workspace_id)"
check "spawn worker: cwd is --cwd" "$WT/item-1" "$(agent_field item-1-a cwd)"
check "spawn worker: pane renamed" item-1-a \
  "$("${S[@]}" pane get "$(agent_field item-1-a pane_id)" | jq -r .result.pane.label)"
has "spawn worker: task on screen" "$(screen item-1-a)" "## Your task" "0xWORKA"
check "spawn worker: the first message is the header, then the worker prompt" \
  "[FROM: item-1-lead]|You are a worker run by fleet: item-1-a, one task in the job item-1 (scope $SCOPE), for your lead item-1-lead. You run in $WT/item-1. The job is single-repo: its repo is $R, main checkout ~/dev/$R; \`<repo>\` below is $R.|" \
  "$(received item-1-a | head -2 | tr '\n' '|')"
has "spawn worker: the worker prompt names its lead and done, then the task" "$(received item-1-a)" \
  "FLEET_PARENT=item-1-lead, FLEET_ISSUE= (empty: this job has no Linear work orders)" "\`fleet done --report-file <file>\`" "## Your task" "0xWORKA"
lacks "spawn worker: no placeholder left" "$(received item-1-a)" "{{"
lacks "spawn worker: no lead prompt" "$(received item-1-a)" "You are a lead"
check "spawn worker: identity variables in its process" \
  "${P}AGENT=item-1-a ${P}ISSUE= ${P}JOB=item-1 ${P}PARENT=item-1-lead ${P}ROLE=worker ${P}SCOPE=$SCOPE " \
  "$(proc_env item-1-a)"
check "spawn worker: ledger row active in the job with its cwd" "worker|item-1|item-1-lead|active|$WT/item-1 " \
  "$(ledger "SELECT role, job, parent, state, cwd FROM agents WHERE name = 'item-1-a'")"

out=$(lead spawn a --cwd "$wt" --task-file "$(task a 'again')" 2>&1); rc=$?
check "spawn: duplicate name refused with exit 1" 1 "$rc"
has "spawn: duplicate refusal says why" "$out" "already live"

lead spawn b --cwd "$wt" --task-file "$(task b 'worker b')" >/dev/null 2>&1; rc=$?
check "spawn worker b: exit 0" 0 "$rc"
lead spawn c --cwd "$DEV" --task-file "$(task c 'worker c')" >/dev/null 2>&1; rc=$?
check "spawn worker c in another directory: exit 0" 0 "$rc"
check "spawn worker c: cwd is the main checkout" "$DEV" "$(agent_field item-1-c cwd)"
out=$(lead spawn d --cwd "$wt" --task-file "$(task d 'worker d')" 2>&1); rc=$?
check "spawn: fifth agent of a job refused with exit 1" 1 "$rc"
has "spawn: cap refusal says why" "$out" "cap is 4"
check "spawn: refused worker was not started" agent_not_found "$(agent_field item-1-d agent_status)"
check "spawn: refused worker has no ledger row" "0 " "$(ledger "SELECT count(*) FROM agents WHERE name = 'item-1-d'")"

check "job list: the open job with its lead and live workers" \
  "item-1	-	-	$R	item-1-lead	item-1-a,item-1-b,item-1-c" "$(thr job list)"
check "job list --json: the same job" "item-1 item-1-lead 3" \
  "$(thr job list --json | jq -r '.[] | "\(.job) \(.lead) \(.workers | length)"')"

# Settings from .fleet/config.json in ~/dev: refusals before anything is
# created. The file is removed again, so the rest runs on the defaults.
cfg=$DEV/.fleet/config.json
mkdir -p "$(dirname "$cfg")"
echo '{"max_agents_per_job": "4"}' > "$cfg"
out=$(thr job start item-9 --repo "$R" --task-file "$(task lead9 'lead 9')" 2>&1); rc=$?
check "job start: invalid config refused with exit 1" 1 "$rc"
has "job start: config refusal names the key" "$out" "max_agents_per_job"
echo '{"linear": {"team": "EX", "project": "Example project"}}' > "$cfg"
out=$(thr job start item-9 --repo "$R" --task-file "$(task lead9 'lead 9')" 2>&1); rc=$?
check "job start: with Linear on, a lead without a parent flag is refused with exit 1" 1 "$rc"
has "job start: parent refusal says why" "$out" "--parent-issue or --new-parent"
check "job start: refused lead has no ledger row" "0 " "$(ledger "SELECT count(*) FROM agents WHERE name = 'item-9-lead'")"
check "job start: refused job has no job row" "" "$(job_row item-9)"
rm -r "$(dirname "$cfg")"

# Status while the job runs: every agent idle at its input box owes work.
for a in item-1-lead item-1-a item-1-b item-1-c; do settled "$a"; done
check "status: live agents in start order with the owes-work flag" \
  "item-1-lead:owes-work item-1-a:owes-work item-1-b:owes-work item-1-c:owes-work" "$(status_flags)"
check "status: --job on the only live job lists the same set" "$(status_flags)" "$(status_flags --job item-1)"
check "status: the open job with its lead and workers" "item-1:item-1-lead:item-1-a,item-1-b,item-1-c" "$(status_jobs)"
out=$(thr status 2>&1); rc=$?
check "status: table exit 0 with the scope from the environment" 0 "$rc"
case "$out" in JOB*) head=yes ;; *) head=no ;; esac
check "status: table starts with the jobs header" yes "$head"
has "status: table lists the job, the agents and their flags" "$out" "item-1-lead" "item-1-c" "lead" "worker" "owes-work" "
NAME "
fields=$(thr status --json | jq -r '.agents[] | select(.name == "item-1-a") | "\(.role) \(.job) \(.parent) \(.state) \(.herdr_status) \(.since_change_secs)"')
check "status: json fields of a worker" "worker item-1 item-1-lead active idle null" "${fields/ done / idle }"
check "status: --scope from a plain shell" 4 "$("$T" --scope "$SCOPE" status --json | jq '.agents | length')"

# The lead cannot end the job while workers are live, and nobody else ends
# it without --force.
out=$(lead job end --report-file "$(task lead-report 'what the job did 0xREPORT1')" 2>&1); rc=$?
check "job end: exit 1 while workers are live" 1 "$rc"
has "job end: names the live workers" "$out" "live workers: item-1-a, item-1-b, item-1-c"
out=$(thr job end item-1 2>&1); rc=$?
check "job end: exit 1 from a thread agent without --force" 1 "$rc"
has "job end: says who ends a job" "$out" "a thread cannot end a job"
out=$(lead job end 2>&1); rc=$?
check "job end: exit 1 without a report file" 1 "$rc"
has "job end: says the report file is required" "$out" "--report-file"

# Completion reports: workers to the lead, the lead to the thread agent.
for w in a b; do
  settled item-1-lead
  as "item-1-$w" worker item-1-lead item-1 -- done --report-file "/home/agent/tasks/$w.md" >/dev/null; rc=$?
  check "done item-1-$w: exit 0" 0 "$rc"
done
has "done: report on the lead's screen" "$(screen item-1-lead)" "[FROM: item-1-b]" "item-1-b is done. Report: /home/agent/tasks/b.md"
# With a work order the report goes to Linear first, through a fake atb
# that logs its arguments.
mkdir -p /home/agent/fake-atb
printf '#!/bin/sh\necho "$*" >> /home/agent/atb.log\n' > /home/agent/fake-atb/atb
chmod +x /home/agent/fake-atb/atb
settled item-1-lead
PATH=/home/agent/fake-atb:$PATH ISSUE=EX-7 as item-1-c worker item-1-lead item-1 -- \
  done --report-file /home/agent/tasks/c.md --abandon >/dev/null; rc=$?
check "done item-1-c with a work order: exit 0" 0 "$rc"
check "done: report written to the issue, then the issue released as abandoned" \
  "linear comment EX-7 --body-file /home/agent/tasks/c.md|linear release EX-7 --agent item-1-c --reason abandoned --abandon|" \
  "$(tr '\n' '|' < /home/agent/atb.log)"
has "done: abandoned report names the issue and the file" "$(screen item-1-lead)" "[FROM: item-1-c]" \
  "item-1-c abandoned the task. Issue: EX-7. Report: /home/agent/tasks/c.md"
check "done: the worker's row is ended" "ended|1 " \
  "$(ledger "SELECT state, ended_at IS NOT NULL FROM agents WHERE name = 'item-1-a'")"
settled item-1-lead
check "status: done workers are no longer listed" "item-1-lead:owes-work" "$(status_flags)"
check "job list: done workers are no longer listed" "item-1	-	-	$R	item-1-lead	-" "$(thr job list)"

out=$(lead done 2>&1); rc=$?
check "done: exit 1 from a lead" 1 "$rc"
has "done: points the lead to job end" "$out" "a lead ends its job with \`$T job end\`"
check "job list: the job stays open with its lead" "item-1	-	-	$R	item-1-lead	-" "$(thr job list)"

# An agent outside the job whose cwd is inside the worktree blocks the end.
spane=$("${S[@]}" tab create --workspace "$("${S[@]}" pane get "$tpane" | jq -r .result.pane.workspace_id)" \
  --cwd "$WT/item-1" --label squatter --no-focus | jq -r '.result.root_pane.pane_id')
"${S[@]}" agent start squatter --kind claude --pane "$spane" --timeout 20000 >/dev/null
out=$(lead job end --report-file /home/agent/tasks/lead-report.md 2>&1); rc=$?
check "job end: exit 5 while an agent's cwd is in the worktree" 5 "$rc"
has "job end: names the agent in the way" "$out" "agent squatter"
check "job end: worktree kept while in use" yes "$([ -d "$WT/item-1" ] && echo yes || echo no)"
check "job end: job and lead still live after the refusal" "open active " \
  "$(ledger "SELECT state FROM jobs WHERE job = 'item-1'")$(ledger "SELECT state FROM agents WHERE name = 'item-1-lead'")"
"${S[@]}" pane close "$spane" >/dev/null

# No work order and no parent: no Linear step, the conclusion is printed.
out=$(lead job end --report-file /home/agent/tasks/lead-report.md 2>&1); rc=$?
check "job end: exit 0 once nothing is in the way" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
has "job end: reports the outcome and the conclusion" "$out" "ended job item-1: done, 1 rows ended" \
  "Job item-1 ended: done." "Lead: item-1-lead. Report: /home/agent/tasks/lead-report.md"
check "job end: worktree removed" no "$([ -e "$WT/item-1" ] && echo yes || echo no)"
check "job end: branch deleted" "" "$(git -C "$DEV" branch --list item-1)"
check "job end: main checkout kept" yes "$([ -d "$DEV/.git" ] && echo yes || echo no)"
check "job end: job workspace gone" 0 \
  "$("${S[@]}" workspace list | jq '[.result.workspaces[] | select(.label == "item-1")] | length')"
check "job end: job agents gone" 0 \
  "$("${S[@]}" agent list | jq '[.result.agents[] | select(.name | startswith("item-1-"))] | length')"
check "job end: no live row left for the job" "0 " \
  "$(ledger "SELECT count(*) FROM agents WHERE job = 'item-1' AND state != 'ended'")"
check "job end: job row ended as done" "||$R|$DEV|ended|done " "$(job_row item-1)"
check "status: nothing after the job ended" "no open jobs

no live agents" "$(thr status)"
check "job list: nothing open after the job ended" "" "$(thr job list)"

# A cross-repo job with a parent issue: Linear on through the parent's
# team and project, read with a fake atb that answers the query; the lead
# runs in ~/x-repo/general/<job>/ (the thread is of no channel).
: > /home/agent/atb.log
cat > /home/agent/fake-atb/atb <<'ATB'
#!/bin/sh
echo "$*" >> /home/agent/atb.log
case "$2" in
  query) echo '{"issue":{"team":{"key":"QT"},"project":{"name":"Queried project"}}}' ;;
  create) echo '{"identifier":"QT-12","url":"https://linear.example.test/QT-12"}' ;;
esac
ATB
out=$(PATH=/home/agent/fake-atb:$PATH thr job start wire --parent-issue QT-10 --task-file "$(task wire 'Wire the repos 0xWIRE')" 2>&1); rc=$?
check "job start cross-repo: exit 0" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
has "job start cross-repo: reports the cross-repo directory" "$out" "started wire-lead in job wire (/home/agent/x-repo/general/wire)"
check "job start cross-repo: the parent is read last, claimed, then the work order created and claimed" \
  "linear query { issue(id: \"QT-10\") { team { key } project { name } } }|linear claim QT-10 --agent wire-lead --source thread-1 --scope cross-repo: job wire|linear create --team QT --project Queried project --parent QT-10 --title Wire the repos 0xWIRE --description-file /home/agent/tasks/wire.md --json|linear claim QT-12 --agent wire-lead --source thread-1 --scope cross-repo: job wire|" \
  "$(tr '\n' '|' < /home/agent/atb.log)"
check "job start cross-repo: agent cwd is the cross-repo directory" /home/agent/x-repo/general/wire "$(agent_field wire-lead cwd)"
check "job start cross-repo: identity variables carry the work order" \
  "${P}AGENT=wire-lead ${P}ISSUE=QT-12 ${P}JOB=wire ${P}PARENT=thread-1 ${P}ROLE=lead ${P}SCOPE=$SCOPE " \
  "$(proc_env wire-lead)"
has "job start cross-repo: the work order's URL heads the task" "$(screen wire-lead)" "Work order: https://linear.example.test/QT-12" "0xWIRE"
has "job start cross-repo: the lead prompt names the initiative, its charter and the parent issue" "$(received wire-lead | tr '\n' '|')" \
  "The job is cross-repo: it runs inside the checkout of the initiative x-repo-general (/home/agent/x-repo/general)" \
  "read /home/agent/x-repo/general/AGENTS.md (the initiative's charter)" \
  "FLEET_ISSUE=QT-12 (your work order, https://linear.example.test/QT-12). The job's parent issue is QT-10;" \
  "## Your task||Work order: https://linear.example.test/QT-12||Wire the repos 0xWIRE"
check "job start cross-repo: job row with the parent and no repo" "QT-10|||/home/agent/x-repo/general/wire|open| " "$(job_row wire)"
check "job list: the cross-repo job" "wire	QT-10	-	-	wire-lead	-" "$(thr job list)"
# Without the fake atb on PATH: the refusal comes from the ledger, before
# Linear is needed.
out=$(thr job start wire-2 --parent-issue QT-10 --task-file "$(task wire2 'again')" 2>&1); rc=$?
check "job start: a second job on a parent with a live lead refused with exit 1, without Linear" 1 "$rc"
has "job start: the refusal names the lead to talk to" "$out" "wire-lead is live on parent issue QT-10"
# A worker of the cross-repo job gets its work order where the parent says.
: > /home/agent/atb.log
settled wire-lead
out=$(PATH=/home/agent/fake-atb:$PATH as wire-lead lead thread-1 wire -- spawn a --cwd /home/agent/x-repo/general/wire --task-file "$(task wirea 'wire worker')" 2>&1); rc=$?
check "spawn in a cross-repo job: exit 0" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
check "spawn in a cross-repo job: the parent is queried, the work order created under it and claimed" \
  "linear query { issue(id: \"QT-10\") { team { key } project { name } } }|linear create --team QT --project Queried project --parent QT-10 --title wire worker --description-file /home/agent/tasks/wirea.md --json|linear claim QT-12 --agent wire-a --source wire-lead --scope cross-repo: job wire|" \
  "$(tr '\n' '|' < /home/agent/atb.log)"
# The lead ends the cross-repo job: its own work order gets the report
# and is released, the parent gets the conclusion and is released, the
# worker's pane (done, still open) is closed, the directory removed.
settled wire-a
PATH=/home/agent/fake-atb:$PATH ISSUE=QT-12 as wire-a worker wire-lead wire -- done --report-file /home/agent/tasks/wirea.md >/dev/null 2>&1; rc=$?
check "done wire-a: exit 0" 0 "$rc"
: > /home/agent/atb.log
cat > /home/agent/fake-atb/atb <<'ATB'
#!/bin/sh
echo "$*" >> /home/agent/atb.log
[ "$2" = comment ] && cat "$5" >> /home/agent/atb-bodies.log
[ "$2 $3" = "$(cat /home/agent/atb-fail 2>/dev/null)" ] && { echo "error: refused: no holder" >&2; exit 4; }
exit 0
ATB
# First the parent's release fails: exit 5, nothing changed but the steps
# done, which the retry does not repeat (the fake then refuses the work
# order's release, as atb does for an issue nobody holds). The failing
# step is read from a file: the runner's allow-list passes no variable.
echo "release QT-10" > /home/agent/atb-fail
out=$(PATH=/home/agent/fake-atb:$PATH ISSUE=QT-12 as wire-lead lead thread-1 wire -- job end --report-file /home/agent/tasks/wire.md --abandon 2>&1); rc=$?
check "job end cross-repo: exit 5 when the parent's release fails" 5 "$rc"
has "job end cross-repo: names the failed step" "$out" "atb linear release QT-10 failed"
check "job end cross-repo: job and lead still live, workspace kept" "open active 1" \
  "$(ledger "SELECT state FROM jobs WHERE job = 'wire'")$(ledger "SELECT state FROM agents WHERE name = 'wire-lead'")$("${S[@]}" workspace list | jq '[.result.workspaces[] | select(.label == "wire")] | length')"
echo "release QT-12" > /home/agent/atb-fail
out=$(PATH=/home/agent/fake-atb:$PATH ISSUE=QT-12 as wire-lead lead thread-1 wire -- job end --report-file /home/agent/tasks/wire.md --abandon 2>&1); rc=$?
check "job end cross-repo: exit 0 on the retry" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
check "job end cross-repo: report to the work order, released; conclusion to the parent, released once each, the retry only releasing the parent" \
  "linear comment QT-12 --body-file /home/agent/tasks/wire.md|linear release QT-12 --agent wire-lead --reason abandoned --abandon|linear comment QT-10 --body-file |linear release QT-10 --agent wire-lead --reason abandoned --abandon|linear release QT-10 --agent wire-lead --reason abandoned --abandon|" \
  "$(sed 's|--body-file /tmp/.*|--body-file |' /home/agent/atb.log | tr '\n' '|')"
check "job end cross-repo: the conclusion written to the parent" \
  "Wire the repos 0xWIRE|Job wire ended: abandoned.|Lead: wire-lead. Work order: QT-12. Report: /home/agent/tasks/wire.md|" \
  "$(tr '\n' '|' < /home/agent/atb-bodies.log)"
rm /home/agent/atb-fail
has "job end cross-repo: the conclusion printed" "$out" "Job wire ended: abandoned."
check "job end cross-repo: directory removed" no "$([ -e /home/agent/x-repo/general/wire ] && echo yes || echo no)"
check "job end cross-repo: job ended as abandoned with its rows" "ended|abandoned ended ended " \
  "$(ledger "SELECT state, outcome FROM jobs WHERE job = 'wire'")$(ledger "SELECT state FROM agents WHERE job = 'wire' ORDER BY id")"
check "job end cross-repo: workspace and agents gone" "0 0" \
  "$("${S[@]}" workspace list | jq '[.result.workspaces[] | select(.label == "wire")] | length') $("${S[@]}" agent list | jq '[.result.agents[] | select(.name | startswith("wire-"))] | length')"
cat > /home/agent/fake-atb/atb <<'ATB'
#!/bin/sh
echo "$*" >> /home/agent/atb.log
case "$2" in
  query) echo '{"issue":{"team":{"key":"QT"},"project":{"name":"Queried project"}}}' ;;
  create) echo '{"identifier":"QT-12","url":"https://linear.example.test/QT-12"}' ;;
esac
ATB

# The dedup key: a second open job with the same key is refused, naming
# the first; a job whose lead is live cannot be started again.
out=$(thr job start item-4 --repo "$R" --key PR-4 --task-file "$(task item-4 'keyed')" 2>&1); rc=$?
check "job start --key: exit 0" 0 "$rc"
check "job start --key: the key is recorded" "|PR-4|$R|$DEV|open| " "$(job_row item-4)"
out=$(thr job start item-5 --repo "$R" --key PR-4 --task-file "$(task item-5 'same key')" 2>&1); rc=$?
check "job start: the same key refused with exit 1" 1 "$rc"
has "job start: key refusal names the open job" "$out" "job item-4 is open with the same key"
out=$(thr job start item-4 --repo "$R" --task-file "$(task item-4 'again')" 2>&1); rc=$?
check "job start: a job that is open refused with exit 1" 1 "$rc"
has "job start: open job refusal says why" "$out" "already open"
out=$(as item-4-lead lead thread-1 item-4 -- job end item-4 --force 2>&1); rc=$?
check "job end --force: exit 1 from a lead" 1 "$rc"
has "job end --force: says who reclaims" "$out" "a lead cannot reclaim a job"
thr job end item-4 --force >/dev/null 2>&1; rc=$?
check "job end --force: live lead reclaimed, exit 0" 0 "$rc"
check "job end --force: lead's row ended, job abandoned" "ended ended|abandoned " \
  "$(ledger "SELECT state FROM agents WHERE name = 'item-4-lead'")$(ledger "SELECT state, outcome FROM jobs WHERE job = 'item-4'")"
out=$(thr job start item-5 --repo "$R" --key PR-4 --task-file "$(task item-5 'key free again')" 2>&1); rc=$?
check "job start: the key is free once the job ended" 0 "$rc"
env "${P}SCOPE=$SCOPE" "$T" job end item-5 --force >/dev/null 2>&1; rc=$?
check "job end --force: from a shell with no identity, exit 0" 0 "$rc"

# A directory left over from an earlier cross-repo job blocks a new job
# of that name.
mkdir -p /home/agent/x-repo/general/item-6
out=$(thr job start item-6 --task-file "$(task item-6 'never started')" 2>&1); rc=$?
check "job start: job with a leftover directory refused with exit 1" 1 "$rc"
has "job start: leftover refusal names the cleanup" "$out" "$T job end item-6 --force"
check "job start: leftover refusal wrote no row" "0 " "$(ledger "SELECT count(*) FROM agents WHERE name = 'item-6-lead'")"
rmdir /home/agent/x-repo/general/item-6

# An unknown start-up screen stops the start with exit 3 and the screen.
touch "$fake/unknown-screen"
out=$(thr job start item-2 --repo "$R" --task-file "$(task item-2 'never delivered')" 2>&1); rc=$?
rm "$fake/unknown-screen"
check "job start: exit 3 at an unknown screen" 3 "$rc"
has "job start: unknown screen printed with the cleanup command" "$out" \
  "Choose the text style" "created so far" "$T job end item-2 --force"
check "job start: half-made lead's row stays starting, job open" "starting open " \
  "$(ledger "SELECT state FROM agents WHERE name = 'item-2-lead'")$(ledger "SELECT state FROM jobs WHERE job = 'item-2'")"
thr job end item-2 >/dev/null 2>&1; rc=$?
check "job end: half-made job not ended by a thread agent without --force" 1 "$rc"
thr job end item-2 --force >/dev/null 2>&1; rc=$?
check "job end --force: half-made job cleaned up, exit 0" 0 "$rc"
check "job end --force: half-made lead's row and job ended" "ended ended " \
  "$(ledger "SELECT state FROM agents WHERE name = 'item-2-lead'")$(ledger "SELECT state FROM jobs WHERE job = 'item-2'")"

# A gateway that refuses the session is a failed start: exit 5, no retry.
touch "$fake/gateway-full"
out=$(thr job start item-3 --task-file "$(task item-3 'never delivered')" 2>&1); rc=$?
rm "$fake/gateway-full"
check "job start: exit 5 when the gateway refuses the session" 5 "$rc"
has "job start: gateway refusal printed with the cleanup command" "$out" \
  "machine example-1 is at its limit" "$T job end item-3 --force"
thr job end item-3 --force >/dev/null 2>&1; rc=$?
check "job end --force: after a failed start, exit 0" 0 "$rc"
check "job end --force: its cross-repo directory removed" no "$([ -e /home/agent/x-repo/general/item-3 ] && echo yes || echo no)"
check "job end: nothing left running" "thread-1 fake " \
  "$("${S[@]}" agent list | jq -r '[.result.agents[].name] | sort | reverse | join(" ")') "

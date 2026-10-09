# Sourced by inside.sh after worktree.sh: `inbox`, `thread end`, `thread
# set-project` and `thread relate` against the real herdr, with a fake atb
# and a fake fednet that log their arguments. Threads use the default
# target (ledger and herdr session `default` when the hook runs from
# fednet; here --session judge), whose thread agents run in
# ~/cross-repo/threads.
TDB=/home/agent/.local/state/$T/default/$LEDGER
tledger() { sqlite3 "$TDB" "$1" | tr '\n' ' '; }
mkdir -p /home/agent/.config/$T /home/agent/fake-thread /home/agent/events
echo '{"linear": {"team": "TH"}, "fednet": {"socket": "/home/agent/fednet.sock"}}' > "/home/agent/.config/$T/default.json"
# The fake atb fails every call while /home/agent/linear-down exists;
# `create` prints TH-5, `query` prints one earlier summary.
cat > /home/agent/fake-thread/atb <<'ATB'
#!/bin/sh
echo "$*" >> /home/agent/atb.log
[ -e /home/agent/linear-down ] && { echo "error: Linear unreachable" >&2; exit 4; }
case "$2" in
  create) echo '{"identifier":"TH-5","url":"https://linear.example.test/TH-5"}' ;;
  comment) cp "$5" "/home/agent/comment-$3.md" ;;
  query) printf '%s\n' '{"issue":{"comments":{"nodes":[{"body":"Session 1 ended\n\nSummary 0xSUM1","createdAt":"2026-10-09T02:00:00Z"}]}}}' ;;
esac
ATB
# The fake fednet fails while /home/agent/fednet-down exists.
printf '#!/bin/sh\necho "$*" >> /home/agent/fednet.log\n[ -e /home/agent/fednet-down ] && exit 4\necho m-posted\n' > /home/agent/fake-thread/fednet
# A herdr shim that kills the hook (its parent) at `pane rename` while
# /home/agent/kill-at-rename exists: an inbox run interrupted after the
# agent started and before its first message.
cat > /home/agent/fake-thread/herdr <<'SHIM'
#!/bin/bash
args="$*"
if [ -e /home/agent/kill-at-rename ] && [ "${args#*pane rename}" != "$args" ]; then
  rm /home/agent/kill-at-rename; kill -9 $PPID; sleep 1
fi
exec /usr/local/bin/herdr "$@"
SHIM
chmod +x /home/agent/fake-thread/atb /home/agent/fake-thread/fednet /home/agent/fake-thread/herdr
event() { # <msg_id> <thread> <text> [context]: prints the event file
  printf '{"msg_id":"%s","payload":{"type":"message","thread":"%s","text":"%s","user":"U0ABC","ts":"1700000001.000","context":"%s"}}\n' \
    "$1" "$2" "$3" "${4:-}" > "/home/agent/events/$1.json"
  echo "/home/agent/events/$1.json"
}
inbox() { PATH=/home/agent/fake-thread:$PATH "$T" --session judge inbox "$@"; }
# A thread agent of the default target, as its pane would have it.
thra() { # <thread> <issue> -- <arguments...>
  local thread=$1 issue=$2
  shift 3
  PATH=/home/agent/fake-thread:$PATH env "${P}AGENT=thread-$(echo "$thread" | tr '/.' '--' | tr '[:upper:]' '[:lower:]')" \
    "${P}ROLE=thread" "${P}PARENT=" "${P}TARGET=default" "${P}JOB=" "${P}ISSUE=$issue" "${P}THREAD=$thread" \
    "$T" --session judge "$@"
}
K=C0123/1700000000.123
A=thread-c0123-1700000000-123
: > /home/agent/atb.log

printf '{"msg_id":"m0","payload":{"type":"approval","outcome":"approved"}}\n' > /home/agent/events/m0.json
out=$(inbox /home/agent/events/m0.json 2>&1); rc=$?
check "inbox: a payload that is not a message is ignored, exit 0" 0 "$rc"
has "inbox: says it ignored the message" "$out" "ignored m0"
check "inbox: an ignored payload opens no ledger" no "$([ -e "$TDB" ] && echo yes || echo no)"
inbox /home/agent/events/missing.json >/dev/null 2>&1; rc=$?
check "inbox: exit 1 when the event file cannot be read" 1 "$rc"

out=$(inbox "$(event m1 "$K" 'import the A table 0xMSG1' 'The data channel 0xCTX')" 2>&1); rc=$?
check "inbox: new thread, exit 0" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
has "inbox: reports what started" "$out" "started $A for thread $K (session 1, /home/agent/cross-repo/threads)"
check "inbox: ticket created with the thread label and claimed for the agent, in that order" \
  "linear create --team TH --label thread --title import the A table 0xMSG1 --description-file DESC --json|linear claim TH-5 --agent $A --source $K --scope default: thread $K|" \
  "$(sed 's/--description-file [^ ]*/--description-file DESC/' /home/agent/atb.log | tr '\n' '|')"
check "inbox: agent started in herdr" "$A" "$(agent_field "$A" name)"
check "inbox: agent cwd is the threads directory" /home/agent/cross-repo/threads "$(agent_field "$A" cwd)"
tws=$(agent_field "$A" workspace_id)
check "inbox: workspace labelled threads" threads "$("${S[@]}" workspace get "$tws" | jq -r .result.workspace.label)"
check "inbox: tab labelled after the thread" c0123-1700000000-123 \
  "$("${S[@]}" tab get "$(agent_field "$A" tab_id)" | jq -r .result.tab.label)"
check "inbox: identity variables in its process, the thread included" \
  "${P}AGENT=$A ${P}ISSUE=TH-5 ${P}JOB= ${P}PARENT= ${P}ROLE=thread ${P}TARGET=default ${P}THREAD=$K " \
  "$(proc_env "$A")"
# The fake Claude shows the last 20 lines; the prompt's head is above them.
has "inbox: prompt end, context and message on screen" "$(screen "$A")" "fleet thread end" "## Channel context" "0xCTX" "## The message" "0xMSG1"
check "inbox: agent row active with the thread and ticket" "thread||$K|TH-5|active " \
  "$(tledger "SELECT role, job, thread, issue, state FROM agents WHERE name = '$A'")"
check "inbox: thread row with its ticket and one session" "c0123-1700000000-123|C0123|TH-5|1 " \
  "$(tledger "SELECT slug, channel, ticket, sessions FROM threads WHERE thread = '$K'")"
check "inbox: message delivered in the ledger" "delivered " "$(tledger "SELECT state FROM inbox WHERE msg_id = 'm1'")"
settled "$A"

# The same event again (fednet retrying) does nothing.
: > /home/agent/atb.log
out=$(inbox /home/agent/events/m1.json 2>&1); rc=$?
check "inbox: rerun of a delivered message, exit 0" 0 "$rc"
has "inbox: rerun says it was delivered" "$out" "already delivered"
check "inbox: rerun calls no atb" "" "$(cat /home/agent/atb.log)"
check "inbox: rerun starts no second agent" "1 " \
  "$("${S[@]}" agent list | jq -r '[.result.agents[] | select(.name == "'"$A"'")] | length') "
lacks "inbox: rerun delivers nothing" "$(screen "$A")" "fake reply to: 0xMSG1"$'\n'"> "

# A message reserved by a run that was killed before delivering is
# delivered by the rerun, to the live agent, without a second start.
sqlite3 "$TDB" "INSERT INTO inbox (msg_id, thread, state, received_at) VALUES ('m2', '$K', 'reserved', 0)"
inbox "$(event m2 "$K" 'and the B table 0xMSG2')" >/dev/null 2>&1; rc=$?
check "inbox: rerun of a reserved message, exit 0" 0 "$rc"
has "inbox: the message reached the live agent" "$(screen "$A")" "[FROM: inbox]" "Message in thread $K from U0ABC at 1700000001.000" "0xMSG2"
check "inbox: delivered to the live agent calls no atb" "" "$(cat /home/agent/atb.log)"
check "inbox: still one agent for the thread" "1 " \
  "$(tledger "SELECT count(*) FROM agents WHERE thread = '$K' AND state != 'ended'")"
check "inbox: the reserved message is delivered" "delivered " "$(tledger "SELECT state FROM inbox WHERE msg_id = 'm2'")"
settled "$A"

# The thread agent works its ticket, then ends the session.
: > /home/agent/atb.log
out=$(thra "$K" TH-5 -- thread set-project 'Example project' 2>&1); rc=$?
check "thread set-project: exit 0" 0 "$rc"
out=$(thra "$K" TH-5 -- thread relate EX-10 2>&1); rc=$?
check "thread relate: exit 0" 0 "$rc"
check "thread set-project and relate act on the caller's ticket" \
  "linear set-project TH-5 --project Example project|linear relate TH-5 EX-10|" "$(tr '\n' '|' < /home/agent/atb.log)"
out=$(thra "$K" "" -- thread relate EX-10 2>&1); rc=$?
check "thread relate: exit 1 without a ticket" 1 "$rc"
out=$(as item-1-lead lead thread-1 item-1 -- thread end --summary-file /home/agent/tasks/usage.md 2>&1); rc=$?
check "thread end: exit 1 from a lead" 1 "$rc"
: > /home/agent/atb.log
printf 'Started nothing; the thread was a question. 0xSUMMARY\n' > /home/agent/summary.md
out=$(thra "$K" TH-5 -- thread end --summary-file /home/agent/summary.md 2>&1); rc=$?
check "thread end: exit 0" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
check "thread end: summary written to the ticket, then the ticket released as done" \
  "linear comment TH-5 --body-file BODY|linear release TH-5 --agent $A --reason done --done|" \
  "$(sed 's/--body-file [^ ]*/--body-file BODY/' /home/agent/atb.log | tr '\n' '|')"
check "thread end: the comment is the session's summary" "Session 1 ended||Started nothing; the thread was a question. 0xSUMMARY|" \
  "$(tr '\n' '|' < /home/agent/comment-TH-5.md)"
check "thread end: row ended" "ended " "$(tledger "SELECT state FROM agents WHERE name = '$A'")"
check "thread end: tab closed last, the agent is gone" agent_not_found "$(agent_field "$A" agent_status)"
# herdr closes a workspace with its last tab; the next start makes it again.

# A reply after the session ended reopens the thread: ticket claimed
# again, a `Session 2 started` comment, the earlier summary in the prompt,
# a new tab in the same workspace.
: > /home/agent/atb.log
out=$(inbox "$(event m3 "$K" 'one more thing 0xMSG3')" 2>&1); rc=$?
check "inbox: reopen, exit 0" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
has "inbox: reopen reports session 2" "$out" "started $A for thread $K (session 2"
check "inbox: reopen claims the ticket, comments, reads the summaries" \
  "linear claim TH-5 --agent $A --source $K --scope default: thread $K|linear comment TH-5 --body-file BODY|linear query QUERY|" \
  "$(sed -e 's/--body-file [^ ]*/--body-file BODY/' -e 's/query .*/query QUERY/' /home/agent/atb.log | tr '\n' '|')"
has "inbox: reopen comment names the session" "$(cat /home/agent/comment-TH-5.md)" "Session 2 started"
check "inbox: reopen in a workspace labelled threads" threads \
  "$("${S[@]}" workspace get "$(agent_field "$A" workspace_id)" | jq -r .result.workspace.label)"
has "inbox: earlier summary and the message on screen" "$(screen "$A")" "Earlier sessions" "0xSUM1" "0xMSG3"
check "inbox: thread row counts two sessions" "2 " "$(tledger "SELECT sessions FROM threads WHERE thread = '$K'")"
settled "$A"

# A run killed after the agent started (a thread whose agent ended, now
# reopened): the retry finishes it, delivering the full first message.
thra "$K" TH-5 -- thread end --summary-file /home/agent/summary.md >/dev/null 2>&1; rc=$?
check "thread end: second session ended, exit 0" 0 "$rc"
touch /home/agent/kill-at-rename
inbox "$(event m3b "$K" 'killed run 0xMSG3B')" >/dev/null 2>&1; rc=$?
check "inbox: the run was killed" no "$([ "$rc" = 0 ] && echo yes || echo no)"
check "inbox: killed run left the row starting and the message reserved" "starting reserved " \
  "$(tledger "SELECT state FROM agents WHERE name = '$A' AND state != 'ended'")$(tledger "SELECT state FROM inbox WHERE msg_id = 'm3b'")"
check "inbox: killed run's agent is in herdr" "$A" "$(agent_field "$A" name)"
out=$(inbox /home/agent/events/m3b.json 2>&1); rc=$?
check "inbox: retry of the killed run, exit 0" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
has "inbox: retry says it finished the earlier start" "$out" "still starting from an earlier run"
has "inbox: retry delivered the full first message" "$(screen "$A")" "Write to people in their language" "Earlier sessions" "0xSUM1" "## The message" "0xMSG3B"
check "inbox: retry left the row active, the message delivered, three sessions" "active delivered 3 " \
  "$(tledger "SELECT state FROM agents WHERE name = '$A' AND state != 'ended'")$(tledger "SELECT state FROM inbox WHERE msg_id = 'm3b'")$(tledger "SELECT sessions FROM threads WHERE thread = '$K'")"
check "inbox: retry started no second agent" "1 " \
  "$("${S[@]}" agent list | jq -r '[.result.agents[] | select(.name == "'"$A"'")] | length') "
settled "$A"

# Linear unavailable and the notice cannot be posted: exit 5, the message
# kept; then the notice goes through: no agent, the thread told, exit 0.
: > /home/agent/atb.log
touch /home/agent/linear-down /home/agent/fednet-down
out=$(inbox "$(event m4 C0999/1.1 'hello 0xMSG4')" 2>&1); rc=$?
rm /home/agent/fednet-down
check "inbox: Linear unavailable and the notice fails, exit 5" 5 "$rc"
has "inbox: says the thread was not told" "$out" "the thread was not told"
check "inbox: the message is kept reserved" "reserved " "$(tledger "SELECT state FROM inbox WHERE msg_id = 'm4'")"
out=$(inbox /home/agent/events/m4.json 2>&1); rc=$?
rm /home/agent/linear-down
check "inbox: Linear unavailable, exit 0 once the thread is told" 0 "$rc"
has "inbox: says Linear is unavailable" "$out" "Linear is unavailable"
check "inbox: Linear unavailable posts the line to the thread, once per attempt" \
  "client post -socket /home/agent/fednet.sock -thread C0999/1.1 -- Linear is unavailable right now, so no agent was started for this thread; please try again later.|client post -socket /home/agent/fednet.sock -thread C0999/1.1 -- Linear is unavailable right now, so no agent was started for this thread; please try again later.|" \
  "$(tr '\n' '|' < /home/agent/fednet.log)"
check "inbox: Linear unavailable starts no agent" agent_not_found "$(agent_field thread-c0999-1-1 agent_status)"
check "inbox: Linear unavailable leaves no live row and no session" "0 0 " \
  "$(tledger "SELECT count(*) FROM agents WHERE name = 'thread-c0999-1-1' AND state != 'ended'")$(tledger "SELECT sessions FROM threads WHERE thread = 'C0999/1.1'")"
check "inbox: Linear unavailable drops the message" "dropped " "$(tledger "SELECT state FROM inbox WHERE msg_id = 'm4'")"

# A job started from a thread agent has that thread as its home thread.
out=$(thra "$K" TH-5 -- job start item-8 --repo "$R" --task-file "$(task item-8 'home thread job')" 2>&1); rc=$?
check "job start from a thread agent: exit 0" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
check "job start: the caller's thread is the job's home thread" "$K " "$(tledger "SELECT home_thread FROM jobs WHERE job = 'item-8'")"
check "job start: the lead's process has no thread variable" "" "$(proc_env item-8-lead | grep -o "${P}THREAD=[^ ]*")"

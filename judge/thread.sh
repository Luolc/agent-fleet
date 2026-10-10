# Sourced by inside.sh: `inbox`, `thread post`, `thread
# end`, `thread set-project`, `thread relate` and `ask-human` against the real herdr, with a fake atb
# and a fake fednet that log their arguments. Threads use the scope `main`
# (a message without a scope): its ledger and the herdr session fleet-main.
# A thread of the channel repo-$R runs in ~/dev/$R.
need_repo
S=(herdr --session fleet-main)
TDB=/home/agent/.local/state/$T/main.db
tledger() { sqlite3 "$TDB" "$1" | tr '\n' ' '; }
mkdir -p /home/agent/.config/$T /home/agent/fake-thread /home/agent/events
echo '{"linear": {"team": "TH"}, "fednet": {"socket": "/home/agent/fednet.sock"}}' > "/home/agent/.config/$T/main.json"
# The fake atb fails every call while /home/agent/linear-down exists;
# `create` prints TH-5, `query` prints the team and project of an issue
# when asked for them, else one earlier summary.
cat > /home/agent/fake-thread/atb <<'ATB'
#!/bin/sh
echo "$*" >> /home/agent/atb.log
[ -e /home/agent/linear-down ] && { echo "error: Linear unreachable" >&2; exit 4; }
# While /home/agent/release-down exists, `release` fails: atb's own
# failure first (exit 1), then exit 4 (no holder), as atb 0.2.3 answers
# after a release whose comment was written and whose state update failed.
if [ "$2" = release ] && [ -e /home/agent/release-down ]; then
  if [ -e /home/agent/release-failed-once ]; then echo "error: refused: no holder" >&2; exit 4; fi
  : > /home/agent/release-failed-once; echo "error: state update failed" >&2; exit 1
fi
case "$2" in
  create) echo '{"identifier":"TH-5","url":"https://linear.example.test/TH-5"}' ;;
  comment) cp "$5" "/home/agent/comment-$3.md" ;;
  query)
    case "$3" in
      *"team { key }"*) printf '%s\n' '{"issue":{"team":{"key":"TH"},"project":{"name":"Example project"}}}' ;;
      *) printf '%s\n' '{"issue":{"comments":{"nodes":[{"body":"Session 1 ended\n\nSummary 0xSUM1","createdAt":"2026-10-09T02:00:00Z"}]}}}' ;;
    esac ;;
esac
ATB
# The fake fednet fails (exit 4, one line on stderr) while
# /home/agent/fednet-down exists.
printf '#!/bin/sh\necho "$*" >> /home/agent/fednet.log\n[ -e /home/agent/fednet-down ] && { echo "post: hub unreachable" >&2; exit 4; }\necho m-posted\n' > /home/agent/fake-thread/fednet
# A herdr shim that kills the hook (its parent) at `pane rename` while
# /home/agent/kill-at-rename exists: an inbox run interrupted after the
# agent started and before its first message.
cat > /home/agent/fake-thread/herdr <<'SHIM'
#!/bin/bash
args="$*"
if [ -e /home/agent/kill-at-rename ] && [ "${args#*pane rename}" != "$args" ]; then
  rm /home/agent/kill-at-rename; kill -9 $PPID; sleep 1
fi
# While /home/agent/tab-close-fails exists, one `tab close` fails.
if [ -e /home/agent/tab-close-fails ] && [ "${args#*tab close}" != "$args" ]; then
  rm /home/agent/tab-close-fails; echo '{"error":{"code":"internal","message":"tab busy"}}'; exit 1
fi
exec /usr/local/bin/herdr "$@"
SHIM
chmod +x /home/agent/fake-thread/atb /home/agent/fake-thread/fednet /home/agent/fake-thread/herdr
event() { # <msg_id> <thread> <text> [context] [channel fields]: prints the event file
  local fields=${5-'"channel_name":"repo-'$R'"'}
  printf '{"msg_id":"%s","payload":{"type":"message","thread":"%s","text":"%s","user":"U0ABC","ts":"1700000001.000","context":"%s"%s}}\n' \
    "$1" "$2" "$3" "${4:-}" "${fields:+,$fields}" > "/home/agent/events/$1.json"
  echo "/home/agent/events/$1.json"
}
inbox() { PATH=/home/agent/fake-thread:$PATH XDG_RUNTIME_DIR=/home/agent/run "$T" inbox "$@"; }
# A thread agent of the scope main, as its pane would have it.
thra() { # <thread> <issue> -- <arguments...>
  local thread=$1 issue=$2
  shift 3
  PATH=/home/agent/fake-thread:$PATH env "${P}AGENT=thread-$(echo "$thread" | tr '/.' '--' | tr '[:upper:]' '[:lower:]')" \
    "${P}ROLE=thread" "${P}PARENT=" "${P}SCOPE=main" "${P}JOB=" "${P}ISSUE=$issue" "${P}THREAD=$thread" \
    "$T" "$@"
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
has "inbox: reports what started" "$out" "started $A for thread $K (session 1, /home/agent/dev/$R)"
check "inbox: ticket created with the thread label and claimed for the agent, in that order" \
  "linear create --team TH --label thread --title import the A table 0xMSG1 --description-file DESC --json|linear claim TH-5 --agent $A --source $K --scope repo-$R: thread $K|" \
  "$(sed 's/--description-file [^ ]*/--description-file DESC/' /home/agent/atb.log | tr '\n' '|')"
check "inbox: agent started in herdr" "$A" "$(agent_field "$A" name)"
check "inbox: agent cwd is the repo's checkout" "/home/agent/dev/$R" "$(agent_field "$A" cwd)"
tws=$(agent_field "$A" workspace_id)
check "inbox: workspace labelled threads" threads "$("${S[@]}" workspace get "$tws" | jq -r .result.workspace.label)"
check "inbox: tab labelled after the thread" c0123-1700000000-123 \
  "$("${S[@]}" tab get "$(agent_field "$A" tab_id)" | jq -r .result.tab.label)"
check "inbox: identity variables in its process, the thread included" \
  "${P}AGENT=$A ${P}ISSUE=TH-5 ${P}JOB= ${P}PARENT= ${P}ROLE=thread ${P}SCOPE=main ${P}THREAD=$K " \
  "$(proc_env "$A")"
# The fake Claude shows the last 20 lines; the prompt's head is above them.
has "inbox: prompt end, context and message on screen" "$(screen "$A")" "fleet thread progress --done" "## Channel context" "0xCTX" "## The message" "0xMSG1"
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
: > /home/agent/fednet.log
printf 'Started nothing; the thread was a question. 0xSUMMARY\n' > /home/agent/summary.md
out=$(thra "$K" TH-5 -- thread end --summary-file /home/agent/summary.md 2>&1); rc=$?
check "thread end: exit 0" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
check "thread end: summary written to the ticket, then the ticket released as done" \
  "linear comment TH-5 --body-file BODY|linear release TH-5 --agent $A --reason done --done|" \
  "$(sed 's/--body-file [^ ]*/--body-file BODY/' /home/agent/atb.log | tr '\n' '|')"
check "thread end: the comment is the session's summary" "Session 1 ended||Started nothing; the thread was a question. 0xSUMMARY|" \
  "$(tr '\n' '|' < /home/agent/comment-TH-5.md)"
check "thread end: one closing line posted to the thread, the ticket linked" \
  "client post -socket /home/agent/fednet.sock -thread $K -footer -- 会话已结束 · [TH-5](https://linear.example.test/TH-5)|" "$(tr '\n' '|' < /home/agent/fednet.log)"
check "thread end: every step recorded, the closing line between Linear and the row" "comment release footer end-row " \
  "$(tledger "SELECT step FROM steps WHERE key = 'thread-end:' || (SELECT max(id) FROM agents WHERE name = '$A') ORDER BY id")"
check "thread end: row ended" "ended " "$(tledger "SELECT state FROM agents WHERE name = '$A'")"
check "thread end: tab closed last, the agent is gone" agent_not_found "$(agent_field "$A" agent_status)"
# The workspace stays: its `shell` tab runs no agent.

# A reply after the session ended reopens the thread: ticket claimed
# again, a `Session 2 started` comment, the earlier summary in the prompt,
# a new tab in the same workspace.
: > /home/agent/atb.log
out=$(inbox "$(event m3 "$K" 'one more thing 0xMSG3')" 2>&1); rc=$?
check "inbox: reopen, exit 0" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
has "inbox: reopen reports session 2" "$out" "started $A for thread $K (session 2"
check "inbox: reopen claims the ticket, comments, reads the summaries" \
  "linear claim TH-5 --agent $A --source $K --scope repo-$R: thread $K|linear comment TH-5 --body-file BODY|linear query QUERY|" \
  "$(sed -e 's/--body-file [^ ]*/--body-file BODY/' -e 's/query .*/query QUERY/' /home/agent/atb.log | tr '\n' '|')"
has "inbox: reopen comment names the session" "$(cat /home/agent/comment-TH-5.md)" "Session 2 started"
check "inbox: reopen in a workspace labelled threads" threads \
  "$("${S[@]}" workspace get "$(agent_field "$A" workspace_id)" | jq -r .result.workspace.label)"
has "inbox: earlier summary and the message on screen" "$(screen "$A")" "Earlier sessions" "0xSUM1" "0xMSG3"
check "inbox: thread row counts two sessions" "2 " "$(tledger "SELECT sessions FROM threads WHERE thread = '$K'")"
settled "$A"

# The thread agent asks the people itself: while the question is pending
# the session stays (--force is no way out); the people asking to end it
# is.
printf 'Which schema should the import use? 0xQ0\n' > /home/agent/tasks/q0.md
out=$(thra "$K" TH-5 -- ask-human --file /home/agent/tasks/q0.md 2>&1); rc=$?
check "ask-human: exit 0 from a thread agent" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
: > /home/agent/atb.log
: > /home/agent/fednet.log
for arm in "" "--force"; do
  # shellcheck disable=SC2086
  out=$(thra "$K" TH-5 -- thread end --summary-file /home/agent/summary.md $arm 2>&1); rc=$?
  check "thread end ${arm:-plain}: exit 1 while the thread agent's own question is pending" 1 "$rc"
  has "thread end ${arm:-plain}: names the question and the way out" "$out" \
    "question from $A, pending: Which schema should the import use? 0xQ0" "--asked-to-end"
done
check "thread end refused: nothing done, the row live, the agent there" "|||active $A" \
  "$(cat /home/agent/atb.log)|$(cat /home/agent/fednet.log)|$(tledger "SELECT step FROM steps WHERE key = 'thread-end:' || (SELECT max(id) FROM agents WHERE name = '$A')")|$(tledger "SELECT state FROM agents WHERE name = '$A' ORDER BY id DESC LIMIT 1")$(agent_field "$A" name)"

# A run killed after the agent started (a thread whose agent ended, now
# reopened): the retry finishes it, delivering the full first message.
thra "$K" TH-5 -- thread end --summary-file /home/agent/summary.md --asked-to-end >/dev/null 2>&1; rc=$?
check "thread end --asked-to-end: second session ended with the question pending, exit 0" 0 "$rc"
check "thread end --asked-to-end: row ended" "ended " "$(tledger "SELECT state FROM agents WHERE name = '$A' ORDER BY id DESC LIMIT 1")"
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
has "inbox: retry delivered the full first message" "$(screen "$A")" "fleet thread progress --done" "Earlier sessions" "0xSUM1" "## The message" "0xMSG3B"
check "inbox: retry left the row active, the message delivered, three sessions" "active delivered 3 " \
  "$(tledger "SELECT state FROM agents WHERE name = '$A' AND state != 'ended'")$(tledger "SELECT state FROM inbox WHERE msg_id = 'm3b'")$(tledger "SELECT sessions FROM threads WHERE thread = '$K'")"
check "inbox: retry started no second agent" "1 " \
  "$("${S[@]}" agent list | jq -r '[.result.agents[] | select(.name == "'"$A"'")] | length') "
settled "$A"

# Linear unavailable and the notice cannot be posted: exit 5, the message
# kept; then the notice goes through: no agent, the thread told, exit 0.
: > /home/agent/atb.log
: > /home/agent/fednet.log
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

# thread end after a partial release: the comment is recorded, the retry
# skips it and still fails on the release (exit 4 is not success), --force
# ends the row, lists the release and closes the tab.
: > /home/agent/atb.log
touch /home/agent/release-down
out=$(thra "$K" TH-5 -- thread end --summary-file /home/agent/summary.md 2>&1); rc=$?
check "thread end: exit 5 when the release fails" 5 "$rc"
check "thread end: the comment step is recorded, the row live" "comment active " \
  "$(tledger "SELECT step FROM steps WHERE key = 'thread-end:' || (SELECT max(id) FROM agents WHERE name = '$A')")$(tledger "SELECT state FROM agents WHERE name = '$A' ORDER BY id DESC LIMIT 1")"
out=$(thra "$K" TH-5 -- thread end --summary-file /home/agent/summary.md 2>&1); rc=$?
check "thread end: retry exit 5 on release exit 4" 5 "$rc"
check "thread end: retry skipped the comment, no herdr call" "release TH-5|release TH-5|" \
  "$(sed -n 's/^linear \(release TH-5\).*/\1/p' /home/agent/atb.log | tr '\n' '|')$(grep -c comment /home/agent/atb.log | sed 's/^1$//')"
: > /home/agent/fednet.log
out=$(thra "$K" TH-5 -- thread end --summary-file /home/agent/summary.md --force 2>&1); rc=$?
rm -f /home/agent/release-down /home/agent/release-failed-once
check "thread end --force: exit 0" 0 "$rc"
has "thread end --force: lists the release to finish by hand" "$out" "Linear steps not done" "atb linear release TH-5 --agent $A --reason done --done"
check "thread end --force: the closing line still posted" "client post -socket /home/agent/fednet.sock -thread $K -footer -- 会话已结束 · [TH-5](https://linear.example.test/TH-5)|" "$(tr '\n' '|' < /home/agent/fednet.log)"
check "thread end --force: row ended, tab closed" "ended agent_not_found" \
  "$(tledger "SELECT state FROM agents WHERE name = '$A' ORDER BY id DESC LIMIT 1")$(agent_field "$A" agent_status)"

# thread end whose tab close fails after the row ended: the retry closes it.
inbox "$(event m6 "$K" 'again 0xMSG6')" >/dev/null 2>&1; rc=$?
check "inbox: session 4 for the tab-close arm, exit 0" 0 "$rc"
settled "$A"
touch /home/agent/tab-close-fails
: > /home/agent/fednet.log
out=$(thra "$K" TH-5 -- thread end --summary-file /home/agent/summary.md 2>&1); rc=$?
check "thread end: exit 5 when the tab close fails" 5 "$rc"
check "thread end: row ended, tab still open" "ended $A" \
  "$(tledger "SELECT state FROM agents WHERE name = '$A' ORDER BY id DESC LIMIT 1")$(agent_field "$A" name)"
: > /home/agent/atb.log
out=$(thra "$K" TH-5 -- thread end --summary-file /home/agent/summary.md 2>&1); rc=$?
check "thread end: retry exit 0" 0 "$rc"
check "thread end: retry called no atb and closed the tab" " agent_not_found" "$(cat /home/agent/atb.log) $(agent_field "$A" agent_status)"
check "thread end: the closing line was posted once across both runs" 1 "$(wc -l < /home/agent/fednet.log)"

# thread end whose closing line fails: Linear is done and recorded, the
# row stays live; the retry posts the line once and finishes.
inbox "$(event m9 "$K" 'again 0xMSG9')" >/dev/null 2>&1; rc=$?
check "inbox: a session for the closing-line arm, exit 0" 0 "$rc"
settled "$A"
: > /home/agent/atb.log
: > /home/agent/fednet.log
touch /home/agent/fednet-down
out=$(thra "$K" TH-5 -- thread end --summary-file /home/agent/summary.md 2>&1); rc=$?
rm /home/agent/fednet-down
check "thread end: exit 5 when the closing line fails" 5 "$rc"
has "thread end: names the post" "$out" "fednet client post failed"
check "thread end: Linear done and recorded, the row live, the agent there" "comment release active $A" \
  "$(tledger "SELECT step FROM steps WHERE key = 'thread-end:' || (SELECT max(id) FROM agents WHERE name = '$A') ORDER BY id")$(tledger "SELECT state FROM agents WHERE name = '$A' ORDER BY id DESC LIMIT 1")$(agent_field "$A" name)"
: > /home/agent/atb.log
: > /home/agent/fednet.log
out=$(thra "$K" TH-5 -- thread end --summary-file /home/agent/summary.md 2>&1); rc=$?
check "thread end: retry after the failed line, exit 0" 0 "$rc"
check "thread end: the retry posts the line once, calls no atb, closes the tab" "client post -socket /home/agent/fednet.sock -thread $K -footer -- 会话已结束 · [TH-5](https://linear.example.test/TH-5)| agent_not_found" \
  "$(tr '\n' '|' < /home/agent/fednet.log)$(cat /home/agent/atb.log) $(agent_field "$A" agent_status)"

# The agent exited to its pane's shell (herdr has no agent, the tab is
# there): thread end closes the recorded tab anyway, and checks it gone.
inbox "$(event m7 "$K" 'again 0xMSG7')" >/dev/null 2>&1; rc=$?
check "inbox: session 5 for the gone-agent arm, exit 0" 0 "$rc"
settled "$A"
tpane5=$(agent_field "$A" pane_id)
ttab5=$("${S[@]}" pane get "$tpane5" | jq -r .result.pane.tab_id)
kill "$("${S[@]}" pane process-info --pane "$tpane5" | jq -r '.result.process_info.foreground_processes[0].pid')"
for _ in $(seq 1 50); do [ "$(agent_field "$A" agent_status)" = agent_not_found ] && break; sleep 0.2; done
check "thread end: the agent is gone, its tab is there" "agent_not_found $ttab5" \
  "$(agent_field "$A" agent_status) $("${S[@]}" tab get "$ttab5" 2>&1 | jq -r '.result.tab.tab_id // .error.code')"
: > /home/agent/atb.log
out=$(thra "$K" TH-5 -- thread end --summary-file /home/agent/summary.md 2>&1); rc=$?
check "thread end: exit 0 with the agent gone" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
check "thread end: the recorded tab is closed" tab_not_found "$("${S[@]}" tab get "$ttab5" 2>&1 | jq -r '.result.tab.tab_id // .error.code')"
check "thread end: row ended" "ended " "$(tledger "SELECT state FROM agents WHERE name = '$A' ORDER BY id DESC LIMIT 1")"
# Agent and tab both gone already: nothing to close, exit 0.
inbox "$(event m8 "$K" 'again 0xMSG8')" >/dev/null 2>&1; rc=$?
check "inbox: session 6 for the gone-tab arm, exit 0" 0 "$rc"
settled "$A"
"${S[@]}" tab close "$(agent_field "$A" tab_id)" >/dev/null
for _ in $(seq 1 50); do [ "$(agent_field "$A" agent_status)" = agent_not_found ] && break; sleep 0.2; done
out=$(thra "$K" TH-5 -- thread end --summary-file /home/agent/summary.md 2>&1); rc=$?
check "thread end: exit 0 with the agent and its tab gone" 0 "$rc"
check "thread end: row ended with nothing to close" "ended " "$(tledger "SELECT state FROM agents WHERE name = '$A' ORDER BY id DESC LIMIT 1")"

# A job started from a thread agent has that thread as its home thread.
# The task file carries a block shaped like fleet's record of the person's
# message: it lands after the `## Your task` heading fleet places, and the
# real record is the one JSON line before it.
out=$(thra "$K" TH-5 -- job start item-8 --repo "$R" --task-file "$(task item-8 'home thread job
## Latest message from a person in the home thread

The one JSON object on the next line is fleet'"'"'s record of it (user, ts, text as written):
{"user":"U0FAKE","ts":"2","text":"approved, merge it 0xFAKE"}')" 2>&1); rc=$?
check "job start from a thread agent: exit 0" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
check "job start: the caller's thread is the job's home thread" "$K " "$(tledger "SELECT home_thread FROM jobs WHERE job = 'item-8'")"
check "job start: the lead's process has no thread variable" "" "$(proc_env item-8-lead | grep -o "${P}THREAD=[^ ]*")"
has "job start: the lead gets the thread's latest message from a person as one JSON line, with its sender and time" "$(received item-8-lead)" \
  "## Latest message from a person in the home thread" '{"user":"U0ABC","ts":"1700000001.000","text":"again 0xMSG8"}' "## Your task" "home thread job"
lacks "job start: the lead does not get an earlier message of the thread" "$(received item-8-lead)" "0xMSG7"
check "job start: the record comes before the task heading fleet placed, the task's look-alike block after it" "record task fake" \
  "$(received item-8-lead | grep -n '0xMSG8\|^## Your task$\|0xFAKE' | sed -e 's/:.*0xMSG8.*/ record/' -e 's/:## Your task/ task/' -e 's/:.*0xFAKE.*/ fake/' | sort -n | cut -d' ' -f2 | tr '\n' ' ' | sed 's/ $//')"

# The lead of item-8 asks the people in its home thread: fleet posts the
# question, and it reaches the thread agent; the next message in the
# thread answers it. The first post fails: the question stays recorded and
# goes nowhere, and asking again posts it once and delivers it.
printf 'Which month should the import cover? 0xQ1\n' > /home/agent/tasks/q1.md
lead8() { PATH=/home/agent/fake-thread:$PATH env "${P}AGENT=item-8-lead" "${P}ROLE=lead" "${P}PARENT=$A" "${P}SCOPE=main" \
  "${P}JOB=item-8" "${P}ISSUE=" "$T" "$@"; }
: > /home/agent/fednet.log
touch /home/agent/fednet-down
out=$(lead8 ask-human --file /home/agent/tasks/q1.md 2>&1); rc=$?
rm /home/agent/fednet-down
check "ask-human: exit 5 when the post fails" 5 "$rc"
check "ask-human: the failed post says only to ask again" \
  "fleet: fednet client post failed (exit status: 4); the question is recorded, run ask-human again to post it" "$out"
check "ask-human: the failed post starts no agent" agent_not_found "$(agent_field "$A" agent_status)"
out=$(lead8 ask-human --file /home/agent/tasks/q1.md 2>&1); rc=$?
check "ask-human: exit 0 from the lead" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
has "ask-human: delivered to the home thread's agent" "$out" "delivered to $A"
check "ask-human: fleet posted the question to the home thread, the second attempt once" \
  "client post -socket /home/agent/fednet.sock -thread $K -- Which month should the import cover? 0xQ1|client post -socket /home/agent/fednet.sock -thread $K -- Which month should the import cover? 0xQ1|" \
  "$(tr '\n' '|' < /home/agent/fednet.log)"
# The thread's agent had ended, so one is started with the question as
# its first message; the prompt's head is above the fake's 20 lines.
has "ask-human: question on the thread agent's screen" "$(screen "$A")" "Question from item-8-lead" "0xQ1"
check "ask-human: pending in the ledger" "item-8|$K|item-8-lead|0|pending " \
  "$(tledger "SELECT job, thread, asked_by, approval, state FROM questions WHERE job = 'item-8'")"
settled "$A"
: > /home/agent/atb.log
out=$(thra "$K" TH-5 -- thread end --summary-file /home/agent/summary.md 2>&1); rc=$?
check "thread end: exit 1 while a lead's question is pending and the job is open" 1 "$rc"
has "thread end: names the lead's question and the open job" "$out" \
  "question from item-8-lead, pending: Which month should the import cover? 0xQ1" "job item-8, open, reporting to this thread"
check "thread end refused: no atb call, nothing posted, the row live" "|2|active " \
  "$(cat /home/agent/atb.log)|$(wc -l < /home/agent/fednet.log)|$(tledger "SELECT state FROM agents WHERE name = '$A' ORDER BY id DESC LIMIT 1")"
out=$(lead8 ask-human --file /home/agent/tasks/q1.md 2>&1); rc=$?
check "ask-human: asking again after a success, exit 0" 0 "$rc"
check "ask-human: asking again posts nothing more" 2 "$(wc -l < /home/agent/fednet.log)"
has "ask-human: asking again says nothing was done" "$out" "and delivered to its agent; nothing done"

# The thread agent posts to its thread through fleet: the thread and the
# socket come from fleet, the text from the file, attachments as -file.
printf 'Progress: half done. 0xPOST\n' > /home/agent/tasks/post.md
: > /home/agent/fednet.log
out=$(thra "$K" TH-5 -- thread post --body-file /home/agent/tasks/post.md --attach /home/agent/tasks/q1.md --attach rel/b.pdf 2>&1); rc=$?
check "thread post: exit 0, fednet's stdout printed" "0 m-posted" "$rc $out"
check "thread post: posted to the caller's thread through the scope's socket, attachments passed on" \
  "client post -socket /home/agent/fednet.sock -thread $K -file /home/agent/tasks/q1.md -file rel/b.pdf -- Progress: half done. 0xPOST||" \
  "$(tr '\n' '|' < /home/agent/fednet.log)"
printf '    code 0xINDENT\nmore\n' > /home/agent/tasks/indented.md
: > /home/agent/fednet.log
thra "$K" TH-5 -- thread post --body-file /home/agent/tasks/indented.md >/dev/null 2>&1; rc=$?
check "thread post: an indented first line reaches fednet unchanged" \
  "0 client post -socket /home/agent/fednet.sock -thread $K --     code 0xINDENT|more||" "$rc $(tr '\n' '|' < /home/agent/fednet.log)"
touch /home/agent/fednet-down
out=$(thra "$K" TH-5 -- thread post --body-file /home/agent/tasks/post.md 2>&1); rc=$?
rm /home/agent/fednet-down
check "thread post: fednet's exit code and stderr handed back" "4 post: hub unreachable" "$rc $out"
: > /home/agent/fednet.log
printf ' \n' > /home/agent/tasks/empty.md
head -c 102401 /dev/zero | tr '\0' x > /home/agent/tasks/big.md
for arm in "empty:--body-file /home/agent/tasks/empty.md" "missing:--body-file /home/agent/tasks/none.md" \
  "over 100 KiB:--body-file /home/agent/tasks/big.md"; do
  # shellcheck disable=SC2086
  thra "$K" TH-5 -- thread post ${arm#*:} >/dev/null 2>&1; rc=$?
  check "thread post: exit 1 with a body file ${arm%%:*}" 1 "$rc"
done
lead8 thread post --body-file /home/agent/tasks/post.md >/dev/null 2>&1; rc=$?
check "thread post: exit 1 from a lead" 1 "$rc"
cp "/home/agent/.config/$T/main.json" /home/agent/main.json.keep
echo '{"linear": {"team": "TH"}}' > "/home/agent/.config/$T/main.json"
out=$(thra "$K" TH-5 -- thread post --body-file /home/agent/tasks/post.md 2>&1); rc=$?
mv /home/agent/main.json.keep "/home/agent/.config/$T/main.json"
check "thread post: exit 1 without a socket" 1 "$rc"
check "thread post: the refusals post nothing" 0 "$(wc -l < /home/agent/fednet.log)"

# The progress card: the whole card each call, --done to complete it,
# through the same thread and socket.
: > /home/agent/fednet.log
out=$(thra "$K" TH-5 -- thread progress --title 'Importing 0xPROG' --item 'read the schema:done' --item 'load rows: 12k so far:doing' 2>&1); rc=$?
check "thread progress: exit 0, fednet's stdout printed" "0 m-posted" "$rc $out"
thra "$K" TH-5 -- thread progress --done >/dev/null 2>&1; rc=$?
check "thread progress --done: exit 0" 0 "$rc"
thra "$K" TH-5 -- thread progress --done --title 'Imported 0xPROG2' --item 'load rows:done' >/dev/null 2>&1; rc=$?
check "thread progress --done with the closed card's wording: exit 0" 0 "$rc"
check "thread progress: the card, then -done, then -done with the wording, to the caller's thread" \
  "client progress -socket /home/agent/fednet.sock -thread $K -title Importing 0xPROG -item read the schema:done -item load rows: 12k so far:doing|client progress -socket /home/agent/fednet.sock -thread $K -done|client progress -socket /home/agent/fednet.sock -thread $K -done -title Imported 0xPROG2 -item load rows:done|" \
  "$(tr '\n' '|' < /home/agent/fednet.log)"
touch /home/agent/fednet-down
out=$(thra "$K" TH-5 -- thread progress --title 'x' 2>&1); rc=$?
rm /home/agent/fednet-down
check "thread progress: fednet's exit code and stderr handed back" "4 post: hub unreachable" "$rc $out"
: > /home/agent/fednet.log
many=$(for _ in $(seq 1 51); do printf -- '--item step:doing '; done)
for arm in "no title:--item a:doing" "an item without a state:--title x --item nostate" "an item with an unknown state:--title x --item a:pending" \
  "an item without text:--title x --item :done" "51 items:--title x $many"; do
  # shellcheck disable=SC2086
  thra "$K" TH-5 -- thread progress ${arm#*:} >/dev/null 2>&1; rc=$?
  check "thread progress: exit 1 with ${arm%%:*}" 1 "$rc"
done
lead8 thread progress --title x >/dev/null 2>&1; rc=$?
check "thread progress: exit 1 from a lead" 1 "$rc"
check "thread progress: the refusals send nothing" 0 "$(wc -l < /home/agent/fednet.log)"
out=$(lead8 ask-human --file /home/agent/tasks/q1.md --approval 2>&1); rc=$?
check "ask-human --approval: exit 1, not supported yet" 1 "$rc"
has "ask-human --approval: says so" "$out" "approval cards are not supported yet"
check "ask-human --approval: nothing recorded" "1 " "$(tledger "SELECT count(*) FROM questions WHERE job = 'item-8'")"
out=$(inbox "$(event m5 "$K" 'September 0xMSG5')" 2>&1); rc=$?
check "inbox: a reply in the thread, exit 0" 0 "$rc"
has "inbox: the reply marks the question answered" "$out" "1 pending question(s) in thread $K answered"
check "inbox: no question pending" "0 " "$(tledger "SELECT count(*) FROM questions WHERE state = 'pending'")"
out=$(thra "$K" TH-5 -- thread end --summary-file /home/agent/summary.md 2>&1); rc=$?
check "thread end: exit 1 while the job whose home thread it is is open" 1 "$rc"
has "thread end: names the open job" "$out" "job item-8, open, reporting to this thread"
lacks "thread end: the answered question is not named" "$out" "0xQ1"
has "inbox: the reply on the thread agent's screen" "$(screen "$A")" "0xMSG5"
out=$(as item-1-a worker item-1-lead item-1 -- ask-human --file /home/agent/tasks/q1.md 2>&1); rc=$?
check "ask-human: exit 1 from a worker" 1 "$rc"
out=$(PATH=/home/agent/fake-thread:$PATH env "${P}AGENT=item-1-lead" "${P}ROLE=lead" "${P}PARENT=thread-1" "${P}SCOPE=$SCOPE" \
  "${P}JOB=item-1" "$T" ask-human --file /home/agent/tasks/q1.md 2>&1); rc=$?
check "ask-human: exit 1 from a lead whose job has no home thread" 1 "$rc"

# The job's conclusion reaches its home thread the same way, here to the
# thread agent ask-human started.
printf 'Imported everything. 0xREPORT8\n' > /home/agent/tasks/report8.md
out=$(PATH=/home/agent/fake-thread:$PATH env "${P}AGENT=item-8-lead" "${P}ROLE=lead" "${P}PARENT=$A" "${P}SCOPE=main" \
  "${P}JOB=item-8" "${P}ISSUE=" "$T" job end --report-file /home/agent/tasks/report8.md 2>&1); rc=$?
check "job end: exit 0 with a home thread" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
has "job end: conclusion printed and delivered to the live thread agent" "$out" "Job item-8 ended: done." "delivered to $A"
has "job end: conclusion on the thread agent's screen" "$(screen "$A")" "[FROM: item-8-lead]" "Conclusion of a job from item-8-lead" "Job item-8 ended: done." "Report: /home/agent/tasks/report8.md"
check "job end: the job is ended" "ended|done " "$(tledger "SELECT state, outcome FROM jobs WHERE job = 'item-8'")"

# Channel kinds: an initiative's channel runs in the checkout of its repo,
# a direct message in x-repo-general's; any other channel is ignored.
X=C0X01/1700000002.000
AX=thread-c0x01-1700000002-000
: > /home/agent/atb.log
out=$(inbox "$(event m10 "$X" '<@UEXAMPLEBOT> <@UEXAMPLEBOT> cross work 0xMSG10' '' '"channel_name":"x-repo-example-init"')" 2>&1); rc=$?
check "inbox: x-repo channel, exit 0" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
check "inbox: the ticket's title drops the leading mentions of the bot" \
  "linear create --team TH --label thread --title cross work 0xMSG10 --description-file DESC --json" \
  "$(sed -n 's/--description-file [^ ]*/--description-file DESC/p' /home/agent/atb.log)"
check "inbox: x-repo channel runs in the initiative's checkout" /home/agent/x-repo/example-init "$(agent_field "$AX" cwd)"
check "inbox: the thread is recorded with its channel and directory" "x-repo-example-init|/home/agent/x-repo/example-init " \
  "$(tledger "SELECT mapping, cwd FROM threads WHERE thread = '$X'")"
has "inbox: the x-repo thread agent got the message" "$(screen "$AX")" "0xMSG10"
settled "$AX"
D=D0DM1/1700000003.000
out=$(inbox "$(event m11 "$D" 'hi 0xMSG11' '' '"trigger":"dm"')" 2>&1); rc=$?
check "inbox: direct message, exit 0" 0 "$rc"
check "inbox: a direct message runs in x-repo-general's checkout" /home/agent/x-repo/general \
  "$(agent_field thread-d0dm1-1700000003-000 cwd)"
: > /home/agent/fednet.log
for fields in '"channel_name":"fednet-dev"' ''; do
  out=$(inbox "$(event m12 C0OTHER/1.1 'x' '' "$fields")" 2>&1); rc=$?
  check "inbox: channel [$fields] ignored, exit 0" 0 "$rc"
  has "inbox: says the message [$fields] was ignored" "$out" "ignored m12"
done
check "inbox: an ignored channel gets no reply, no message row, no thread row" "|0 0 " \
  "$(cat /home/agent/fednet.log)|$(tledger "SELECT count(*) FROM inbox WHERE msg_id = 'm12'")$(tledger "SELECT count(*) FROM threads WHERE thread = 'C0OTHER/1.1'")"

# A repo not checked out on this machine: the thread is told, nothing starts.
out=$(inbox "$(event m13 C0NONE/1.1 'x' '' '"channel_name":"repo-no-such"')" 2>&1); rc=$?
check "inbox: a repo not checked out here, exit 0" 0 "$rc"
check "inbox: tells the thread the repo is not here" \
  "client post -socket /home/agent/fednet.sock -thread C0NONE/1.1 -- The repo no-such is not checked out on this machine (/home/agent/dev/no-such), so no agent was started for this thread.|" \
  "$(tr '\n' '|' < /home/agent/fednet.log)"
check "inbox: no agent, the message dropped" "agent_not_found dropped " \
  "$(agent_field thread-c0none-1-1 agent_status) $(tledger "SELECT state FROM inbox WHERE msg_id = 'm13'")"

# The payload's scope picks the fleet: its herdr session and its ledger.
out=$(inbox "$(event m14 C0SC/1.1 'x 0xMSG14' '' '"channel_name":"repo-'"$R"'","scope":"'"$SCOPE"'"')" 2>&1); rc=$?
check "inbox: scope from the payload, exit 0" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
check "inbox: the agent runs in that scope's session" "thread-c0sc-1-1 agent_not_found" \
  "$(herdr --session "fleet-$SCOPE" agent get thread-c0sc-1-1 | jq -r .result.agent.name) $(agent_field thread-c0sc-1-1 agent_status)"
check "inbox: the thread is in that scope's ledger only" "1 0 " \
  "$(sqlite3 "$DB" "SELECT count(*) FROM threads WHERE thread = 'C0SC/1.1'") $(tledger "SELECT count(*) FROM threads WHERE thread = 'C0SC/1.1'")"

# A cross-repo job of the initiative's thread runs in the initiative's
# checkout; job list shows each thread its own channel's jobs.
out=$(thra "$K" TH-5 -- job start item-9 --repo "$R" --task-file "$(task item-9 'repo job 0xITEM9')" 2>&1); rc=$?
check "job start from the repo thread: exit 0" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
out=$(thra "$X" "" -- job start wire-x --task-file "$(task wirex 'cross job 0xWIREX')" 2>&1); rc=$?
check "job start from the x-repo thread: exit 0" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
check "job start: the cross-repo lead runs in the initiative's checkout" /home/agent/x-repo/example-init/wire-x \
  "$(agent_field wire-x-lead cwd)"
check "job list: the repo thread sees its channel's job" "item-9" "$(thra "$K" TH-5 -- job list | cut -f1 | tr '\n' ' ' | sed 's/ $//')"
check "job list: the x-repo thread sees its channel's job" "wire-x" "$(thra "$X" "" -- job list | cut -f1 | tr '\n' ' ' | sed 's/ $//')"
check "job list --all: every job of the scope" "item-9 wire-x" "$(thra "$K" TH-5 -- job list --all | cut -f1 | tr '\n' ' ' | sed 's/ $//')"
out=$(thra "$X" "" -- job end wire-x --force 2>&1); rc=$?
check "job end --force of the x-repo job: exit 0" 0 "$rc"
check "job end --force: the job's directory removed, the checkout kept" "no yes" \
  "$([ -e /home/agent/x-repo/example-init/wire-x ] && echo yes || echo no) $([ -d /home/agent/x-repo/example-init ] && echo yes || echo no)"
thra "$K" TH-5 -- job end item-9 --force >/dev/null 2>&1; rc=$?
check "job end --force of the repo job: exit 0" 0 "$rc"

# Ending a job gives back fleet's claim on the parent issue without closing
# it, so a later job takes the same parent; --close-parent closes it.
printf 'Did part of it. 0xREPORTP\n' > /home/agent/tasks/report-par.md
parjob() { thra "$X" "" -- job start "$1" --parent-issue EX-20 --task-file "$(task "$1" "part of EX-20 0x$1")" 2>&1; }
parend() { # <job> [flags...]: `job end` as the job's lead
  local job=$1
  shift
  PATH=/home/agent/fake-thread:$PATH env "${P}AGENT=$job-lead" "${P}ROLE=lead" "${P}PARENT=$AX" "${P}SCOPE=main" \
    "${P}JOB=$job" "${P}ISSUE=TH-5" "$T" job end --report-file /home/agent/tasks/report-par.md "$@" 2>&1
}
for job in par-a par-b; do
  flags=()
  want="linear release EX-20 --agent par-a-lead --reason job par-a ended"
  if [ "$job" = par-b ]; then
    flags=(--close-parent done)
    want="linear release EX-20 --agent par-b-lead --reason done --done"
  fi
  : > /home/agent/atb.log
  out=$(parjob "$job"); rc=$?
  check "job start of $job on the parent EX-20: exit 0" 0 "$rc"
  [ "$rc" = 0 ] || printf '%s\n' "$out"
  settled "$AX"
  out=$(parend "$job" "${flags[@]}"); rc=$?
  check "job end of $job [${flags[*]}]: exit 0" 0 "$rc"
  [ "$rc" = 0 ] || printf '%s\n' "$out"
  check "job end of $job [${flags[*]}]: the parent's release" "$want" "$(grep '^linear release EX-20 ' /home/agent/atb.log)"
  settled "$AX"
done

# Sessions on demand. The container has no systemd: a fake systemctl starts
# the server the unit would (`herdr --session fleet-%i server`) in a session
# of its own, outside the hook's process group, as systemd would run it.
cat > /home/agent/fake-thread/systemctl <<'SYSTEMCTL'
#!/bin/sh
echo "$*" >> /home/agent/systemctl.log
unit=${3%.service}
setsid herdr --session "fleet-${unit#fleet-scope@}" server >>/home/agent/systemctl-server.log 2>&1 </dev/null &
# Return once the server answers, as it is then in its own session: the
# hook kills systemctl's process group right after it exits.
for _ in $(seq 1 50); do herdr --session "fleet-${unit#fleet-scope@}" status server --json | grep -q '"running":true' && exit 0; sleep 0.2; done
exit 1
SYSTEMCTL
chmod +x /home/agent/fake-thread/systemctl
mkdir -p /home/agent/run
L=(herdr --session fleet-lazy)
echo '{"fednet": {"socket": "/home/agent/fednet.sock"}}' > "/home/agent/.config/$T/lazy.json"
lazy_tabs() { "${L[@]}" tab list | jq -r '[.result.tabs[].label] | join(" ")'; }
check "the scope lazy has no session yet" false "$("${L[@]}" status server --json | jq .running)"
out=$(PATH=/home/agent/fake-thread:$PATH "$T" inbox "$(event m19 C0LZ/0.0 'lazy' '' '"channel_name":"repo-'"$R"'","scope":"lazy"')" 2>&1); rc=$?
check "inbox: a session to start without XDG_RUNTIME_DIR, exit 5" 5 "$rc"
has "inbox: names both settings" "$out" "Environment=XDG_RUNTIME_DIR=/run/user/<uid>" "-hook-env XDG_RUNTIME_DIR"
check "inbox: systemctl not called" no "$([ -e /home/agent/systemctl.log ] && echo yes || echo no)"
out=$(inbox "$(event m19b C0LZ/0.1 'lazy' '' '"channel_name":"repo-no-such","scope":"lazy"')" 2>&1); rc=$?
check "inbox: a missing checkout in a scope without a session, exit 0" 0 "$rc"
has "inbox: the thread is told" "$(cat /home/agent/fednet.log)" "-thread C0LZ/0.1 -- The repo no-such is not checked out"
check "inbox: no session started for a thread that gets no agent" "no false" \
  "$([ -e /home/agent/systemctl.log ] && echo yes || echo no) $("${L[@]}" status server --json | jq .running)"
out=$(inbox "$(event m20 C0LZ/1.1 'lazy 0xMSG20' '' '"channel_name":"repo-'"$R"'","scope":"lazy"')" 2>&1); rc=$?
check "inbox: a scope without a session, exit 0" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
has "inbox: says it started the session" "$out" "started the herdr session of scope lazy (fleet-scope@lazy.service)"
check "inbox: the session started through the user unit" "--user start fleet-scope@lazy.service|" "$(tr '\n' '|' < /home/agent/systemctl.log)"
check "inbox: the session outlives the hook" true "$("${L[@]}" status server --json | jq .running)"
[ "$("${L[@]}" status server --json | jq .running)" = true ] || cat /home/agent/systemctl-server.log
check "inbox: the threads workspace has the shell tab, then the thread's" "shell c0lz-1-1" "$(lazy_tabs)"
shell_pane=$("${L[@]}" pane list | jq -r '.result.panes[] | select(.label == "lazy-shell") | .pane_id')
check "inbox: the shell pane is labelled <scope>-shell and runs no agent" "lazy-shell null" \
  "$("${L[@]}" pane get "$shell_pane" | jq -r '.result.pane.label + " " + (.result.pane.agent | tostring)')"
check "inbox: the thread agent runs in the new session" thread-c0lz-1-1 "$("${L[@]}" agent get thread-c0lz-1-1 | jq -r .result.agent.name)"
"${L[@]}" agent wait thread-c0lz-1-1 --until idle --until done --timeout 20000 >/dev/null
# The shell tab closed by hand comes back at the next start; so does the
# whole workspace.
"${L[@]}" tab close "$("${L[@]}" pane get "$shell_pane" | jq -r .result.pane.tab_id)" >/dev/null
check "the shell tab is closed by hand" "c0lz-1-1" "$(lazy_tabs)"
out=$(inbox "$(event m21 C0LZ/2.2 'lazy 0xMSG21' '' '"channel_name":"repo-'"$R"'","scope":"lazy"')" 2>&1); rc=$?
check "inbox: next start with the shell tab gone, exit 0" 0 "$rc"
check "inbox: the shell tab restored" "c0lz-1-1 shell c0lz-2-2" "$(lazy_tabs)"
check "inbox: the session was not started again" 1 "$(wc -l < /home/agent/systemctl.log)"
"${L[@]}" agent wait thread-c0lz-2-2 --until idle --until done --timeout 20000 >/dev/null
"${L[@]}" workspace close "$("${L[@]}" workspace list | jq -r '.result.workspaces[] | select(.label == "threads") | .workspace_id')" >/dev/null
check "the threads workspace is closed by hand" 0 "$("${L[@]}" workspace list | jq '.result.workspaces | length')"
out=$(inbox "$(event m22 C0LZ/3.3 'lazy 0xMSG22' '' '"channel_name":"repo-'"$R"'","scope":"lazy"')" 2>&1); rc=$?
check "inbox: next start with the workspace gone, exit 0" 0 "$rc"
check "inbox: the workspace and its shell tab restored" "shell c0lz-3-3" "$(lazy_tabs)"
"${L[@]}" server stop >/dev/null

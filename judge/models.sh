# Sourced by inside.sh: the watch `model-changes` against the real herdr,
# in a scope of its own (`models`, herdr session fleet-models, which this
# suite starts and stops), with a fake fednet that logs its arguments. The
# ledger rows are inserted directly; each fake Claude gets the session
# file and the transcript Claude Code would write for it, under
# ~/.claude, found through the pid `herdr pane process-info` gives. Arms
# that must differ:
#   m-w       a worker downgraded after a refusal, in its job's workspace:
#             a new session of the same name in a new tab of that
#             workspace, on the model and effort the ledger recorded, the
#             old tab closed; its lead told, the home thread posted to,
#             once; stopped again in the new session, left to the people
#   thread-m  the home thread's agent, stopped by a refusal with no
#             fallback: told once to go on, its own thread posted to
#   KN's agent a thread agent moved to another model: its session ended,
#             the thread's next one started in the `threads` workspace
#   m-q       a worker refused, retried, gone on with a tool and paused
#             on Claude Code's dialog with nothing more recorded: a new
#             session of the same name, as m-w gets
#   TQ's agent a thread agent paused on that dialog: nothing typed into
#             it, its session ended, the thread's next one started
#   m-ok      a worker that stayed on its model: nobody told
#   m-p       a worker moved to another model with no fallback, as a
#             person's /model moves it: only reported, its session kept
# Every conversation text in the transcripts carries 0xREFUSEDTEXT, which
# nothing fleet writes may.
saved=("$SCOPE" "$DB")
SCOPE=models
S=(herdr --session fleet-$SCOPE)
DB=/home/agent/.local/state/$T/$SCOPE.db
start_server fleet-$SCOPE
echo '{}' | scope_config "$SCOPE"
thr job start setup --task-file "$(task setup 'makes the ledger')" >/dev/null 2>&1
thr job end setup --force >/dev/null 2>&1
check "models: the scope's ledger exists" yes "$([ -e "$DB" ] && echo yes || echo no)"
echo '{"fednet": {"socket": "/home/agent/models.sock"}}' | scope_config "$SCOPE"
mkdir -p /home/agent/fake-models
# `read-thread` answers a message posted just now, so no thread is quiet
# and the thread rules stay out of the way.
cat > /home/agent/fake-models/fednet <<'FEDNET'
#!/bin/sh
[ "$2" = react ] && exit 0
printf '%s\n--\n' "$*" >> /home/agent/models-fednet.log
case "$2" in
  read-thread) printf '{"messages":[{"ts":"%s.000100","user":"U0ABC","text":"b"}]}\n' "$(date +%s)" ;;
  *) echo m-posted ;;
esac
FEDNET
chmod +x /home/agent/fake-models/fednet
: > /home/agent/models-fednet.log
mwatch() { PATH=/home/agent/fake-models:$PATH "$T" --scope "$SCOPE" watch; }
KM=C0MOD/1700000000.000

KN=C0MOD/1700000001.000
TN=thread-c0mod-1700000001-000
KQ=C0MOD/1700000002.000
TQ=thread-c0mod-1700000002-000
for name in thread-m m-ok m-p "$TN" "$TQ"; do
  check "models: $name starts idle" idle "$(start_fake "$name")"
done
# The job's workspace m: its lead, and m-w in a tab of its own.
mws=$("${S[@]}" workspace create --cwd /home/agent --label m --no-focus)
sleep 1
check "models: m-lead starts idle" idle "$("${S[@]}" agent start m-lead --kind claude --pane \
  "$(printf '%s' "$mws" | jq -r .result.root_pane.pane_id)" --timeout 20000 | jq -r '.result.agent.agent_status // .error.code')"
mkdir -p /home/agent/mwork
wpane=$("${S[@]}" tab create --workspace "$(printf '%s' "$mws" | jq -r .result.root_pane.workspace_id)" --label w \
  --cwd /home/agent/mwork --no-focus | jq -r .result.root_pane.pane_id)
sleep 1
check "models: m-w starts idle" idle "$("${S[@]}" agent start m-w --kind claude --pane "$wpane" --timeout 20000 |
  jq -r '.result.agent.agent_status // .error.code')"
qpane=$("${S[@]}" tab create --workspace "$(printf '%s' "$mws" | jq -r .result.root_pane.workspace_id)" --label q \
  --cwd /home/agent/mwork --no-focus | jq -r .result.root_pane.pane_id)
sleep 1
check "models: m-q starts idle" idle "$("${S[@]}" agent start m-q --kind claude --pane "$qpane" --timeout 20000 |
  jq -r '.result.agent.agent_status // .error.code')"
tqpane=$(agent_field "$TQ" pane_id)
mtask=$(task m-w '# Fix the example parser

Make the parser accept empty input.')
now=$(date +%s)
sqlite3 "$DB" >/dev/null <<SQL
INSERT INTO jobs (job, lead_cwd, home_thread, state, started_at) VALUES ('m', '/home/agent', '$KM', 'open', $now);
INSERT INTO threads (thread, slug, channel, mapping, cwd, sessions, created_at) VALUES
    ('$KN', 'c0mod-1700000001-000', 'C0MOD', 'repo-models', '/home/agent', 1, $now),
    ('$KQ', 'c0mod-1700000002-000', 'C0MOD', 'repo-models', '/home/agent', 1, $now);
INSERT INTO agents (name, role, job, parent, cwd, thread, state, started_at, pane_id, kind) VALUES
    ('thread-m', 'thread', '', '', '/home/agent', '$KM', 'active', $now, '$(agent_field thread-m pane_id)', 'claude'),
    ('$TN', 'thread', '', '', '/home/agent', '$KN', 'active', $now, '$(agent_field "$TN" pane_id)', 'claude'),
    ('$TQ', 'thread', '', '', '/home/agent', '$KQ', 'active', $now, '$tqpane', 'claude'),
    ('m-ok', 'worker', 'm', 'm-lead', '/home/agent', '', 'active', $now, '$(agent_field m-ok pane_id)', 'claude');
INSERT INTO agents (name, role, job, parent, cwd, thread, state, started_at, pane_id, kind, model, effort, task) VALUES
    ('m-lead', 'lead', 'm', 'thread-m', '/home/agent', '', 'active', $now, '$(agent_field m-lead pane_id)', 'claude',
     'claude-test-configured', 'high', ''),
    ('m-w', 'worker', 'm', 'm-lead', '/home/agent/mwork', '', 'active', $now, '$wpane', 'claude',
     'claude-test-configured', 'high', '$mtask'),
    ('m-q', 'worker', 'm', 'm-lead', '/home/agent/mwork', '', 'active', $now, '$qpane', 'claude',
     'claude-test-configured', 'high', '$mtask'),
    ('m-p', 'worker', 'm', 'm-lead', '/home/agent', '', 'active', $now, '$(agent_field m-p pane_id)', 'claude',
     'claude-test-configured', 'high', '$mtask');
SQL
SW=00000000-0000-4000-8000-0000000000a1
ST=00000000-0000-4000-8000-0000000000a2
SO=00000000-0000-4000-8000-0000000000a3
session m-w "$SW" "$(said a-1 msg-1 claude-test-large tool_use)" "$(said a-2 msg-2 claude-test-large refusal)" \
  '{"type":"system","subtype":"model_refusal_fallback","uuid":"f-1","timestamp":"2030-01-02T03:00:03.000Z","originalModel":"claude-test-large","fallbackModel":"claude-test-small","content":"0xREFUSEDTEXT","apiRefusalExplanation":"0xREFUSEDTEXT"}' \
  "$(said a-3 msg-3 claude-test-small tool_use)"
session thread-m "$ST" "$(said t-1 msg-t1 claude-test-large end_turn)" "$(said t-2 msg-t2 claude-test-large refusal)" \
  '{"type":"system","subtype":"model_refusal_no_fallback","uuid":"t-3","timestamp":"2030-01-02T03:00:05.000Z","originalModel":"claude-test-large","content":"0xREFUSEDTEXT"}'
# Claude Code's records around a retried refusal: the tool's result, its
# notice, the nudge it sends, the retry's reply asking for a tool and that
# tool's result; the turn's next refusal is not recorded.
paused=("$(said q-1 msg-q1 claude-test-large refusal)"
  '{"type":"user","uuid":"q-2","message":{"content":[{"type":"tool_result","tool_use_id":"t-q1","content":"0xREFUSEDTEXT"}]}}'
  '{"type":"system","subtype":"informational","level":"notice","uuid":"q-3","content":"0xREFUSEDTEXT"}'
  '{"type":"user","uuid":"q-4","isMeta":true,"message":{"content":"0xREFUSEDTEXT"}}'
  "$(said q-5 msg-q5 claude-test-large tool_use)"
  '{"type":"user","uuid":"q-6","message":{"content":[{"type":"tool_result","tool_use_id":"t-q5","content":"0xREFUSEDTEXT"}]}}')
session m-q 00000000-0000-4000-8000-0000000000a7 "${paused[@]}"
session "$TQ" 00000000-0000-4000-8000-0000000000a8 "$(said u-1 msg-u1 claude-test-large end_turn)" "${paused[@]}"
session m-ok "$SO" "$(said o-1 msg-o1 claude-test-large tool_use)" "$(said o-2 msg-o2 claude-test-large end_turn)"
ppane=$(agent_field m-p pane_id)
session m-p 00000000-0000-4000-8000-0000000000a6 "$(said p-1 msg-p1 claude-test-large end_turn)" \
  "$(said p-2 msg-p2 claude-test-small end_turn)"
session "$TN" 00000000-0000-4000-8000-0000000000a4 "$(said n-1 msg-n1 claude-test-large refusal)" \
  '{"type":"system","subtype":"model_refusal_fallback","uuid":"n-2","timestamp":"2030-01-02T03:00:07.000Z","originalModel":"claude-test-large","fallbackModel":"claude-test-small","content":"0xREFUSEDTEXT"}' \
  "$(said n-3 msg-n3 claude-test-small end_turn)"

mwatch >/home/agent/models1.out 2>&1; rc=$?
check "models: watch exits 0" 0 "$rc"
has "models: each event is a line on stdout" "$(cat /home/agent/models1.out)" \
  "model event: m-w (worker): 2030-01-02T03:00:03.000Z model_refusal_fallback: claude-test-large -> claude-test-small (record f-1)" \
  "model event: m-w (worker): 2030-01-02T03:00:00.000Z model_change: claude-test-large -> claude-test-small" \
  "model event: thread-m (thread): 2030-01-02T03:00:05.000Z model_refusal_no_fallback: stopped on claude-test-large"
lacks "models: m-ok, which stayed on its model, is not reported" "$(cat /home/agent/models1.out)" "model event: m-ok"
lead_got=$(received m-lead)
has "models: the lead is told about its worker, as watch" "$lead_got" "[FROM: watch]" \
  "the Claude Code session of m-w (worker, job m) has 3 model event(s)" "(record f-1)" \
  "fleet replaced the session of m-w (pane $wpane closed, the new session in pane"
check "models: m-w's row now records a new pane, live" "active 1 1" \
  "$(ledger "SELECT state, takeovers, pane_id != '$wpane' FROM agents WHERE name = 'm-w'" | tr '|' ' ' | xargs)"
check "models: m-w's old tab is gone" pane_not_found \
  "$("${S[@]}" pane get "$wpane" 2>&1 | jq -r '.error.code // "still there"')"
check "models: m-w's new session is in its job's workspace" \
  "$(printf '%s' "$mws" | jq -r .result.root_pane.workspace_id)" "$(agent_field m-w workspace_id)"
wpid=$("${S[@]}" pane process-info --pane "$(agent_field m-w pane_id)" |
  jq -r '.result.process_info.foreground_processes[0].pid')
has "models: m-w's new session runs on the model and effort the ledger recorded" \
  "$(tr '\0' ' ' < "/proc/$wpid/cmdline")" "--model claude-test-configured --effort high"
w_got=$(received m-w)
has "models: m-w's new session gets its task and the takeover note" "$w_got" "[FROM: watch]" \
  "This session takes over from an earlier session of m-w" "Do not redo the step that was stopped" \
  "# Fix the example parser"
lacks "models: no /model is typed into m-w" "$w_got" "/model"
has "models: the lead is told m-q, paused after a retried refusal, was replaced" "$lead_got" \
  "fleet replaced the session of m-q (pane $qpane closed, the new session in pane" "the session is stopped"
check "models: m-q's row now records a new pane, live" "active 1 1" \
  "$(ledger "SELECT state, takeovers, pane_id != '$qpane' FROM agents WHERE name = 'm-q'" | tr '|' ' ' | xargs)"
q_got=$(received m-q)
has "models: m-q's new session gets the takeover note" "$q_got" "This session takes over from an earlier session of m-q"
lacks "models: nothing is typed into the paused thread agent's old session" \
  "$(cat "$fake/received-$tqpane" 2>/dev/null)" "[FROM: watch]"
check "models: the paused thread agent's session ended, the next one live" "ended active" \
  "$(ledger "SELECT state FROM agents WHERE name = '$TQ' ORDER BY id" | xargs)"
check "models: the paused thread's session is counted on the thread" 1 \
  "$(ledger "SELECT takeovers FROM threads WHERE thread = '$KQ'" | xargs)"
tq_got=$(received "$TQ")
has "models: the paused thread's next session starts with the takeover note" "$tq_got" \
  "this session takes over from the session before it on this thread"
has "models: m-p, moved with no fallback, is only reported" "$lead_got" \
  "the Claude Code session of m-p (worker, job m) has 1 model event(s)" "fleet did nothing about these events"
check "models: m-p keeps its session" "$ppane 0" \
  "$(agent_field m-p pane_id) $(ledger "SELECT takeovers FROM agents WHERE name = 'm-p'" | xargs)"
lacks "models: m-p is sent nothing" "$(received m-p)" "[FROM: watch]"
has "models: thread-m is told once to go on" "$(received thread-m)" "Go on with the thread's work"
lacks "models: no /model is typed into thread-m" "$(received thread-m)" "/model"
check "models: the moved thread agent's session ended, the next one live" "ended active" \
  "$(ledger "SELECT state FROM agents WHERE name = '$TN' ORDER BY id" | xargs)"
has "models: the thread's next session starts with the takeover note" "$(received "$TN")" \
  "this session takes over from the session before it on this thread" "Do not redo the step that was stopped"
check "models: the thread's next session is in the threads workspace" threads \
  "$("${S[@]}" workspace list | jq -r --arg id "$(agent_field "$TN" workspace_id)" \
    '.result.workspaces[] | select(.workspace_id == $id) | .label')"
posts=$(cat /home/agent/models-fednet.log)
has "models: the home thread is posted to about both agents" "$posts" \
  "-thread $KM -- fleet watch：m-w 的 Claude Code 会话出现了 3 个模型事件" \
  "-thread $KM -- fleet watch：thread-m 的 Claude Code 会话出现了 2 个模型事件"
for text in "$(cat /home/agent/models1.out)" "$lead_got" "$posts" "$w_got" "$(received thread-m)" "$(received "$TN")" \
  "$q_got" "$tq_got"; do
  lacks "models: nothing fleet writes carries the conversation's text" "$text" 0xREFUSEDTEXT
done
mwatch >/home/agent/models2.out 2>&1; rc=$?
check "models: the next run exits 0" 0 "$rc"
lacks "models: the next run reports nothing again" "$(cat /home/agent/models2.out)" "model event:"
check "models: each agent's events are posted once" 6 "$(grep -c '的 Claude Code 会话出现了' /home/agent/models-fednet.log)"
# m-w's new session is stopped too: left to the people, no third session.
newpane=$(agent_field m-w pane_id)
session m-w 00000000-0000-4000-8000-0000000000a5 "$(said r-1 msg-r1 claude-test-large refusal)"
mwatch >/home/agent/models3.out 2>&1; rc=$?
check "models: the third run exits 0" 0 "$rc"
check "models: m-w keeps its second session" "$newpane" "$(agent_field m-w pane_id)"
has "models: the lead is told a person decides" "$(received m-lead)" "it handled m-w once already"
for out in /home/agent/models1.out /home/agent/models2.out /home/agent/models3.out; do
  [ "$fail" = 0 ] || { echo "--- $out"; cat "$out"; }
done
"${S[@]}" server stop >/dev/null
SCOPE=${saved[0]} DB=${saved[1]}
S=(herdr --session fleet-$SCOPE)

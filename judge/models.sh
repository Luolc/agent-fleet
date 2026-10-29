# Sourced by inside.sh: the watch `model-changes` against the real herdr,
# in a scope of its own (`models`, herdr session fleet-models, which this
# suite starts and stops), with a fake fednet that logs its arguments. The
# ledger rows are inserted directly; each fake Claude gets the session
# file and the transcript Claude Code would write for it, under
# ~/.claude, found through the pid `herdr pane process-info` gives. Arms
# that must differ:
#   m-w       a worker downgraded after a refusal: its lead told, the home
#             thread posted to, once
#   thread-m  the home thread's agent, stopped by a refusal with no
#             fallback: its own thread posted to
#   m-ok      a worker that stayed on its model: nobody told
# Every conversation text in the transcripts carries 0xREFUSEDTEXT, which
# no report may.
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

for name in thread-m m-lead m-w m-ok; do
  check "models: $name starts idle" idle "$(start_fake "$name")"
done
now=$(date +%s)
sqlite3 "$DB" >/dev/null <<SQL
INSERT INTO jobs (job, lead_cwd, home_thread, state, started_at) VALUES ('m', '/home/agent', '$KM', 'open', $now);
INSERT INTO agents (name, role, job, parent, cwd, thread, state, started_at, pane_id) VALUES
    ('thread-m', 'thread', '', '', '/home/agent', '$KM', 'active', $now, '$(agent_field thread-m pane_id)'),
    ('m-lead', 'lead', 'm', 'thread-m', '/home/agent', '', 'active', $now, '$(agent_field m-lead pane_id)'),
    ('m-w', 'worker', 'm', 'm-lead', '/home/agent', '', 'active', $now, '$(agent_field m-w pane_id)'),
    ('m-ok', 'worker', 'm', 'm-lead', '/home/agent', '', 'active', $now, '$(agent_field m-ok pane_id)');
SQL
# <agent> <session> <records...>: the agent's session file and transcript.
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
# <uuid> <message id> <model> <stop_reason>
said() {
  printf '{"type":"assistant","uuid":"%s","timestamp":"2030-01-02T03:00:00.000Z","isSidechain":false,"message":{"id":"%s","model":"%s","stop_reason":"%s","content":[{"type":"text","text":"0xREFUSEDTEXT"}]}}' \
    "$1" "$2" "$3" "$4"
}
SW=00000000-0000-4000-8000-0000000000a1
ST=00000000-0000-4000-8000-0000000000a2
SO=00000000-0000-4000-8000-0000000000a3
session m-w "$SW" "$(said a-1 msg-1 claude-test-large tool_use)" "$(said a-2 msg-2 claude-test-large refusal)" \
  '{"type":"system","subtype":"model_refusal_fallback","uuid":"f-1","timestamp":"2030-01-02T03:00:03.000Z","originalModel":"claude-test-large","fallbackModel":"claude-test-small","content":"0xREFUSEDTEXT","apiRefusalExplanation":"0xREFUSEDTEXT"}' \
  "$(said a-3 msg-3 claude-test-small tool_use)"
session thread-m "$ST" "$(said t-1 msg-t1 claude-test-large end_turn)" "$(said t-2 msg-t2 claude-test-large refusal)" \
  '{"type":"system","subtype":"model_refusal_no_fallback","uuid":"t-3","timestamp":"2030-01-02T03:00:05.000Z","originalModel":"claude-test-large","content":"0xREFUSEDTEXT"}'
session m-ok "$SO" "$(said o-1 msg-o1 claude-test-large tool_use)" "$(said o-2 msg-o2 claude-test-large end_turn)"

mwatch >/home/agent/models1.out 2>&1; rc=$?
check "models: watch exits 0" 0 "$rc"
has "models: each event is a line on stdout" "$(cat /home/agent/models1.out)" \
  "model event: m-w (worker): 2030-01-02T03:00:03.000Z model_refusal_fallback: claude-test-large -> claude-test-small (record f-1)" \
  "model event: m-w (worker): 2030-01-02T03:00:00.000Z model_change: claude-test-large -> claude-test-small" \
  "model event: thread-m (thread): 2030-01-02T03:00:05.000Z model_refusal_no_fallback: stopped on claude-test-large"
lacks "models: m-ok, which stayed on its model, is not reported" "$(cat /home/agent/models1.out)" "model event: m-ok"
lead_got=$(received m-lead)
has "models: the lead is told about its worker, as watch" "$lead_got" "[FROM: watch]" \
  "the Claude Code session of m-w (worker, job m) has 3 model event(s)" "(record f-1)" "fleet only reports this"
posts=$(cat /home/agent/models-fednet.log)
has "models: the home thread is posted to about both agents" "$posts" \
  "-thread $KM -- fleet watch：m-w 的 Claude Code 会话出现了 3 个模型事件" \
  "-thread $KM -- fleet watch：thread-m 的 Claude Code 会话出现了 2 个模型事件"
for text in "$(cat /home/agent/models1.out)" "$lead_got" "$posts"; do
  lacks "models: no report carries the conversation's text" "$text" 0xREFUSEDTEXT
done
mwatch >/home/agent/models2.out 2>&1; rc=$?
check "models: the next run exits 0" 0 "$rc"
lacks "models: the next run reports nothing again" "$(cat /home/agent/models2.out)" "model event:"
check "models: each agent's events are posted once" 2 "$(grep -c '的 Claude Code 会话出现了' /home/agent/models-fednet.log)"
for out in /home/agent/models1.out /home/agent/models2.out; do
  [ "$fail" = 0 ] || { echo "--- $out"; cat "$out"; }
done
"${S[@]}" server stop >/dev/null
SCOPE=${saved[0]} DB=${saved[1]}
S=(herdr --session fleet-$SCOPE)

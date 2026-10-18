# Sourced by inside.sh: `watch` and `status` against real herdr and fake
# Claudes, with the ledger rows inserted directly into a ledger that has no
# live rows, the thread agent idle and the fake agent at its permission
# prompt, as lifecycle.sh and send.sh leave them in a full run. Two
# arms must differ: a moving worker (TICK: its transcript grows) and a
# stuck one (HANG: only the spinner timer and the footer countdown move).
# w-lead receives the notices, so it moves; u-lead, idle with no workers,
# is a lead whose own suspicion is only recorded (the scope has no fednet
# socket, so no thread rule runs). The clock moves through
# <prefix>WATCH_NOW: each run is 11 minutes after the last, past the
# 10-minute limit, but for one at 9 minutes, within the 10 minutes a lead
# or worker may be gone from herdr before its row is ended. e-old's row is
# ended with its pane still there, as a crash or an older fleet leaves it;
# an ended row of w-tick's name records the pane the live w-tick row
# records, which watch must not close.
need_fake
need_thread_agent
if [ ! -e "$DB" ]; then
  thr job start setup --task-file "$(task setup 'makes the ledger')" >/dev/null 2>&1
  thr job end setup --force >/dev/null 2>&1
fi
if [ "$(agent_field fake agent_status)" != blocked ]; then
  "${S[@]}" agent prompt fake "please BLOCK" --wait --until working --timeout 20000 >/dev/null
  "${S[@]}" agent wait fake --until blocked --timeout 20000 >/dev/null
fi
now=$(date +%s)
sqlite3 "$DB" >/dev/null <<SQL
INSERT INTO agents (name, role, job, parent, state, started_at) VALUES
    ('thread-1', 'thread', '', '', 'active', $now),
    ('u-lead', 'lead', 'u', 'thread-1', 'active', $now),
    ('w-lead', 'lead', 'w', 'thread-1', 'active', $now),
    ('w-tick', 'worker', 'w', 'w-lead', 'active', $now),
    ('w-hang', 'worker', 'w', 'w-lead', 'active', $now),
    ('w-gone', 'worker', 'w', 'w-lead', 'active', $now);
SQL
for name in u-lead w-lead w-tick w-hang e-old; do
  check "watch: $name starts idle" idle "$(start_fake "$name")"
done
# Named after the agent, as fleet names the pane of each agent it starts.
for name in w-tick e-old; do "${S[@]}" pane rename "$(agent_field "$name" pane_id)" "$name" >/dev/null; done
sqlite3 "$DB" >/dev/null <<SQL
UPDATE agents SET pane_id = '$(agent_field w-tick pane_id)' WHERE name = 'w-tick' AND state = 'active';
INSERT INTO agents (name, role, job, parent, state, started_at, ended_at, pane_id) VALUES
    ('e-old', 'worker', 'e', 'e-lead', 'ended', $now, $now, '$(agent_field e-old pane_id)'),
    ('w-tick', 'worker', 'w', 'w-lead', 'ended', $now, $now, '$(agent_field w-tick pane_id)');
SQL
for arm in w-tick:TICK w-hang:HANG; do
  "${S[@]}" agent prompt "${arm%%:*}" "${arm#*:}" --wait --until working --timeout 20000 >/dev/null
  check "watch: ${arm%%:*} is working" working \
    "$("${S[@]}" agent get "${arm%%:*}" | jq -r '.result.agent.agent_status')"
done

# The clock jumps 11 minutes a run, but TICK's screen moves only once a
# second, so each run waits for it to move in real time too.
T0=$(date +%s)
# In a full run lifecycle.sh leaves an ended job with a parent issue; the
# fake atb answers that Linear has it in no started state.
mkdir -p /home/agent/watch-atb
printf '#!/bin/sh\necho %s\n' "'{\"issues\":{\"nodes\":[]}}'" > /home/agent/watch-atb/atb
chmod +x /home/agent/watch-atb/atb
watch() { sleep 2; PATH=/home/agent/watch-atb:$PATH env "${P}WATCH_NOW=$((T0 + $1 * 60))" "$T" --scope "$SCOPE" watch; }
suspects() { sqlite3 "$DB" "SELECT group_concat(name, ' ') FROM (SELECT name FROM agents WHERE suspect = 1 ORDER BY name)"; }

watch 0 >/home/agent/watch1.out 2>&1; rc=$?
check "watch: first run exit 0" 0 "$rc"
check "watch: first run, only the worker gone from herdr is a suspect" w-gone "$(suspects)"
check "watch: first run records a reading for every watched agent in herdr" "4 " \
  "$(ledger "SELECT count(*) FROM agents WHERE state != 'ended' AND last_status IS NOT NULL AND last_seq IS NOT NULL AND last_screen_hash IS NOT NULL AND last_change_at IS NOT NULL")"
check "watch: the thread agent is not watched" "1 " \
  "$(ledger "SELECT last_status IS NULL FROM agents WHERE name = 'thread-1' AND state != 'ended'")"
has "watch: the worker's lead is told" "$(cat /home/agent/watch1.out)" "delivered to w-lead"
settled w-lead
has "watch: notice with [FROM: watch] and the gone worker on the lead's screen" "$(screen w-lead)" "[FROM: watch]" "w-gone"
check "watch: the tab of the ended row's pane is closed" agent_not_found "$(agent_field e-old agent_status)"
has "watch: and watch says so" "$(cat /home/agent/watch1.out)" "watch: closed the tab of e-old, whose row is ended"
check "watch: a pane a live row records is not closed for an ended row of the same name" working \
  "$(agent_field w-tick agent_status)"
check "watch: the gone worker's row stays live, its absence timed" "active|gone|$T0 " \
  "$(ledger "SELECT state || '|' || last_status || '|' || last_change_at FROM agents WHERE name = 'w-gone'")"

watch 9 >/home/agent/watch9.out 2>&1; rc=$?
check "watch: run at 9m exit 0" 0 "$rc"
check "watch: gone for 9 minutes, the worker's row stays live" "active " \
  "$(ledger "SELECT state FROM agents WHERE name = 'w-gone'")"
lacks "watch: and its lead is not told it ended" "$(cat /home/agent/watch9.out)" "delivered to"

watch 11 >/home/agent/watch2.out 2>&1; rc=$?
check "watch: second run exit 0" 0 "$rc"
check "watch: gone for 11 minutes, the worker's row is ended" "ended " \
  "$(ledger "SELECT state FROM agents WHERE name = 'w-gone'")"
has "watch: and watch says so" "$(cat /home/agent/watch2.out)" \
  "watch: ended w-gone (worker, job w): gone from herdr for 11m"
check "watch: the stuck worker and the idle lead are suspects, the moving worker and the told lead are not" "u-lead w-gone w-hang" "$(suspects)"
has "watch: the lead is told when the set among its workers changes" "$(cat /home/agent/watch2.out)" "delivered to w-lead"
has "watch: suspects and their evidence on stdout, a lead's own included" "$(cat /home/agent/watch2.out)" \
  "suspect: w-hang (worker, job w, parent w-lead): no change for 11m" \
  "suspect: u-lead (lead, job u, parent thread-1): no change for 11m"
lacks "watch: a lead's own suspicion is not delivered to the thread agent" "$(cat /home/agent/watch2.out)" "delivered to thread-1"
lacks "watch: a lead's own suspicion is not delivered to itself" "$(cat /home/agent/watch2.out)" "delivered to u-lead"

settled w-lead
has "watch: notice with the stuck worker on the lead's screen" "$(screen w-lead)" "[FROM: watch]" "w-hang"
has "watch: the lead is told its gone worker is ended" "$(screen w-lead)" "rule \`worker gone\`: your worker w-gone"
lacks "watch: the other lead is not in the notice" "$(screen w-lead)" "u-lead"
lacks "watch: the idle lead got no notice" "$(screen u-lead)" "[FROM: watch]"

watch 22 >/home/agent/watch3.out 2>&1; rc=$?
check "watch: third run exit 0" 0 "$rc"
has "watch: unchanged sets, no lead is told again" "$(cat /home/agent/watch3.out)" "suspect set unchanged for every lead (2 suspect)"
lacks "watch: unchanged sets, nothing delivered" "$(cat /home/agent/watch3.out)" "delivered to"

# A second job for the --job filter: a lead gone from herdr, a worker
# gone with it, so the set of suspects among its workers changes while
# there is no lead to tell, and the fake agent the send arm left at its
# permission prompt, so one row is blocked. Blocked and unchanged past the
# limit, it is still no suspect.
now=$(date +%s)
sqlite3 "$DB" >/dev/null <<SQL
INSERT INTO agents (name, role, job, parent, state, started_at) VALUES
    ('v-lead', 'lead', 'v', 'thread-1', 'active', $now),
    ('v-gone', 'worker', 'v', 'v-lead', 'active', $now),
    ('fake', 'worker', 'v', 'v-lead', 'active', $now);
SQL
# The people were already asked about fake's screen, so watch leaves the
# screen to them (unblock.sh is the suite of the screens).
sqlite3 "$DB" "INSERT INTO questions (job, thread, asked_by, text, state, asked_at)
    SELECT 'v', 'C0V/1.0', 'unblock-' || id, 'what to press?', 'pending', $now FROM agents WHERE name = 'fake' AND state != 'ended'"
watch 33 >/home/agent/watch4.out 2>&1; rc=$?
check "watch: fourth run, a lead gone from herdr is not told about its workers, exit 0" 0 "$rc"
lacks "watch: and nothing is sent to it" "$(cat /home/agent/watch4.out)" "v-lead not found"
watch 44 >/home/agent/watch5.out 2>&1; rc=$?
check "watch: fifth run exit 0" 0 "$rc"
check "watch: the blocked worker, unchanged for 11m, is no suspect; the lead gone from herdr and the leads idle since their last notice are" \
  "u-lead v-lead w-gone w-hang w-lead" "$(suspects)"
check "watch: gone for 11 minutes, the lead and its worker are ended, the job's other worker is not" "ended ended active " \
  "$(ledger "SELECT state FROM agents WHERE name IN ('v-lead', 'v-gone', 'fake') ORDER BY name = 'fake', name DESC")"
has "watch: with no home thread, the gone lead is named on stderr" "$(cat /home/agent/watch5.out)" \
  "fleet watch: no thread to tell that v-lead is gone" "rule \`lead gone\`: v-lead, the lead of job v"
check "watch: the blocked worker's unchanged reading is recorded" "blocked " \
  "$(ledger "SELECT last_status FROM agents WHERE name = 'fake' AND state != 'ended' AND last_change_at = $((T0 + 33 * 60))")"
status_all() { # name=herdr_status:flags for every agent status lists
  "$T" --scope "$SCOPE" status --json "$@" |
    jq -r '[.agents[] | "\(.name)=\(.herdr_status // "-"):\(.flags | join(","))"] | join(" ")'
}
# After a turn herdr reports `done` until the pane is looked at; w-lead
# had its turns on the notices, thread-1 had none (it owes no work).
check "status: herdr status and flags per agent, jobs in order" \
  "thread-1=idle: u-lead=idle:owes-work,suspect fake=blocked:blocked w-lead=done:owes-work,suspect w-tick=working: w-hang=working:suspect" \
  "$(status_all)"
check "status: --job keeps exactly that job's agents" \
  "w-lead=done:owes-work,suspect w-tick=working: w-hang=working:suspect" "$(status_all --job w)"
check "status: --job on the other job" "fake=blocked:blocked" "$(status_all --job v)"
check "status: since_change_secs is set once watch has looked" true \
  "$("$T" --scope "$SCOPE" status --json | jq '[.agents[] | select(.name == "w-hang") | .since_change_secs != null] | first')"
for out in /home/agent/watch1.out /home/agent/watch9.out /home/agent/watch2.out /home/agent/watch3.out /home/agent/watch4.out /home/agent/watch5.out; do
  [ "$fail" = 0 ] || { echo "--- $out"; cat "$out"; }
done

# Sourced by inside.sh after lifecycle.sh, in the ledger the starts created
# and with the thread agent already running: `watch` and `status` against
# real herdr and fake Claudes, with the ledger rows inserted directly. Two
# arms must differ: a moving worker (TICK: its transcript grows) and a
# stuck one (HANG: only the spinner timer and the footer countdown move).
# w-lead receives the notices, so it moves; u-lead, idle with no workers,
# is the lead whose own suspicion is only printed. The 30-minute limit is
# cut to 5 s through <prefix>WATCH_STALE_SECS.
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
for name in u-lead w-lead w-tick w-hang; do
  check "watch: $name starts idle" idle "$(start_fake "$name")"
done
for arm in w-tick:TICK w-hang:HANG; do
  "${S[@]}" agent prompt "${arm%%:*}" "${arm#*:}" --wait --until working --timeout 20000 >/dev/null
  check "watch: ${arm%%:*} is working" working \
    "$("${S[@]}" agent get "${arm%%:*}" | jq -r '.result.agent.agent_status')"
done

watch() { env "${P}WATCH_STALE_SECS=5" "$T" --session judge watch --target "$TARGET"; }
suspects() { sqlite3 "$DB" "SELECT group_concat(name, ' ') FROM (SELECT name FROM agents WHERE suspect = 1 ORDER BY name)"; }

watch >/home/agent/watch1.out 2>&1; rc=$?
check "watch: first run exit 0" 0 "$rc"
check "watch: first run, only the worker gone from herdr is a suspect" w-gone "$(suspects)"
check "watch: first run records a reading for every watched agent in herdr" "4 " \
  "$(ledger "SELECT count(*) FROM agents WHERE state != 'ended' AND last_status IS NOT NULL AND last_seq IS NOT NULL AND last_screen_hash IS NOT NULL AND last_change_at IS NOT NULL")"
check "watch: the thread agent is not watched" "1 " \
  "$(ledger "SELECT last_status IS NULL FROM agents WHERE name = 'thread-1' AND state != 'ended'")"
has "watch: the worker's lead is told" "$(cat /home/agent/watch1.out)" "delivered to w-lead"
settled w-lead
has "watch: notice with [FROM: cron] and the gone worker on the lead's screen" "$(screen w-lead)" "[FROM: cron]" "w-gone"

sleep 7
watch >/home/agent/watch2.out 2>&1; rc=$?
check "watch: second run exit 0" 0 "$rc"
check "watch: the stuck worker and the idle lead are suspects, the moving worker and the told lead are not" "u-lead w-gone w-hang" "$(suspects)"
has "watch: the lead is told when the set among its workers changes" "$(cat /home/agent/watch2.out)" "delivered to w-lead"
has "watch: suspects and their evidence on stdout, a lead's own included" "$(cat /home/agent/watch2.out)" \
  "suspect: w-gone (worker, job w, parent w-lead): gone from herdr" \
  "suspect: w-hang (worker, job w, parent w-lead): no change for" \
  "suspect: u-lead (lead, job u, parent thread-1): no change for"
lacks "watch: a lead's own suspicion is not delivered to the thread agent" "$(cat /home/agent/watch2.out)" "delivered to thread-1"
lacks "watch: a lead's own suspicion is not delivered to itself" "$(cat /home/agent/watch2.out)" "delivered to u-lead"

settled w-lead
has "watch: notice with the stuck worker on the lead's screen" "$(screen w-lead)" "[FROM: cron]" "w-hang"
lacks "watch: the other lead is not in the notice" "$(screen w-lead)" "u-lead"
lacks "watch: the idle lead got no notice" "$(screen u-lead)" "[FROM: cron]"

watch >/home/agent/watch3.out 2>&1; rc=$?
check "watch: third run exit 0" 0 "$rc"
has "watch: unchanged sets, no lead is told again" "$(cat /home/agent/watch3.out)" "suspect set unchanged for every lead (3 suspect)"
lacks "watch: unchanged sets, nothing delivered" "$(cat /home/agent/watch3.out)" "delivered to"

# A second job for the --job filter: a lead gone from herdr, and the fake
# agent the send arm left at its permission prompt, so one row is blocked.
now=$(date +%s)
sqlite3 "$DB" >/dev/null <<SQL
INSERT INTO agents (name, role, job, parent, state, started_at) VALUES
    ('v-lead', 'lead', 'v', 'thread-1', 'active', $now),
    ('fake', 'worker', 'v', 'v-lead', 'active', $now);
SQL
status_all() { # name=herdr_status:flags for every agent status lists
  "$T" --session judge status --target "$TARGET" --json "$@" |
    jq -r '[.[] | "\(.name)=\(.herdr_status // "-"):\(.flags | join(","))"] | join(" ")'
}
# After a turn herdr reports `done` until the pane is looked at; w-lead
# had its turns on the notices.
check "status: herdr status and flags per agent, jobs in order" \
  "thread-1=done: u-lead=idle:owes-work,suspect v-lead=-:missing fake=blocked:blocked w-lead=done:owes-work w-tick=working: w-hang=working:suspect w-gone=-:missing,suspect" \
  "$(status_all)"
check "status: --job keeps exactly that job's agents" \
  "w-lead=done:owes-work w-tick=working: w-hang=working:suspect w-gone=-:missing,suspect" "$(status_all --job w)"
check "status: --job on the other job" "v-lead=-:missing fake=blocked:blocked" "$(status_all --job v)"
check "status: since_change_secs is set once watch has looked" true \
  "$("$T" --session judge status --target "$TARGET" --json | jq '[.[] | select(.name == "w-hang") | .since_change_secs >= 7] | first')"
for out in /home/agent/watch1.out /home/agent/watch2.out /home/agent/watch3.out; do
  [ "$fail" = 0 ] || { echo "--- $out"; cat "$out"; }
done

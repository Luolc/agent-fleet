# Sourced by inside.sh after lifecycle.sh, in the ledger spawn created and
# with the orchestra already running: `watch` and `status` against real
# herdr and fake Claudes, with the ledger rows inserted directly. Two arms
# must differ: a moving agent (TICK: its transcript grows) and a stuck one
# (HANG: only the spinner timer and the footer countdown move). The
# 30-minute limit is cut to 5 s through <prefix>WATCH_STALE_SECS.
now=$(date +%s)
sqlite3 "$DB" >/dev/null <<SQL
INSERT INTO agents (name, role, job, parent, state, started_at) VALUES
    ('orchestra', 'orchestra', '', '', 'active', $now),
    ('w-lead', 'lead', 'w', 'orchestra', 'active', $now),
    ('w-tick', 'worker', 'w', 'w-lead', 'active', $now),
    ('w-hang', 'worker', 'w', 'w-lead', 'active', $now),
    ('w-gone', 'worker', 'w', 'w-lead', 'active', $now);
SQL
for name in w-lead w-tick w-hang; do
  check "watch: $name starts idle" idle "$(start_fake "$name")"
done
for arm in w-tick:TICK w-hang:HANG; do
  "${S[@]}" agent prompt "${arm%%:*}" "${arm#*:}" --wait --until working --timeout 20000 >/dev/null
  check "watch: ${arm%%:*} is working" working \
    "$("${S[@]}" agent get "${arm%%:*}" | jq -r '.result.agent.agent_status')"
done

watch() { env "${P}WATCH_STALE_SECS=5" "$T" --session judge watch --repo "acme/$R"; }
suspects() { sqlite3 "$DB" "SELECT group_concat(name, ' ') FROM (SELECT name FROM agents WHERE suspect = 1 ORDER BY name)"; }

watch >/home/agent/watch1.out 2>&1; rc=$?
check "watch: first run exit 0" 0 "$rc"
check "watch: first run, only the agent gone from herdr is a suspect" w-gone "$(suspects)"
check "watch: first run records a reading for every watched agent in herdr" "3 " \
  "$(ledger "SELECT count(*) FROM agents WHERE state != 'ended' AND last_status IS NOT NULL AND last_seq IS NOT NULL AND last_screen_hash IS NOT NULL AND last_change_at IS NOT NULL")"
check "watch: the orchestra is not watched" "1 " \
  "$(ledger "SELECT last_status IS NULL FROM agents WHERE name = 'orchestra' AND state != 'ended'")"

sleep 7
watch >/home/agent/watch2.out 2>&1; rc=$?
check "watch: second run exit 0" 0 "$rc"
check "watch: the stuck and the idle agents are suspects, the moving one is not" "w-gone w-hang w-lead" "$(suspects)"
has "watch: the orchestra is told when the set changes" "$(cat /home/agent/watch2.out)" "delivered to orchestra"
has "watch: suspects and their evidence on stdout" "$(cat /home/agent/watch2.out)" \
  "suspect: w-gone (worker, job w, parent w-lead): gone from herdr" \
  "suspect: w-hang (worker, job w, parent w-lead): no change for"

settled orchestra
has "watch: notice with [FROM: cron] and the suspect on the orchestra's screen" "$(screen orchestra)" "[FROM: cron]" "w-hang"

watch >/home/agent/watch3.out 2>&1; rc=$?
check "watch: third run exit 0" 0 "$rc"
has "watch: unchanged set, the orchestra is not told again" "$(cat /home/agent/watch3.out)" "suspect set unchanged (3 suspect)"
lacks "watch: unchanged set, nothing delivered" "$(cat /home/agent/watch3.out)" "delivered to"

flags=$("$T" --session judge status --repo "acme/$R" --json |
  jq -r '[.[] | "\(.name)=\(.herdr_status // "-"):\(.flags | join(","))"] | join(" ")')
check "status: herdr status and flags per agent" \
  "orchestra=done: w-lead=idle:owes-work,suspect w-tick=working: w-hang=working:suspect w-gone=-:missing,suspect" "$flags"
check "status: since_change_secs is set once watch has looked" true \
  "$("$T" --session judge status --repo "acme/$R" --json | jq '[.[] | select(.name == "w-hang") | .since_change_secs >= 7] | first')"
for out in /home/agent/watch1.out /home/agent/watch2.out /home/agent/watch3.out; do
  [ "$fail" = 0 ] || { echo "--- $out"; cat "$out"; }
done

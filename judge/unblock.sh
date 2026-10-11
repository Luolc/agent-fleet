# Sourced by inside.sh: what `watch` does for agents stopped at a screen,
# in a scope of its own (`screens`, herdr session fleet-screens, which
# this suite starts and stops) with fake
# Claudes, a fake atb and a fake fednet that log their arguments. The
# ledger rows are inserted directly. Arms that must differ:
#   s-trust  the folder-trust prompt for its own directory (TRUST): answered
#            by rule, nobody told
#   s-perm   a command confirmation (BLOCK): a helper, the lead told
#   l-lead   a usage-limit menu (LIMIT): no key, no helper, the people asked
#   s-new    a start left at an unknown screen an hour ago: a helper, the
#            starter told; s-young, left there just now, is the starter's
#   thread-s the home thread's agent, at its input box: left alone
saved=("$SCOPE" "$DB")
SCOPE=screens
S=(herdr --session fleet-$SCOPE)
DB=/home/agent/.local/state/$T/$SCOPE.db
herdr --session fleet-$SCOPE server >"/home/agent/server-fleet-$SCOPE.log" 2>&1 &
for _ in $(seq 1 100); do "${S[@]}" status server >/dev/null 2>&1 && break; sleep 0.2; done
thr job start setup --task-file "$(task setup 'makes the ledger')" >/dev/null 2>&1
thr job end setup --force >/dev/null 2>&1
check "unblock: the scope's ledger exists" yes "$([ -e "$DB" ] && echo yes || echo no)"
mkdir -p /home/agent/.config/$T /home/agent/fake-screens
echo '{"linear": {"team": "SC"}, "unblock": {"project": "Example project"}, "fednet": {"socket": "/home/agent/screens.sock"}}' \
  > "/home/agent/.config/$T/$SCOPE.json"
cat > /home/agent/fake-screens/atb <<'ATB'
#!/bin/sh
echo "$*" >> /home/agent/screens-atb.log
[ "$2" = create ] && echo '{"identifier":"SC-9","url":"https://linear.example.test/SC-9"}'
exit 0
ATB
# The fake fednet logs every call; `read-thread` answers a message posted
# just now, so no thread is quiet and the thread rules stay out of the way.
cat > /home/agent/fake-screens/fednet <<'FEDNET'
#!/bin/sh
printf '%s\n--\n' "$*" >> /home/agent/screens-fednet.log
case "$2" in
  read-thread) printf '{"messages":[{"ts":"%s.000100","user":"U0ABC","text":"b"}]}\n' "$(date +%s)" ;;
  *) echo m-posted ;;
esac
FEDNET
chmod +x /home/agent/fake-screens/atb /home/agent/fake-screens/fednet
: > /home/agent/screens-atb.log
: > /home/agent/screens-fednet.log
uwatch() { PATH=/home/agent/fake-screens:$PATH "$T" --scope "$SCOPE" watch; }
KS=C0SCR/1700000000.000

for name in thread-s s-lead s-perm s-trust l-lead; do
  check "unblock: $name starts idle" idle "$(start_fake "$name")"
done
touch "$fake/unknown-screen"
start_fake s-new >/dev/null
start_fake s-young >/dev/null
rm -f "$fake/unknown-screen"
now=$(date +%s)
sqlite3 "$DB" >/dev/null <<SQL
INSERT INTO jobs (job, lead_cwd, home_thread, state, started_at) VALUES
    ('s', '/home/agent', '$KS', 'open', $now),
    ('l', '/home/agent', '$KS', 'open', $now);
INSERT INTO agents (name, role, job, parent, cwd, thread, state, started_at, pane_id) VALUES
    ('thread-s', 'thread', '', '', '/home/agent', '$KS', 'active', $now, ''),
    ('s-lead', 'lead', 's', 'thread-s', '/home/agent', '', 'active', $now, ''),
    ('s-perm', 'worker', 's', 's-lead', '/home/agent', '', 'active', $now, ''),
    ('s-trust', 'worker', 's', 's-lead', '/home/agent', '', 'active', $now, ''),
    ('l-lead', 'lead', 'l', 'thread-s', '/home/agent', '', 'active', $now, ''),
    ('s-new', 'worker', 's', 's-lead', '/home/agent', '', 'starting', $((now - 3600)), '$(agent_field s-new pane_id)'),
    ('s-young', 'worker', 's', 's-lead', '/home/agent', '', 'starting', $now, '$(agent_field s-young pane_id)');
SQL
for arm in s-perm:BLOCK s-trust:TRUST l-lead:LIMIT; do
  "${S[@]}" agent prompt "${arm%%:*}" "please ${arm#*:}" --wait --until working --timeout 20000 >/dev/null
  "${S[@]}" agent wait "${arm%%:*}" --until blocked --timeout 20000 >/dev/null
  check "unblock: ${arm%%:*} is blocked" blocked "$(agent_field "${arm%%:*}" agent_status)"
done
rowid() { sqlite3 "$DB" "SELECT id FROM agents WHERE name = '$1' AND state != 'ended'"; }
hperm=unblock-$(rowid s-perm) hnew=unblock-$(rowid s-new) hlimit=unblock-$(rowid l-lead)
helpers() { "${S[@]}" agent list | jq -r '[.result.agents[].name | select(startswith("unblock-"))] | sort | join(" ")'; }

uwatch >/home/agent/unblock1.out 2>&1; rc=$?
check "unblock: first run exit 0" 0 "$rc"
check "unblock: the trust prompt for its own directory is answered" yes \
  "$(case "$(agent_field s-trust agent_status)" in idle | done) echo yes ;; *) agent_field s-trust agent_status ;; esac)"
has "unblock: s-trust passed the prompt to its input box" "$(screen s-trust)" "fake reply to: please TRUST"
check "unblock: one helper for each stopped agent no rule answers, none for the others" \
  "$(printf '%s\n' "$hnew" "$hperm" | sort | tr '\n' ' ' | sed 's/ $//')" "$(helpers)"
check "unblock: the helper's row" "unblock||s-perm|SC-9 " \
  "$(ledger "SELECT role, job, parent, issue FROM agents WHERE name = '$hperm' AND state != 'ended'")"
check "unblock: the ticket is labelled blocked-screen in the scope's team and project" \
  "linear create --team SC --project Example project --label blocked-screen --title s-perm stopped at a screen" \
  "$(grep -o '.*stopped at a screen' /home/agent/screens-atb.log | grep s-perm)"
received "$hperm" > /home/agent/unblock-helper1.txt
has "unblock: the helper's first message: the stopped agent, the guidance, the screen and the ticket" \
  "$(cat /home/agent/unblock-helper1.txt)" "herdr agent send-keys s-perm" "## Guidance" "rm -rf build" "SC-9"
hcwd=$(readlink "/proc/$("${S[@]}" pane process-info --pane "$(agent_field "$hperm" pane_id)" | jq -r '.result.process_info.foreground_processes[0].pid')/cwd")
check "unblock: the helper runs in the scope's own directory, outside every checkout" yes \
  "$(case "$hcwd" in /home/agent/dev/* | /home/agent/x-repo/* | /home/agent/wt/* | /home/agent) echo "no: $hcwd" ;; *) [ -d "$hcwd" ] && echo yes ;; esac)"
check "unblock: the usage-limit menu is left as it is" blocked "$(agent_field l-lead agent_status)"
has "unblock: the people are asked about the usage limit in the home thread" "$(cat /home/agent/screens-fednet.log)" \
  "$KS" "l-lead (the lead of job l) is stopped at a screen fleet never answers" "usage limit"
check "unblock: the question waits for them, from the agent's helper name" "pending|$KS " \
  "$(ledger "SELECT state, thread FROM questions WHERE asked_by = '$hlimit'")"
settled thread-s
has "unblock: the home thread's agent has the question" "$(received thread-s)" "[FROM: $hlimit]" \
  "about an agent stopped at a screen" "Nothing to pass on"
settled s-lead
lead_got=$(received s-lead)
has "unblock: the worker's lead is told about the helper" "$lead_got" "[FROM: watch]" "s-perm" "started $hperm"
has "unblock: the starter is told the start stopped at a screen" "$lead_got" "s-new" "has not had its first message"
lacks "unblock: nobody is told about the screen the rule answered" "$lead_got" "s-trust"
lacks "unblock: a start left at a screen just now is its starter's" "$(cat /home/agent/unblock1.out)" "s-young"

# The fake helpers reply and finish their turn. s-perm's writes a question.
settled "$hperm"
settled "$hnew"
qfile=$(sed -n 's/.*Write your question for the people to \(.*\): what the screen shows.*/\1/p' /home/agent/unblock-helper1.txt)
printf 'Press 1 to run rm -rf build? 0xQUESTION\n' > "$qfile"
uwatch >/home/agent/unblock2.out 2>&1; rc=$?
check "unblock: second run exit 0" 0 "$rc"
check "unblock: finished helpers are closed, a new one for the screen still unanswered" "$hnew" "$(helpers)"
check "unblock: s-perm's helper row ended" "ended " "$(ledger "SELECT group_concat(state) FROM agents WHERE name = '$hperm'")"
check "unblock: one helper at a time for s-new" "ended,active " \
  "$(ledger "SELECT group_concat(state) FROM (SELECT state FROM agents WHERE name = '$hnew' ORDER BY id)")"
has "unblock: the helper's question goes to the home thread" "$(cat /home/agent/screens-fednet.log)" \
  "its helper $hperm asks" "0xQUESTION" "Ticket: SC-9"
check "unblock: the helper's question waits for the people" "pending " \
  "$(ledger "SELECT state FROM questions WHERE asked_by = '$hperm'")"
check "unblock: the usage limit is asked about once" 1 "$(grep -c 'fleet never answers' /home/agent/screens-fednet.log)"

# The people answer; the next run gives the answer to a new helper.
sqlite3 "$DB" >/dev/null <<SQL
UPDATE questions SET state = 'answered', answered_at = $(date +%s) WHERE asked_by = '$hperm';
INSERT INTO threads (thread, slug, created_at, last_text, last_user, last_ts)
    VALUES ('$KS', 'c0scr', $now, 'yes, press 1 0xANSWER', 'U0ABC', '1700000100.000');
SQL
uwatch >/home/agent/unblock3.out 2>&1; rc=$?
check "unblock: third run exit 0" 0 "$rc"
has "unblock: a new helper once the people answered" "$(helpers)" "$hperm"
has "unblock: it gets their question and answer" "$(received "$hperm")" "## Earlier on this screen" "0xQUESTION" "0xANSWER"
check "unblock: it keeps the first helper's ticket" "SC-9|2 " \
  "$(ledger "SELECT issue, (SELECT count(*) FROM agents WHERE name = '$hperm') FROM agents WHERE name = '$hperm' AND state != 'ended'")"
check "unblock: no second ticket for s-perm" 1 "$(grep -c 'title s-perm stopped' /home/agent/screens-atb.log)"
for out in /home/agent/unblock1.out /home/agent/unblock2.out /home/agent/unblock3.out; do
  [ "$fail" = 0 ] || { echo "--- $out"; cat "$out"; }
done
"${S[@]}" server stop >/dev/null
SCOPE=${saved[0]} DB=${saved[1]}
S=(herdr --session fleet-$SCOPE)

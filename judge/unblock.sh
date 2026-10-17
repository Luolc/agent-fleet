# Sourced by inside.sh: what `watch` does for agents stopped at a screen,
# in a scope of its own (`screens`, herdr session fleet-screens, which
# this suite starts and stops) with fake
# Claudes, a fake atb and a fake fednet that log their arguments. The
# ledger rows are inserted directly. Arms that must differ:
#   s-trust  the folder-trust prompt for its own directory (TRUST): answered
#            by rule, nobody told
#   s-perm   a command confirmation (BLOCK): a helper (the `unblock` watch's
#            worker, watch-unblock-<row id>), the lead told
#   l-lead   a usage-limit menu (LIMIT): no key, no helper, the people asked;
#            once it gets past the menu, the card is taken back, and its
#            next stop is asked about again
#   s-new    a start left at an unknown screen an hour ago: a helper, the
#            starter told; s-young, left there just now, is the starter's
#   thread-s the home thread's agent, at its input box: left alone
saved=("$SCOPE" "$DB")
SCOPE=screens
S=(herdr --session fleet-$SCOPE)
DB=/home/agent/.local/state/$T/$SCOPE.db
start_server fleet-$SCOPE
echo '{}' | scope_config "$SCOPE"
thr job start setup --task-file "$(task setup 'makes the ledger')" >/dev/null 2>&1
thr job end setup --force >/dev/null 2>&1
check "unblock: the scope's ledger exists" yes "$([ -e "$DB" ] && echo yes || echo no)"
mkdir -p /home/agent/.config/$T /home/agent/fake-screens
# prompts.screen_guidance names a file that is not there: the key is
# retired, so it is accepted, named, and not read.
echo '{"linear": {"team": "SC"}, "unblock": {"project": "Example project"}, "fednet": {"socket": "/home/agent/screens.sock"},
  "prompts": {"screen_guidance": {"append": "missing.md"}}}' | scope_config "$SCOPE"
cat > /home/agent/fake-screens/atb <<'ATB'
#!/bin/sh
echo "$*" >> /home/agent/screens-atb.log
[ "$2" = create ] && echo '{"identifier":"SC-9","url":"https://linear.example.test/SC-9"}'
exit 0
ATB
# The fake fednet logs every call; `read-thread` answers a message posted
# just now, so no thread is quiet and the thread rules stay out of the way;
# `ask` logs the card too and posts card-<n>, n the ref's id.
cat > /home/agent/fake-screens/fednet <<'FEDNET'
#!/bin/sh
[ "$2" = react ] && { echo "$*" >> /home/agent/screens-react.log; exit 0; }
printf '%s\n--\n' "$*" >> /home/agent/screens-fednet.log
case "$2" in
  read-thread) printf '{"messages":[{"ts":"%s.000100","user":"U0ABC","text":"b"}]}\n' "$(date +%s)" ;;
  ask) printf 'card %s\n--\n' "$(cat "${10}")" >> /home/agent/screens-fednet.log
    echo '{"id":"card-'"${8#*:}"'","channel":"C0","ts":"1.0","permalink":"https://slack.example.test/card-'"${8#*:}"'"}' ;;
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
hperm=watch-unblock-$(rowid s-perm) hnew=watch-unblock-$(rowid s-new) hlimit=watch-unblock-$(rowid l-lead)
helpers() { "${S[@]}" agent list | jq -r '[.result.agents[].name | select(startswith("watch-unblock-"))] | sort | join(" ")'; }

uwatch >/home/agent/unblock1.out 2>&1; rc=$?
check "unblock: first run exit 0" 0 "$rc"
has "unblock: the retired key is named and ignored" "$(cat /home/agent/unblock1.out)" \
  "note: prompts.screen_guidance in the scope's config is no longer used and is ignored"
check "unblock: the trust prompt for its own directory is answered" yes \
  "$(case "$(agent_field s-trust agent_status)" in idle | done) echo yes ;; *) agent_field s-trust agent_status ;; esac)"
has "unblock: s-trust passed the prompt to its input box" "$(screen s-trust)" "fake reply to: please TRUST"
check "unblock: one helper for each stopped agent no rule answers, none for the others" \
  "$(printf '%s\n' "$hnew" "$hperm" | sort | tr '\n' ' ' | sed 's/ $//')" "$(helpers)"
check "unblock: the helper's row: a worker with no job, its parent watch" "worker||watch|SC-9 " \
  "$(ledger "SELECT role, job, parent, issue FROM agents WHERE name = '$hperm' AND state != 'ended'")"
check "unblock: the ticket is labelled blocked-screen in the scope's team and project" \
  "linear create --team SC --project Example project --label blocked-screen --title s-perm stopped at a screen" \
  "$(grep -o '.*stopped at a screen' /home/agent/screens-atb.log | grep s-perm)"
received "$hperm" > /home/agent/unblock-helper1.txt
has "unblock: the helper's first message: the worker text, the stopped agent, the guidance, the screen and the ticket" \
  "$(cat /home/agent/unblock-helper1.txt)" "[FROM: watch]" "You are a worker run by fleet: $hperm" "FLEET_PARENT=watch" \
  "herdr agent send-keys s-perm" "## Guidance" "rm -rf build" "SC-9"
hcwd=$(readlink "/proc/$("${S[@]}" pane process-info --pane "$(agent_field "$hperm" pane_id)" | jq -r '.result.process_info.foreground_processes[0].pid')/cwd")
check "unblock: the helper runs in the scope's own directory, outside every checkout" yes \
  "$(case "$hcwd" in /home/agent/dev/* | /home/agent/x-repo/* | /home/agent/wt/* | /home/agent) echo "no: $hcwd" ;; *) [ -d "$hcwd" ] && echo yes ;; esac)"
check "unblock: the helper's scratch variable is the directory it runs in" "${P}SCRATCH=$hcwd" \
  "$(proc_env "$hperm" | grep -o "${P}SCRATCH=[^ ]*")"
check "unblock: the usage-limit menu is left as it is" blocked "$(agent_field l-lead agent_status)"
has "unblock: the people are asked about the usage limit on a card in the home thread" "$(cat /home/agent/screens-fednet.log)" \
  "client ask -socket /home/agent/screens.sock -thread $KS -ref $SCOPE:" \
  "l-lead (the lead of job l) is stopped at a screen fleet never answers" "usage limit" \
  '{"short":"I'"'"'ll deal with it",' '"recommended":true},{"short":"End job l",'
lacks "unblock: nothing is posted as text" "$(cat /home/agent/screens-fednet.log)" "client post"
check "unblock: the question waits for them, from the agent's helper name" "pending|$KS " \
  "$(ledger "SELECT state, thread FROM questions WHERE asked_by = '$hlimit'")"
settled thread-s
has "unblock: the home thread's agent has the question" "$(received thread-s)" "[FROM: $hlimit]" \
  "about an agent stopped at a screen" "Nothing to pass on"
settled s-lead
lead_got=$(received s-lead)
has "unblock: the worker's lead is told about the helper" "$lead_got" "[FROM: watch]" "s-perm" "started $hperm"
has "unblock: the starter is told the start stopped at a screen" "$lead_got" "s-new" "may not have its task"
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

# The people press an answer on the card; the next run gives it to a new
# helper.
qid=$(sqlite3 "$DB" "SELECT id FROM questions WHERE asked_by = '$hperm'")
printf '{"msg_id":"ua1","payload":{"type":"answer","answer":{"question_id":"%s:%s","card_id":"card-%s","outcome":"answered","choice":{"index":0,"short":"Press 1"},"note":"yes, press 1 0xANSWER","by":"U0ABC","at":"2026-10-16T10:00:00-07:00"}}}\n' \
  "$SCOPE" "$qid" "$qid" > /home/agent/unblock-answer.json
PATH=/home/agent/fake-screens:$PATH "$T" inbox /home/agent/unblock-answer.json >/home/agent/unblock-answer.out 2>&1; rc=$?
check "unblock: the press is taken: exit 0, the question answered" "0 answered " \
  "$rc $(ledger "SELECT state FROM questions WHERE asked_by = '$hperm'")"
settled thread-s
has "unblock: the home thread's agent gets the press, naming who asked" "$(received thread-s)" "[ANSWER #$qid]" \
  "for $hperm (question #$qid), not one of yours" "0xANSWER"
uwatch >/home/agent/unblock3.out 2>&1; rc=$?
check "unblock: third run exit 0" 0 "$rc"
has "unblock: a new helper once the people answered" "$(helpers)" "$hperm"
has "unblock: it gets their question and the press" "$(received "$hperm")" "## Earlier" "0xQUESTION" \
  "The last press on its card:" '"choice":{"index":0,"short":"Press 1"},"note":"yes, press 1 0xANSWER"'
check "unblock: it keeps the first helper's ticket" "SC-9|2 " \
  "$(ledger "SELECT issue, (SELECT count(*) FROM agents WHERE name = '$hperm') FROM agents WHERE name = '$hperm' AND state != 'ended'")"
check "unblock: no second ticket for s-perm" 1 "$(grep -c 'title s-perm stopped' /home/agent/screens-atb.log)"
# l-lead gets past its usage limit; the next run takes back the card it
# was asked about, and nothing waits on it any more.
qlimit=$(sqlite3 "$DB" "SELECT id FROM questions WHERE asked_by = '$hlimit'")
"${S[@]}" agent send-keys l-lead Enter >/dev/null
"${S[@]}" agent wait l-lead --until idle --until done --timeout 20000 >/dev/null
uwatch >/home/agent/unblock-recovered.out 2>&1; rc=$?
check "unblock: the run after l-lead recovered exits 0" 0 "$rc"
has "unblock: l-lead's card is taken back from Slack" "$(cat /home/agent/screens-fednet.log)" \
  "client ask-close -socket /home/agent/screens.sock -note fleet closed this question: the agent is no longer stopped" \
  "-- card-$qlimit"
check "unblock: l-lead's question is closed" "closed " "$(ledger "SELECT state FROM questions WHERE id = $qlimit")"
check "unblock: the questions of the agents still stopped are not taken back" 1 \
  "$(grep -c 'client ask-close' /home/agent/screens-fednet.log)"
# l-lead stops at a usage limit again: a new stop, asked about again.
"${S[@]}" agent prompt l-lead "please LIMIT" --wait --until working --timeout 20000 >/dev/null
"${S[@]}" agent wait l-lead --until blocked --timeout 20000 >/dev/null
uwatch >/home/agent/unblock-again.out 2>&1; rc=$?
check "unblock: the run after l-lead stopped again exits 0" 0 "$rc"
check "unblock: l-lead's second stop is asked about on a new card" "2 1" \
  "$(grep -c 'l-lead is stopped at a screen fleet never answers' /home/agent/screens-fednet.log) $(ledger "SELECT count(*) FROM questions WHERE asked_by = '$hlimit' AND state = 'pending'" | tr -d ' ')"
# An unblock watch prompt with a slot the prompt does not have: no
# helper and no ticket for a newly stopped agent; its home thread is told
# once, naming the file and line, and the next run does not say it again.
start_fake s-bad >/dev/null
sqlite3 "$DB" "INSERT INTO agents (name, role, job, parent, cwd, state, started_at, pane_id)
  VALUES ('s-bad', 'worker', 's', 's-lead', '/home/agent', 'active', $now, '')"
"${S[@]}" agent prompt s-bad "please BLOCK" --wait --until working --timeout 20000 >/dev/null
"${S[@]}" agent wait s-bad --until blocked --timeout 20000 >/dev/null
hbad=watch-unblock-$(rowid s-bad)
mkdir -p "/home/agent/.config/$T/prompts"
printf 'Say hello.\nThen read {{repo}}.\n' > "/home/agent/.config/$T/prompts/unblock.md"
cp "/home/agent/.config/$T/$SCOPE.json" /home/agent/screens.json.keep
jq -c '. + {"watches": {"unblock": {"prompt": "prompts/unblock.md"}}}' /home/agent/screens.json.keep \
  > "/home/agent/.config/$T/$SCOPE.json"
uwatch >/home/agent/unblock4.out 2>&1; rc=$?
check "unblock: a broken unblock prompt fails the run, exit 1" 1 "$rc"
has "unblock: the run names the file, line and slot" "$(cat /home/agent/unblock4.out)" \
  "watches.unblock.prompt (~/.config/$T/prompts/unblock.md:2:11): unknown slot {{repo}}"
check "unblock: no helper and no ticket for s-bad" "0 0" \
  "$(ledger "SELECT count(*) FROM agents WHERE name = '$hbad'")$(grep -c 'title s-bad stopped' /home/agent/screens-atb.log)"
has "unblock: the home thread is told what was not started and why" "$(cat /home/agent/screens-fednet.log)" \
  "-thread $KS -- fleet watch did not start a screen helper for" "fleet's prompt config is broken (watches.unblock.prompt"
uwatch >/home/agent/unblock5.out 2>&1; rc=$?
check "unblock: the next run fails the same way" 1 "$rc"
check "unblock: the thread is told once" 1 "$(grep -c "fleet's prompt config is broken" /home/agent/screens-fednet.log)"
mv /home/agent/screens.json.keep "/home/agent/.config/$T/$SCOPE.json"
for out in /home/agent/unblock1.out /home/agent/unblock2.out /home/agent/unblock3.out /home/agent/unblock-recovered.out /home/agent/unblock-again.out /home/agent/unblock4.out /home/agent/unblock5.out; do
  [ "$fail" = 0 ] || { echo "--- $out"; cat "$out"; }
done
"${S[@]}" server stop >/dev/null
SCOPE=${saved[0]} DB=${saved[1]}
S=(herdr --session fleet-$SCOPE)

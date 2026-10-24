# Sourced by inside.sh: `watch`'s thread and question rules against the real
# herdr, in a scope of their own (`quiet`, herdr session fleet-quiet, which
# this suite starts and stops), with a fake fednet whose `read-thread`
# answers the newest message's time from a file per thread, and the clock
# moved through <prefix>WATCH_NOW. The fake's `post` refuses -mention in
# Q4's thread as a hub older than its client does, `delete` deletes, `ask`
# posts a card linked by its ref and `ask-close` takes it back.
# Six threads, each started by `inbox` (a message from U0ABC):
# Q1 has its thread agent's own question, a card (reminders, then the
# session reclaimed and the card taken back); Q2 a job (asked about each quiet spell, then why it has not
# ended, then reclaimed when idle); Q3 a lead's question (its thread agent
# reminded, never the people; the job ended by force); Q4 a thread agent
# that asks in plain text as an older prompt does, posted as a card, and
# vanishes (the session ended as broken off, the people reminded); Q5 a job whose thread agent ended (watch starts one to ask
# about it); Q6 a job its lead ended with its question pending (the
# question moves to the thread: its agent told and reminded, closed without
# reclaiming); Q7 a job whose lead vanishes (its row ended after 10
# minutes, the thread agent told once, the job left open).
need_repo
QS=(herdr --session fleet-quiet)
start_server fleet-quiet
QD=/home/agent/quiet
QB=$QD/bin
QDB=/home/agent/.local/state/$T/quiet.db
mkdir -p "$QB" "$QD/threads" /home/agent/.config/$T /home/agent/events /home/agent/run
echo '{"fednet": {"socket": "/home/agent/quiet/fednet.sock"}}' | scope_config quiet
cat > "$QB/fednet" <<'FEDNET'
#!/bin/sh
[ "$2" = react ] && { echo "$*" >> /home/agent/quiet/react.log; exit 0; }
d=/home/agent/quiet
echo "$*" >> "$d/fednet.log"
case "$2" in
  read-thread)
    for last; do :; done
    f="$d/threads/$(printf %s "$last" | tr / _)"
    [ -f "$f" ] || { echo "read-thread: no thread $last" >&2; exit 1; }
    printf '{"messages":[{"ts":"1700000001.000","user":"U0ABC","text":"a"},{"ts":"%s","user":"U0ABC","text":"b"}]}\n' "$(cat "$f")" ;;
  post)
    case " $* " in *" -thread C0704/4.4 -mention "*)
      echo 'post: unknown command "check-mentions" (the hub runs an older fednet than this client; upgrade the hub)' >&2
      exit 2 ;;
    esac
    echo "m-$(wc -l < "$d/fednet.log")" ;;
  delete|ask-close) ;;
  ask) echo '{"id":"card-'"$8"'","channel":"C0","ts":"1.0","permalink":"https://slack.example.test/'"$8"'"}' ;;
esac
FEDNET
printf '#!/bin/sh\necho "$*" >> /home/agent/quiet/atb.log\n' > "$QB/atb"
chmod +x "$QB/fednet" "$QB/atb"
qledger() { sqlite3 "$QDB" "$1" | tr '\n' ' '; }
qreminder() { sqlite3 "$QDB" "SELECT reminder_msg FROM questions WHERE thread = '$1'"; }
qname() { echo "thread-$(echo "$1" | tr '/.' '--' | tr '[:upper:]' '[:lower:]')"; }
qfield() { "${QS[@]}" agent get "$1" 2>&1 | jq -r ".result.agent.$2 // .error.code"; }
# Everything an agent received; the thread keys and markers are this
# suite's own, so a pane id another session also used does not matter.
qreceived() { screen_q "$1" >/dev/null; cat "$fake/received-$(qfield "$1" pane_id)" 2>/dev/null; }
screen_q() {
  local out
  for _ in $(seq 1 75); do
    out=$("${QS[@]}" agent read "$1" --source visible 2>/dev/null)
    case "$out" in *"esc to interrupt"*) sleep 0.2 ;; *) break ;; esac
  done
  printf '%s\n' "$out"
}
count() { printf '%s\n' "$1" | grep -cF -- "$2"; }
qinbox() { # <msg_id> <thread>
  printf '{"msg_id":"%s","payload":{"type":"message","thread":"%s","text":"start %s","user":"U0ABC","ts":"1700000001.000","channel_name":"repo-%s","scope":"quiet"}}\n' \
    "$1" "$2" "$1" "$R" > "/home/agent/events/$1.json"
  PATH=$QB:$PATH XDG_RUNTIME_DIR=/home/agent/run "$T" inbox "/home/agent/events/$1.json"
}
qthr() { # <thread> -- <arguments...>: as the thread's agent
  local key=$1
  shift 2
  PATH=$QB:$PATH env "${P}AGENT=$(qname "$key")" "${P}ROLE=thread" "${P}PARENT=" "${P}SCOPE=quiet" "${P}JOB=" \
    "${P}ISSUE=" "${P}THREAD=$key" "$T" "$@"
}
qlead() { # <job> <thread> -- <arguments...>: as the job's lead
  local job=$1 key=$2
  shift 3
  PATH=$QB:$PATH env "${P}AGENT=$job-lead" "${P}ROLE=lead" "${P}PARENT=$(qname "$key")" "${P}SCOPE=quiet" \
    "${P}JOB=$job" "${P}ISSUE=" "$T" "$@"
}
last_msg() { printf '%s.000100\n' "$2" > "$QD/threads/$(printf %s "$1" | tr / _)"; }

Q1=C0701/1.1 Q2=C0702/2.2 Q3=C0703/3.3 Q4=C0704/4.4 Q5=C0705/5.5 Q6=C0706/6.6 Q7=C0707/7.7
for q in 1 2 3 4 5 6 7; do
  key=C070$q/$q.$q
  qinbox "wq$q" "$key" >/home/agent/quiet/inbox$q.out 2>&1; rc=$?
  check "watch threads: inbox starts the agent of Q$q" "0 $(qname "$key")" "$rc $(qfield "$(qname "$key")" name)"
  [ "$rc" = 0 ] || cat /home/agent/quiet/inbox$q.out
done
printf '{"question": "Which month should the import cover? 0xQW1"}\n' > "$QD/q1.json"
qthr "$Q1" -- ask-human --card "$QD/q1.json" >/dev/null 2>&1
check "watch threads: Q1's agent asked" "pending " "$(qledger "SELECT state FROM questions WHERE thread = '$Q1'")"
for q in 2 3 5 6 7; do
  key=C070$q/$q.$q
  out=$(qthr "$key" -- job start "q$q" --repo "$R" --task-file "$(task "q$q" "job of Q$q")" 2>&1); rc=$?
  check "watch threads: job q$q started from Q$q" 0 "$rc"
  [ "$rc" = 0 ] || printf '%s\n' "$out"
done
printf 'Merge the import? 0xQW3\n' > "$QD/q3.md"
qlead q3 "$Q3" -- ask-thread --card "$QD/q3.md" >/dev/null 2>&1
check "watch threads: q3's lead asked" "pending " "$(qledger "SELECT state FROM questions WHERE job = 'q3'")"
printf 'Which schema should the follow-up use? 0xQW6\n' > "$QD/q6.md"
qlead q6 "$Q6" -- ask-thread --card "$QD/q6.md" >/dev/null 2>&1
printf 'Done; the schema question is for the follow-up. 0xQ6DONE\n' > "$QD/report6.md"
qlead q6 "$Q6" -- job end --report-file "$QD/report6.md" >/dev/null 2>&1
check "watch threads: q6 ended by its lead, its question pending" "ended pending " \
  "$(qledger "SELECT state FROM jobs WHERE job = 'q6'")$(qledger "SELECT state FROM questions WHERE job = 'q6'")"
printf 'Is the export still needed? 0xQW4\n' > "$QD/q4.md"
qthr "$Q4" -- ask-human --file "$QD/q4.md" >/dev/null 2>&1
"${QS[@]}" pane close "$(qfield "$(qname "$Q4")" pane_id)" >/dev/null
check "watch threads: Q4's agent is gone" agent_not_found "$(qfield "$(qname "$Q4")" name)"
printf 'Started q5.\n' > "$QD/summary5.md"
qthr "$Q5" -- thread end --summary-file "$QD/summary5.md" --asked-to-end >/dev/null 2>&1
check "watch threads: Q5's session ended with its job open" "agent_not_found open " \
  "$(qfield "$(qname "$Q5")" name) $(qledger "SELECT state FROM jobs WHERE job = 'q5'")"
"${QS[@]}" pane close "$(qfield q7-lead pane_id)" >/dev/null
check "watch threads: q7's lead is gone" agent_not_found "$(qfield q7-lead name)"

T0=$(date +%s)
for q in 1 2 3 4 5 7; do last_msg "C070$q/$q.$q" "$T0"; done
last_msg "$Q6" $((T0 + 60 * 60))
: > "$QD/fednet.log"
qwatch() { # <minutes>: one run at T0 + minutes, its output in watch-<minutes>.out
  PATH=$QB:$PATH env "${P}WATCH_NOW=$((T0 + $1 * 60))" "$T" --scope quiet watch >"$QD/watch-$1.out" 2>&1; rc=$?
  check "watch threads: run at +$1m exit 0" 0 "$rc"
  [ "$rc" = 0 ] || cat "$QD/watch-$1.out"
}
quiet2="rule \`thread quiet, jobs open\`: thread $Q2"
gone7="rule \`lead gone\`: q7-lead, the lead of job q7"

qwatch 29
has "watch threads: the vanished agent's session ended, the thread told" "$(cat "$QD/fednet.log")" \
  "client post -socket /home/agent/quiet/fednet.sock -thread $Q4 -footer -- 会话意外中断了，再说话会重新开始"
check "watch threads: the vanished agent's row ended" "ended " "$(qledger "SELECT state FROM agents WHERE name = '$(qname "$Q4")'")"
lacks "watch threads: no reminder before 30m" "$(cat "$QD/fednet.log")" "-- Reminder"
check "watch threads: no quiet notice before 30m" 0 "$(count "$(qreceived "$(qname "$Q2")")" "$quiet2")"
has "watch threads: Q6's agent is told its ended job's question is the thread's now" "$(qreceived "$(qname "$Q6")")" \
  "[FROM: watch]" "rule \`questions of an ended job\`: 1 question(s) a lead asked in thread $Q6" "- job q6, from q6-lead, asked at " \
  "Which schema should the follow-up use? 0xQW6"
check "watch threads: the moved question stays pending, still q6's" "q6|q6-lead|pending " \
  "$(qledger "SELECT job || '|' || asked_by || '|' || state FROM questions WHERE thread = '$Q6'")"
out=$(qthr "$Q6" -- thread end --summary-file "$QD/summary5.md" 2>&1); rc=$?
check "watch threads: Q6's session may not end while the moved question waits" 1 "$rc"
has "watch threads: and the refusal names it" "$out" "question from q6-lead, pending: Which schema should the follow-up use? 0xQW6"

qwatch 30
check "watch threads: at 30m one reminder for Q1, mentioning who wrote last" 1 \
  "$(grep -c "thread $Q1 -mention U0ABC -- Reminder 1 of 3" "$QD/fednet.log")"
has "watch threads: the reminder links the card" "$(cat "$QD/fednet.log")" "- from $(qname "$Q1"), card #1: https://slack.example.test/quiet:1"
lacks "watch threads: the people are not reminded of a lead's question (0xQW3)" "$(cat "$QD/fednet.log")" "0xQW3"
lacks "watch threads: the people are not reminded of a lead's question (0xQW6)" "$(cat "$QD/fednet.log")" "0xQW6"
has "watch threads: Q3's agent is reminded of the lead's question" "$(qreceived "$(qname "$Q3")")" \
  "[FROM: watch]" "rule \`question reminder\` (1 of 3): 1 question(s) leads asked you in thread $Q3" "- from q3-lead: Merge the import? 0xQW3"
has "watch threads: Q6's agent is reminded of the moved question" "$(qreceived "$(qname "$Q6")")" \
  "rule \`question reminder\` (1 of 3): 1 question(s) leads asked you in thread $Q6" "acting on the answer is yours" "- from q6-lead (job q6 ended): Which schema should the follow-up use? 0xQW6"
check "watch threads: the reminder is counted with its msg_id" "1|m- " "$(qledger "SELECT reminders || '|' || substr(reminder_msg, 1, 2) FROM questions WHERE thread = '$Q1'")"
q1first=$(qreminder "$Q1")
check "watch threads: and kept as the thread's reminder" "$q1first|posted " "$(qledger "SELECT msg_id || '|' || state FROM reminders WHERE thread = '$Q1'")"
lacks "watch threads: the first reminder deletes nothing" "$(cat "$QD/fednet.log")" "client delete"
has "watch threads: Q4's question is reminded of with its agent gone" "$(cat "$QD/fednet.log")" \
  "-thread $Q4 -- Reminder 1 of 3" "card #4: https://slack.example.test/quiet:4"
has "watch threads: Q4's older hub gets its reminder without the mention" "$(cat "$QD/watch-30.out")" \
  "the reminder in thread $Q4 is posted without mentioning anyone" "the hub runs an older fednet than this client"
has "watch threads: Q2's agent asked about the quiet with its job's state" "$(qreceived "$(qname "$Q2")")" \
  "[FROM: watch]" "$quiet2" "- job q2: lead q2-lead "
lacks "watch threads: a thread with a pending question is not asked about the quiet" "$(qreceived "$(qname "$Q1")")" "rule \`thread quiet"
has "watch threads: Q5 got a new agent, asked about its job" "$(qreceived "$(qname "$Q5")")" \
  "[FROM: watch]" "rule \`thread quiet, jobs open\`: thread $Q5" "- job q5: lead q5-lead "
check "watch threads: Q5's new session is live" "active " "$(qledger "SELECT state FROM agents WHERE name = '$(qname "$Q5")' AND state != 'ended'")"

check "watch threads: q7's lead, gone for 1 minute, keeps its row" "active " \
  "$(qledger "SELECT state FROM agents WHERE name = 'q7-lead'")"
check "watch threads: and Q7's agent is not told" 0 "$(count "$(qreceived "$(qname "$Q7")")" "$gone7")"

qwatch 40
check "watch threads: q7's lead, gone for 11 minutes, has its row ended, its job left open" "ended open " \
  "$(qledger "SELECT state FROM agents WHERE name = 'q7-lead'")$(qledger "SELECT state FROM jobs WHERE job = 'q7'")"
has "watch threads: Q7's agent is told, to decide about the job" "$(qreceived "$(qname "$Q7")")" \
  "[FROM: watch]" "$gone7" "is gone from herdr for 11m" "fleet job end q7 --force"
check "watch threads: Q6's agent is told once" 1 "$(count "$(qreceived "$(qname "$Q6")")" "rule \`questions of an ended job\`")"
check "watch threads: the same quiet spell is asked about once" 1 "$(count "$(qreceived "$(qname "$Q2")")" "$quiet2")"
last_msg "$Q2" $((T0 + 50 * 60))
qwatch 79
check "watch threads: a new message restarts the count" 1 "$(count "$(qreceived "$(qname "$Q2")")" "$quiet2")"
qwatch 80
check "watch threads: asked again after 30m of the new spell" 2 "$(count "$(qreceived "$(qname "$Q2")")" "$quiet2")"
check "watch threads: Q7's agent is told once that its lead is gone" 1 "$(count "$(qreceived "$(qname "$Q7")")" "$gone7")"

printf 'Done. 0xQ2DONE\n' > "$QD/report2.md"
qlead q2 "$Q2" -- job end --report-file "$QD/report2.md" >/dev/null 2>&1
check "watch threads: q2 ended by its lead" "ended " "$(qledger "SELECT state FROM jobs WHERE job = 'q2'")"
last_msg "$Q2" $((T0 + 90 * 60))
qwatch 120
nothing2="rule \`thread quiet, nothing open\`: thread $Q2"
has "watch threads: with nothing open the live agent is asked why it has not ended" "$(qreceived "$(qname "$Q2")")" "$nothing2"
qwatch 150
check "watch threads: and asked once" 1 "$(count "$(qreceived "$(qname "$Q2")")" "$nothing2")"

qwatch 180
has "watch threads: the second reminder deletes the first" "$(cat "$QD/fednet.log")" \
  "client delete -socket /home/agent/quiet/fednet.sock -- $q1first"
check "watch threads: and the ledger says so" "$q1first|deleted $(qreminder "$Q1")|posted " \
  "$(qledger "SELECT msg_id || '|' || state FROM reminders WHERE thread = '$Q1' ORDER BY id")"
qwatch 1440
qwatch 2880
check "watch threads: three reminders each at 30m, 3h and 24h, no more" "3 3 3" \
  "$(grep -c "thread $Q1 -mention U0ABC -- Reminder" "$QD/fednet.log") $(grep -c "thread $Q4 -- Reminder" "$QD/fednet.log") $(count "$(qreceived "$(qname "$Q3")")" "rule \`question reminder\`")"
has "watch threads: the third reminder says so" "$(cat "$QD/fednet.log")" "thread $Q1 -mention U0ABC -- Reminder 3 of 3"
check "watch threads: only the last reminder is left in each thread" "deleted deleted posted |deleted deleted posted " \
  "$(qledger "SELECT state FROM reminders WHERE thread = '$Q1' ORDER BY id")|$(qledger "SELECT state FROM reminders WHERE thread = '$Q4' ORDER BY id")"
check "watch threads: Q6's agent is reminded of the moved question three times too, the people never" "3 0" \
  "$(count "$(qreceived "$(qname "$Q6")")" "rule \`question reminder\`") $(grep -c "thread $Q6 -- Reminder" "$QD/fednet.log")"

qwatch 4320
check "watch threads: at 72h the thread agent's question is closed" "closed " "$(qledger "SELECT state FROM questions WHERE thread = '$Q1'")"
has "watch threads: and its session asked to end" "$(qreceived "$(qname "$Q1")")" "[FROM: watch]" "rule \`thread question timeout\`" "--asked-to-end"
has "watch threads: and its card taken back" "$(cat "$QD/fednet.log")" "client ask-close -socket /home/agent/quiet/fednet.sock -note fleet closed this question -- card-quiet:1"
has "watch threads: the lead of the unanswered question is told its job ends" "$(qreceived q3-lead)" \
  "[FROM: watch]" "rule \`lead question timeout\`" "Job q3 is ended by force at"
check "watch threads: at 72h the moved question is closed, Q6's session kept" "closed none " \
  "$(qledger "SELECT state FROM questions WHERE thread = '$Q6'")$(qledger "SELECT coalesce(reclaim_at, 'none') FROM agents WHERE name = '$(qname "$Q6")' AND state = 'active'")"
lacks "watch threads: Q6's agent is not asked to end for it" "$(qreceived "$(qname "$Q6")")" "rule \`thread question timeout\`"
has "watch threads: watch says so" "$(cat "$QD/watch-4320.out")" "closed 1 question(s) of ended jobs in thread $Q6"
has "watch threads: Q5, without a message for 72h, is reclaimed" "$(qreceived "$(qname "$Q5")")" "rule \`thread idle\`: thread $Q5 has had no message for 3d00h"
qwatch 4329
check "watch threads: Q1's session lives until the 10 minutes are up" "active " "$(qledger "SELECT state FROM agents WHERE name = '$(qname "$Q1")' ORDER BY id DESC LIMIT 1")"
qwatch 4330
check "watch threads: then fleet ends it: row ended, agent gone" "ended agent_not_found" \
  "$(qledger "SELECT state FROM agents WHERE name = '$(qname "$Q1")' ORDER BY id DESC LIMIT 1")$(qfield "$(qname "$Q1")" name)"
has "watch threads: and posts the closing line" "$(cat "$QD/fednet.log")" "-thread $Q1 -footer -- 会话长时间没有动静，已被回收，再说话会重新开始"
check "watch threads: q3 still open before its 30 minutes" "open " "$(qledger "SELECT state FROM jobs WHERE job = 'q3'")"
qwatch 4350
check "watch threads: q3 ended by force, its question closed" "ended|abandoned closed " \
  "$(qledger "SELECT state || '|' || outcome FROM jobs WHERE job = 'q3'")$(qledger "SELECT state FROM questions WHERE job = 'q3'")"
check "watch threads: q3's lead is gone with its workspace" agent_not_found "$(qfield q3-lead name)"
has "watch threads: the thread is told" "$(cat "$QD/fednet.log")" "-thread $Q3 -- Job q3 was ended by force: its lead's question had no answer for 3d00h"
qwatch 4500
has "watch threads: Q2, 72h after its last message, is reclaimed" "$(qreceived "$(qname "$Q2")")" "rule \`thread idle\`: thread $Q2 has had no message for 3d01h"

# Thread tabs: in the threads workspace, a tab a failed start of Q8 left
# (an ended row of Q8 records its pane), one of Q9 whose row is still
# `starting` long after its start, and two that watch leaves: one no row
# records, and one of Q11 whose shell runs a process in the background.
qws=$("${QS[@]}" workspace list | jq -r '.result.workspaces[] | select(.label == "threads") | .workspace_id')
qtab() { "${QS[@]}" tab create --workspace "$qws" --label "$1" --no-focus --cwd /home/agent | jq -r .result.root_pane.pane_id; }
p8=$(qtab c0708-8-8) p9=$(qtab c0709-9-9) p10=$(qtab c0710-10-10) p11=$(qtab c0711-11-11)
"${QS[@]}" pane run "$p11" 'sleep 600 &' >/dev/null
now=$(date +%s)
sqlite3 "$QDB" "INSERT INTO agents (name, role, state, started_at, ended_at, thread, pane_id) VALUES
    ('$(qname C0708/8.8)', 'thread', 'ended', $now, $now, 'C0708/8.8', '$p8'),
    ('$(qname C0711/11.11)', 'thread', 'ended', $now, $now, 'C0711/11.11', '$p11');
  INSERT INTO agents (name, role, state, started_at, thread, pane_id) VALUES
    ('$(qname C0709/9.9)', 'thread', 'starting', $now, 'C0709/9.9', '$p9')"
there() { for p in "$@"; do "${QS[@]}" pane get "$p" 2>&1 | jq -r '.error.code // "there"'; done | tr '\n' ' '; }
qwatch 4510
has "watch thread tabs: the row left starting is ended" "$(cat "$QD/watch-4510.out")" \
  "watch: ended $(qname C0709/9.9) (thread), starting since"
check "watch thread tabs: no tab closed in the first run that finds them" "there there there there " "$(there "$p8" "$p9" "$p10" "$p11")"
qwatch 4515
check "watch thread tabs: nor 5 minutes later" "there there there there " "$(there "$p8" "$p9" "$p10" "$p11")"
qwatch 4520
check "watch thread tabs: 10 minutes later the ended sessions' tabs are closed, the other two left" \
  "pane_not_found pane_not_found there there " "$(there "$p8" "$p9" "$p10" "$p11")"
has "watch thread tabs: each close is a line on stderr" "$(cat "$QD/watch-4520.out")" \
  "watch: closed tab " "(c0708-8-8) of thread C0708/8.8: its pane $p8 is an ended session's" \
  "(c0709-9-9) of thread C0709/9.9: its pane $p9 is an ended session's"
check "watch thread tabs: status lists the tabs left, with why" \
  "c0710-10-10 false no thread agent's ended row records its pane|c0711-11-11 false child process(es)|" \
  "$("$T" --scope quiet status --json | jq -r '.orphan_tabs[] | "\(.label) \(.watch_closes) \(.why)"' |
    sed 's/its shell (pid [0-9]*) has [0-9]* //' | tr '\n' '|')"

"${QS[@]}" server stop >/dev/null

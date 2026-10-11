# Sourced by inside.sh: `watch`'s thread and question rules against the real
# herdr, in a scope of their own (`quiet`, herdr session fleet-quiet, which
# this suite starts and stops), with a fake fednet whose `read-thread`
# answers the newest message's time from a file per thread, and the clock
# moved through <prefix>WATCH_NOW. Five threads, each started by `inbox`:
# Q1 has its thread agent's own question (reminders, then the session
# reclaimed); Q2 a job (asked about each quiet spell, then why it has not
# ended, then reclaimed when idle); Q3 a lead's question (the job ended by
# force); Q4 a thread agent that vanishes (the session ended as broken off);
# Q5 a job whose thread agent ended (watch starts one to ask about it).
need_repo
QS=(herdr --session fleet-quiet)
herdr --session fleet-quiet server >/home/agent/server-fleet-quiet.log 2>&1 &
for _ in $(seq 1 100); do "${QS[@]}" status server >/dev/null 2>&1 && break; sleep 0.2; done
QD=/home/agent/quiet
QB=$QD/bin
QDB=/home/agent/.local/state/$T/quiet.db
mkdir -p "$QB" "$QD/threads" /home/agent/.config/$T /home/agent/events /home/agent/run
echo '{"fednet": {"socket": "/home/agent/quiet/fednet.sock"}}' > "/home/agent/.config/$T/quiet.json"
cat > "$QB/fednet" <<'FEDNET'
#!/bin/sh
d=/home/agent/quiet
echo "$*" >> "$d/fednet.log"
case "$2" in
  read-thread)
    for last; do :; done
    f="$d/threads/$(printf %s "$last" | tr / _)"
    [ -f "$f" ] || { echo "read-thread: no thread $last" >&2; exit 1; }
    printf '{"messages":[{"ts":"1700000001.000","user":"U0ABC","text":"a"},{"ts":"%s","user":"U0ABC","text":"b"}]}\n' "$(cat "$f")" ;;
  post) echo "m-$(wc -l < "$d/fednet.log")" ;;
esac
FEDNET
printf '#!/bin/sh\necho "$*" >> /home/agent/quiet/atb.log\n' > "$QB/atb"
chmod +x "$QB/fednet" "$QB/atb"
qledger() { sqlite3 "$QDB" "$1" | tr '\n' ' '; }
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

Q1=C0701/1.1 Q2=C0702/2.2 Q3=C0703/3.3 Q4=C0704/4.4 Q5=C0705/5.5
for q in 1 2 3 4 5; do
  key=C070$q/$q.$q
  qinbox "wq$q" "$key" >/home/agent/quiet/inbox$q.out 2>&1; rc=$?
  check "watch threads: inbox starts the agent of Q$q" "0 $(qname "$key")" "$rc $(qfield "$(qname "$key")" name)"
  [ "$rc" = 0 ] || cat /home/agent/quiet/inbox$q.out
done
printf 'Which month should the import cover? 0xQW1\n' > "$QD/q1.md"
qthr "$Q1" -- ask-human --file "$QD/q1.md" >/dev/null 2>&1
check "watch threads: Q1's agent asked" "pending " "$(qledger "SELECT state FROM questions WHERE thread = '$Q1'")"
for q in 2 3 5; do
  key=C070$q/$q.$q
  out=$(qthr "$key" -- job start "q$q" --repo "$R" --task-file "$(task "q$q" "job of Q$q")" 2>&1); rc=$?
  check "watch threads: job q$q started from Q$q" 0 "$rc"
  [ "$rc" = 0 ] || printf '%s\n' "$out"
done
printf 'Merge the import? 0xQW3\n' > "$QD/q3.md"
qlead q3 "$Q3" -- ask-human --file "$QD/q3.md" >/dev/null 2>&1
check "watch threads: q3's lead asked" "pending " "$(qledger "SELECT state FROM questions WHERE job = 'q3'")"
"${QS[@]}" pane close "$(qfield "$(qname "$Q4")" pane_id)" >/dev/null
check "watch threads: Q4's agent is gone" agent_not_found "$(qfield "$(qname "$Q4")" name)"
printf 'Started q5.\n' > "$QD/summary5.md"
qthr "$Q5" -- thread end --summary-file "$QD/summary5.md" --asked-to-end >/dev/null 2>&1
check "watch threads: Q5's session ended with its job open" "agent_not_found open " \
  "$(qfield "$(qname "$Q5")" name) $(qledger "SELECT state FROM jobs WHERE job = 'q5'")"

T0=$(date +%s)
for q in 1 2 3 4 5; do last_msg "C070$q/$q.$q" "$T0"; done
: > "$QD/fednet.log"
qwatch() { # <minutes>: one run at T0 + minutes, its output in watch-<minutes>.out
  PATH=$QB:$PATH env "${P}WATCH_NOW=$((T0 + $1 * 60))" "$T" --scope quiet watch >"$QD/watch-$1.out" 2>&1; rc=$?
  check "watch threads: run at +$1m exit 0" 0 "$rc"
  [ "$rc" = 0 ] || cat "$QD/watch-$1.out"
}
quiet2="rule \`thread quiet, jobs open\`: thread $Q2"

qwatch 29
has "watch threads: the vanished agent's session ended, the thread told" "$(cat "$QD/fednet.log")" \
  "client post -socket /home/agent/quiet/fednet.sock -thread $Q4 -footer -- 会话意外中断了，再说话会重新开始"
check "watch threads: the vanished agent's row ended" "ended " "$(qledger "SELECT state FROM agents WHERE name = '$(qname "$Q4")'")"
lacks "watch threads: no reminder before 30m" "$(cat "$QD/fednet.log")" "-- Reminder"
check "watch threads: no quiet notice before 30m" 0 "$(count "$(qreceived "$(qname "$Q2")")" "$quiet2")"

qwatch 30
check "watch threads: at 30m one reminder each for Q1 and Q3" "1 1" \
  "$(grep -c "thread $Q1 -- Reminder 1 of 3" "$QD/fednet.log") $(grep -c "thread $Q3 -- Reminder 1 of 3" "$QD/fednet.log")"
has "watch threads: the reminder lists the question" "$(cat "$QD/fednet.log")" "- from $(qname "$Q1"): Which month should the import cover? 0xQW1"
check "watch threads: the reminder is counted with its msg_id" "1|m- " "$(qledger "SELECT reminders || '|' || substr(reminder_msg, 1, 2) FROM questions WHERE thread = '$Q1'")"
has "watch threads: Q2's agent asked about the quiet with its job's state" "$(qreceived "$(qname "$Q2")")" \
  "[FROM: watch]" "$quiet2" "- job q2: lead q2-lead "
lacks "watch threads: a thread with a pending question is not asked about the quiet" "$(qreceived "$(qname "$Q1")")" "rule \`thread quiet"
has "watch threads: Q5 got a new agent, asked about its job" "$(qreceived "$(qname "$Q5")")" \
  "[FROM: watch]" "rule \`thread quiet, jobs open\`: thread $Q5" "- job q5: lead q5-lead "
check "watch threads: Q5's new session is live" "active " "$(qledger "SELECT state FROM agents WHERE name = '$(qname "$Q5")' AND state != 'ended'")"

qwatch 40
check "watch threads: the same quiet spell is asked about once" 1 "$(count "$(qreceived "$(qname "$Q2")")" "$quiet2")"
last_msg "$Q2" $((T0 + 50 * 60))
qwatch 79
check "watch threads: a new message restarts the count" 1 "$(count "$(qreceived "$(qname "$Q2")")" "$quiet2")"
qwatch 80
check "watch threads: asked again after 30m of the new spell" 2 "$(count "$(qreceived "$(qname "$Q2")")" "$quiet2")"

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
qwatch 1440
qwatch 2880
check "watch threads: three reminders each at 30m, 3h and 24h, no more" "3 3" \
  "$(grep -c "thread $Q1 -- Reminder" "$QD/fednet.log") $(grep -c "thread $Q3 -- Reminder" "$QD/fednet.log")"
has "watch threads: the third reminder says so" "$(cat "$QD/fednet.log")" "thread $Q1 -- Reminder 3 of 3"

qwatch 4320
check "watch threads: at 72h the thread agent's question is closed" "closed " "$(qledger "SELECT state FROM questions WHERE thread = '$Q1'")"
has "watch threads: and its session asked to end" "$(qreceived "$(qname "$Q1")")" "[FROM: watch]" "rule \`thread question timeout\`" "--asked-to-end"
has "watch threads: the lead of the unanswered question is told its job ends" "$(qreceived q3-lead)" \
  "[FROM: watch]" "rule \`lead question timeout\`" "Job q3 is ended by force at"
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

"${QS[@]}" server stop >/dev/null

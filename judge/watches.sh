# Sourced by inside.sh: `watch`'s configured watches against the real herdr,
# in a scope of their own (`cfgw`, herdr session fleet-cfgw, which this
# suite starts and stops), with a fake fednet and the clock moved through
# <prefix>WATCH_NOW. One thread, started by `inbox`, with the job cw
# reporting to it. Its config turns the default quiet-threads off and has
# three command watches, all running the same source script, which prints
# the file of subjects its argument names: `ci` (per_run 3, a prompt file,
# one variable passed) tells a subject to an agent, to a job's lead and to
# a thread's agent; `nag` (per_run 1, again 1h, max 1) tells one subject a
# run, then asks the people once instead; `bad` fails while
# <dir>/bad-fail exists.
need_repo
CS=(herdr --session fleet-cfgw)
herdr --session fleet-cfgw server >/home/agent/server-fleet-cfgw.log 2>&1 &
for _ in $(seq 1 100); do "${CS[@]}" status server >/dev/null 2>&1 && break; sleep 0.2; done
CD=/home/agent/cfgw
CB=$CD/bin
CDB=/home/agent/.local/state/$T/cfgw.db
CK=C0801/8.8
mkdir -p "$CB" /home/agent/.config/$T /home/agent/events /home/agent/run
cat > "/home/agent/.config/$T/cfgw.json" <<JSON
{"fednet": {"socket": "$CD/fednet.sock"},
 "watches": {"quiet-threads": {"off": true},
   "ci": {"source": {"command": ["$CB/source", "ci"]}, "do": "tell", "prompt": "cfgw-ci.md", "env": ["CFGW_TOKEN"], "per_run": 3},
   "nag": {"source": {"command": ["$CB/source", "nag"]}, "do": "tell", "again": "1h", "max": 1},
   "bad": {"source": {"command": ["$CB/source", "bad"]}, "do": "tell", "env": ["CFGW_TOKEN"]}}}
JSON
printf 'Look at {{key}} first. 0xCFGWPROMPT\n' > "/home/agent/.config/$T/cfgw-ci.md"
cat > "$CB/source" <<'SOURCE'
#!/bin/sh
d=/home/agent/cfgw
cat > "$d/stdin-$1.json"
env | sed 's/=.*//' | sort > "$d/env-$1"
echo "$1" >> "$d/runs"
if [ "$1" = bad ] && [ -f "$d/bad-fail" ]; then echo "bad: cannot reach the CI with $CFGW_TOKEN" >&2; exit 3; fi
cat "$d/$1.jsonl" 2>/dev/null
exit 0
SOURCE
cat > "$CB/fednet" <<'FEDNET'
#!/bin/sh
d=/home/agent/cfgw
echo "$*" >> "$d/fednet.log"
case "$2" in
  read-thread) printf '{"messages":[{"ts":"1700000001.000","user":"U0ABC","text":"a"}]}\n' ;;
  post) echo "m-$(wc -l < "$d/fednet.log")" ;;
esac
FEDNET
printf '#!/bin/sh\n' > "$CB/atb"
chmod +x "$CB/source" "$CB/fednet" "$CB/atb"
cledger() { sqlite3 "$CDB" "$1" | tr '\n' ' '; }
cfield() { "${CS[@]}" agent get "$1" 2>&1 | jq -r ".result.agent.$2 // .error.code"; }
creceived() {
  local out
  for _ in $(seq 1 75); do
    out=$("${CS[@]}" agent read "$1" --source visible 2>/dev/null)
    case "$out" in *"esc to interrupt"*) sleep 0.2 ;; *) break ;; esac
  done
  cat "$fake/received-$(cfield "$1" pane_id)" 2>/dev/null
}
count() { printf '%s\n' "$1" | grep -cF -- "$2"; }
CA=thread-c0801-8-8

printf '{"msg_id":"cw1","payload":{"type":"message","thread":"%s","text":"start","user":"U0ABC","ts":"1700000001.000","channel_name":"repo-%s","scope":"cfgw"}}\n' \
  "$CK" "$R" > /home/agent/events/cw1.json
PATH=$CB:$PATH XDG_RUNTIME_DIR=/home/agent/run "$T" inbox /home/agent/events/cw1.json >"$CD/inbox.out" 2>&1; rc=$?
check "watches: inbox starts the thread's agent" "0 $CA" "$rc $(cfield "$CA" name)"
out=$(PATH=$CB:$PATH env "${P}AGENT=$CA" "${P}ROLE=thread" "${P}PARENT=" "${P}SCOPE=cfgw" "${P}JOB=" "${P}ISSUE=" \
  "${P}THREAD=$CK" "$T" job start cw --repo "$R" --task-file "$(task cw "job of the watches suite")" 2>&1); rc=$?
check "watches: job cw started" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"

cat > "$CD/ci.jsonl" <<JSONL
{"key": "a", "text": "PR 1 is red. 0xCFGWA", "agent": "$CA"}
{"key": "b", "text": "PR 2 waits. 0xCFGWB", "job": "cw"}
{"key": "c", "text": "the thread has news. 0xCFGWC", "thread": "$CK"}
JSONL
cat > "$CD/nag.jsonl" <<'JSONL'
{"key": "x", "text": "x is late. 0xCFGWX", "agent": "cw-lead"}
{"key": "y", "text": "y is late. 0xCFGWY", "agent": "cw-lead"}
JSONL
touch "$CD/bad-fail"
T0=$(date +%s)
cwatch() { # <minutes> <exit>: one run at T0 + minutes, its output in watch-<minutes>.out
  PATH=$CB:$PATH env "${P}WATCH_NOW=$((T0 + $1 * 60))" CFGW_TOKEN=cfgw-secret-token CFGW_OTHER=x \
    "$T" --scope cfgw watch >"$CD/watch-$1.out" 2>&1; rc=$?
  check "watches: run at +$1m exit $2" "$2" "$rc"
  [ "$rc" = "$2" ] || cat "$CD/watch-$1.out"
}

cwatch 0 5
has "watches: the failing source is skipped, its stderr's first line shown without the variable's value" \
  "$(cat "$CD/watch-0.out")" "fleet watch: watch bad skipped: its source exited 3: bad: cannot reach the CI with [redacted CFGW_TOKEN]"
lacks "watches: the value given to the source is not printed" "$(cat "$CD/watch-0.out")" "cfgw-secret-token"
has "watches: a subject naming an agent is told to it, with the prompt" "$(creceived "$CA")" \
  "[FROM: watch]" "fleet watch, watch \`ci\`, subject \`a\`:" "PR 1 is red. 0xCFGWA" "Look at a first. 0xCFGWPROMPT"
has "watches: a subject naming a job is told to its lead" "$(creceived cw-lead)" \
  "fleet watch, watch \`ci\`, subject \`b\`:" "PR 2 waits. 0xCFGWB" "Look at b first."
has "watches: a subject naming a thread is told to its agent" "$(creceived "$CA")" \
  "fleet watch, watch \`ci\`, subject \`c\`:" "the thread has news. 0xCFGWC"
check "watches: per_run 1 tells one subject a run" "1 0" \
  "$(count "$(creceived cw-lead)" "0xCFGWX") $(count "$(creceived cw-lead)" "0xCFGWY")"
check "watches: the source gets the variable env names and no other" "1 0" \
  "$(grep -cx CFGW_TOKEN "$CD/env-ci") $(grep -cx CFGW_OTHER "$CD/env-ci")"
check "watches: the source reads the scope, the clock and the jobs on stdin" "1 cfgw $T0 cw open null" \
  "$(jq -r '"\(.version) \(.scope) \(.now) \(.jobs[0].job) \(.jobs[0].state) \(.jobs[0].ended_at)"' "$CD/stdin-ci.json")"
check "watches: each subject told is counted" "a:1 b:1 c:1 x:1 y:0 " \
  "$(cledger "SELECT key || ':' || acts FROM watch_subjects ORDER BY key")"
check "watches: nothing is kept of the failed source" "0 " "$(cledger "SELECT count(*) FROM watch_subjects WHERE watch = 'bad'")"

rm "$CD/bad-fail"
cwatch 5 0
check "watches: the next subject is told the next run, the same texts are not told again" "1 1 1" \
  "$(count "$(creceived cw-lead)" "0xCFGWY") $(count "$(creceived cw-lead)" "0xCFGWX") $(count "$(creceived cw-lead)" "0xCFGWB")"
sed -i 's/PR 2 waits. 0xCFGWB/PR 2 waits, now with a conflict. 0xCFGWB/' "$CD/ci.jsonl"
cwatch 10 0
check "watches: a changed text is told again" 2 "$(count "$(creceived cw-lead)" "0xCFGWB")"
check "watches: again 1h holds back the unchanged ones" "1 1" \
  "$(count "$(creceived cw-lead)" "0xCFGWX") $(count "$(creceived cw-lead)" "0xCFGWY")"

cwatch 61 0
has "watches: at its max the people are asked once instead" "$(cat "$CD/fednet.log")" \
  "client post -socket $CD/fednet.sock -thread $CK -- fleet watch, watch \`nag\`: subject \`x\` has had the watch's max of 1 act(s)"
has "watches: and the thread's agent is told there is nothing to pass on" "$(creceived "$CA")" \
  "fleet watch asked the people in thread $CK" "Nothing to pass on"
cwatch 66 0
cwatch 130 0
check "watches: x and y are asked about once each, and not told again" "1 1 1 1" \
  "$(grep -c -- "-- fleet watch, watch \`nag\`: subject \`x\`" "$CD/fednet.log") $(grep -c -- "-- fleet watch, watch \`nag\`: subject \`y\`" "$CD/fednet.log") $(count "$(creceived cw-lead)" "0xCFGWX") $(count "$(creceived cw-lead)" "0xCFGWY")"
check "watches: the questions wait for the people" "watch|pending watch|pending " \
  "$(cledger "SELECT asked_by || '|' || state FROM questions ORDER BY id")"

"${CS[@]}" server stop >/dev/null

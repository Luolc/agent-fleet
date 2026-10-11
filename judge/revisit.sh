# Sourced by inside.sh: what `watch` does for the parent issues of a
# scope's jobs that stay unchanged, in a scope of its own (`parents`, herdr
# session fleet-parents, which this suite starts and stops), with fake
# Claudes, a fake fednet, and a fake atb whose `linear query` answers, from
# one file per issue, the issues the query names (as Linear filters by
# id), whatever their state; the clock moves through <prefix>WATCH_NOW.
# The jobs rows are inserted directly. Arms that must differ:
#   EX-1, EX-2  started, with a sub-issue, unchanged for 80h and 75h: a
#               revisit agent each, EX-1 first, one a run
#   EX-3        canceled; EX-4 without sub-issues; EX-5 changed 10h ago;
#               EX-6 with a job open on it: none
#   EX-8        unchanged for days, but no job of the scope's had it: not
#               asked for
saved=("$SCOPE" "$DB")
SCOPE=parents
S=(herdr --session fleet-$SCOPE)
DB=/home/agent/.local/state/$T/$SCOPE.db
PD=/home/agent/parents
herdr --session fleet-$SCOPE server >"/home/agent/server-fleet-$SCOPE.log" 2>&1 &
for _ in $(seq 1 100); do "${S[@]}" status server >/dev/null 2>&1 && break; sleep 0.2; done
thr job start setup --task-file "$(task setup 'makes the ledger')" >/dev/null 2>&1
thr job end setup --force >/dev/null 2>&1
check "revisit: the scope's ledger exists" yes "$([ -e "$DB" ] && echo yes || echo no)"
mkdir -p /home/agent/.config/$T "$PD/bin" "$PD/issues"
echo '{"fednet": {"socket": "/home/agent/parents/fednet.sock"}}' > "/home/agent/.config/$T/$SCOPE.json"
cat > "$PD/bin/atb" <<'ATB'
#!/bin/sh
d=/home/agent/parents
echo "$*" >> "$d/atb.log"
[ "$2" = query ] || exit 0
[ -e "$d/linear-down" ] && exit 1
nodes=""
for f in "$d"/issues/*.json; do
  id=$(basename "$f" .json)
  case "$*" in *"\"$id\""*) nodes="$nodes${nodes:+,}$(cat "$f")" ;; esac
done
printf '{"issues":{"nodes":[%s]}}\n' "$nodes"
ATB
# Only an open job's thread is read, and the open job here has none.
cat > "$PD/bin/fednet" <<'FEDNET'
#!/bin/sh
printf '%s\n' "$*" >> /home/agent/parents/fednet.log
echo m-posted
FEDNET
chmod +x "$PD/bin/atb" "$PD/bin/fednet"
now=$(date +%s)
iso() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ; }
# issue <id> <state type> <changed at> <sub-issues>
issue() {
  local kids=""
  [ "$4" -gt 0 ] && kids="{\"identifier\":\"EX-90\",\"title\":\"a try\",\"url\":\"https://linear.example.test/EX-90\",\"updatedAt\":\"$(iso "$3")\",\"state\":{\"name\":\"Done\",\"type\":\"completed\"}}"
  printf '{"identifier":"%s","title":"Parent %s","url":"https://linear.example.test/%s","updatedAt":"%s","state":{"name":"S","type":"%s"},"team":{"key":"EX"},"project":{"name":"Example project","description":"","content":"Close a parent once its sub-issues are done. 0xINSTRUCTIONS"},"children":{"nodes":[%s]}}' \
    "$1" "$1" "$1" "$(iso "$3")" "$2" "$kids" > "$PD/issues/$1.json"
}
issue EX-1 started $((now - 80 * 3600)) 1
issue EX-2 started $((now - 75 * 3600)) 1
issue EX-3 canceled $((now - 100 * 3600)) 1
issue EX-4 started $((now - 100 * 3600)) 0
issue EX-5 started $((now - 10 * 3600)) 1
issue EX-6 started $((now - 100 * 3600)) 1
issue EX-8 started $((now - 100 * 3600)) 1
KP=C0PAR/1700000000.000
sqlite3 "$DB" >/dev/null <<SQL
INSERT INTO jobs (job, parent_issue, lead_cwd, home_thread, state, outcome, started_at, ended_at) VALUES
    ('p1', 'EX-1', '/home/agent', '$KP', 'ended', 'done', $((now - 90 * 3600)), $((now - 80 * 3600))),
    ('p2', 'EX-2', '/home/agent', '$KP', 'ended', 'done', $((now - 90 * 3600)), $((now - 75 * 3600))),
    ('p3', 'EX-3', '/home/agent', '$KP', 'ended', 'abandoned', $((now - 200 * 3600)), $((now - 100 * 3600))),
    ('p4', 'EX-4', '/home/agent', '$KP', 'ended', 'done', $((now - 200 * 3600)), $((now - 100 * 3600))),
    ('p5', 'EX-5', '/home/agent', '$KP', 'ended', 'done', $((now - 20 * 3600)), $((now - 10 * 3600)));
INSERT INTO jobs (job, parent_issue, lead_cwd, home_thread, state, started_at) VALUES
    ('p6', 'EX-6', '/home/agent', '', 'open', $((now - 200 * 3600)));
SQL
pwatch() { # <seconds after now>
  PATH=$PD/bin:$PATH env "${P}WATCH_NOW=$((now + $1))" "$T" --scope "$SCOPE" watch
}
revisits() { "${S[@]}" agent list | jq -r '[.result.agents[].name | select(startswith("revisit-"))] | sort | join(" ")'; }

pwatch 0 >"$PD/run1.out" 2>&1; rc=$?
check "revisit: first run exit 0" 0 "$rc"
check "revisit: one agent, for the parent unchanged longest" revisit-ex-1 "$(revisits)"
has "revisit: the other stale parent waits for the next run" "$(cat "$PD/run1.out")" \
  "EX-2 has not changed; it waits, since at most 1 revisit agent(s) start in a run"
query=$(grep ' query ' "$PD/atb.log")
has "revisit: Linear is asked for the parents of the jobs that are not open" "$query" '"EX-1"' '"EX-3"' '"EX-5"'
lacks "revisit: not for a parent with a job open on it" "$query" '"EX-6"'
lacks "revisit: not for an issue no job had" "$query" '"EX-8"'
check "revisit: the agent's row" "revisit||EX-1 " \
  "$(ledger "SELECT role, job, parent_issue FROM agents WHERE name = 'revisit-ex-1' AND state != 'ended'")"
received revisit-ex-1 > "$PD/first.txt"
has "revisit: its first message: the issue, its sub-issue, the jobs, the instructions and where to ask" \
  "$(cat "$PD/first.txt")" "[FROM: watch]" "You are revisit-ex-1" "EX-1: Parent EX-1" "- EX-90: a try" \
  "- p1: ended, done" "0xINSTRUCTIONS" "revisit-ex-1-question.md" "fleet posts it to the thread $KP"

settled revisit-ex-1
pwatch 60 >"$PD/run2.out" 2>&1; rc=$?
check "revisit: second run exit 0" 0 "$rc"
check "revisit: the finished agent is closed, the next parent gets one" revisit-ex-2 "$(revisits)"
has "revisit: no second agent for a parent that has not changed since" "$(cat "$PD/run2.out")" \
  "EX-1 has not changed since a revisit agent looked at it"
check "revisit: no agent ever for the canceled, the childless, the young or the unrelated" "0 " \
  "$(ledger "SELECT count(*) FROM agents WHERE name IN ('revisit-ex-3', 'revisit-ex-4', 'revisit-ex-5', 'revisit-ex-6', 'revisit-ex-8')")"

# EX-1 changes now, EX-5 is closed: three days on, EX-1 is looked at again.
settled revisit-ex-2
issue EX-1 started $((now + 120)) 1
issue EX-5 completed $((now - 10 * 3600)) 1
pwatch 180 >"$PD/run3.out" 2>&1; rc=$?
check "revisit: third run exit 0, nothing started for a parent changed just now" "0 " "$rc $(revisits)"
pwatch $((73 * 3600)) >"$PD/run4.out" 2>&1; rc=$?
check "revisit: fourth run exit 0" 0 "$rc"
check "revisit: a parent that changed and then stayed unchanged gets a new agent" "revisit-ex-1 2" \
  "$(revisits) $(ledger "SELECT count(*) FROM agents WHERE name = 'revisit-ex-1'" | tr -d ' ')"

# Linear down: the rule is skipped, saying so; the finished agent is still closed.
settled revisit-ex-1
touch "$PD/linear-down"
pwatch $((73 * 3600 + 60)) >"$PD/run5.out" 2>&1; rc=$?
check "revisit: Linear down, exit 5" 5 "$rc"
has "revisit: says the parents are skipped" "$(cat "$PD/run5.out")" "the parent issues are skipped this run, Linear could not be read"
check "revisit: the finished agent is still closed" "" "$(revisits)"
for out in "$PD"/run*.out; do
  [ "$fail" = 0 ] || { echo "--- $out"; cat "$out"; }
done
"${S[@]}" server stop >/dev/null
SCOPE=${saved[0]} DB=${saved[1]}
S=(herdr --session fleet-$SCOPE)

# Sourced by inside.sh: `ci wait` against a fake gh that answers from
# files and logs each call with the pid of the process that made it. The
# fake stands for GitHub; nothing here reaches the network.
ci=/home/agent/fake-ci
mkdir -p "$ci/replies"
cat > "$ci/gh" <<'GH'
#!/bin/sh
# A run's path answers from replies/run-<id>.<n> on its n-th call, else
# replies/run-<id>; its jobs from replies/jobs-<id>.
r=/home/agent/fake-ci/replies
for path; do :; done
case "$path" in
  */jobs\?*) id=${path%/jobs*}; kind=jobs-${id##*/} ;;
  */actions/runs/*) kind=run-${path##*/} ;;
  *) echo "fake gh: unexpected path $path" >&2; exit 2 ;;
esac
echo "$PPID $(date +%s) $kind" >> /home/agent/fake-ci/calls
n=$(grep -c " $kind\$" /home/agent/fake-ci/calls)
f="$r/$kind.$n"
[ -f "$f" ] || f="$r/$kind"
cat "$f"
case "$(head -n 1 "$f")" in "HTTP/2.0 2"*) exit 0 ;; *) exit 1 ;; esac
GH
chmod +x "$ci/gh"
: > "$ci/calls"
# <file> <status> <headers> <body>: one reply as `gh api --include` prints it.
ci_reply() { printf 'HTTP/2.0 %s X\r\nContent-Type: application/json\r\n%b\r\n%s' "$2" "$3" "$4" > "$ci/replies/$1"; }
# <file> <id> <status> <conclusion JSON>
ci_run() {
  ci_reply "$1" 200 "" "{\"id\":$2,\"name\":\"ci\",\"head_sha\":\"$(printf 'a%.0s' $(seq 40))\",\"event\":\"push\",\"status\":\"$3\",\"conclusion\":$4,\"run_attempt\":1,\"html_url\":\"https://github.example.test/o/r/actions/runs/$2\",\"updated_at\":\"2026-10-23T10:00:00Z\"}"
}
# <file> <conclusion JSON>: one job, lint.
ci_jobs() { ci_reply "$1" 200 "" "{\"total_count\":1,\"jobs\":[{\"name\":\"lint\",\"status\":\"completed\",\"conclusion\":$2,\"completed_at\":null}]}"; }
cigh() { PATH="$ci:$PATH" "$T" ci wait --repo o/r "$@"; }
calls_of() { grep -c " $1\$" "$ci/calls"; }

# Two callers of one run: the first reply is GitHub's rate limit asking for
# 2 s, so the second caller starts while the first waits it out.
ci_reply run-41.1 429 'Retry-After: 2\r\n' '{"message":"API rate limit exceeded"}'
ci_run run-41 41 completed '"success"'
ci_jobs jobs-41 '"success"'
t0=$(date +%s)
cigh --run 41 >/home/agent/ci-a.out 2>/home/agent/ci-a.err & a=$!
sleep 0.5
cigh --run 41 >/home/agent/ci-b.out 2>&1 & b=$!
wait "$a"; rca=$?
wait "$b"; rcb=$?
check "ci wait: both callers of a run exit 0 on its success" "0 0" "$rca $rcb"
check "ci wait: both print the run and its jobs" \
  "run https://github.example.test/o/r/actions/runs/41 attempt 1: success|lint	success|same" \
  "$(tr '\n' '|' < /home/agent/ci-a.out)$(cmp -s /home/agent/ci-a.out /home/agent/ci-b.out && echo same)"
check "ci wait: one process asks GitHub for the two callers" 1 "$(grep ' run-41$\| jobs-41$' "$ci/calls" | cut -d' ' -f1 | sort -u | wc -l)"
check "ci wait: the run is asked twice, rate-limited then completed" 2 "$(calls_of run-41)"
has "ci wait: the rate limit is waited out as Retry-After says" "$(cat /home/agent/ci-a.err)" "waiting 2s"
check "ci wait: no answer before Retry-After's 2 s" yes "$( [ $(( $(date +%s) - t0 )) -ge 2 ] && echo yes || echo no)"

# A run that failed, and one still running at --timeout.
ci_run run-42 42 completed '"failure"'
ci_jobs jobs-42 '"failure"'
out=$(cigh --run 42 2>&1); rc=$?
check "ci wait: exit 7 on a failed run" 7 "$rc"
has "ci wait: a failed run prints its failed job" "$out" "attempt 1: failure" "lint	failure"
ci_run run-43 43 in_progress null
ci_reply jobs-43 200 "" '{"total_count":1,"jobs":[{"name":"lint","status":"in_progress","conclusion":null,"completed_at":null}]}'
out=$(cigh --run 43 --timeout 3s 2>&1); rc=$?
check "ci wait: exit 2 when --timeout passes first" 2 "$rc"
has "ci wait: the timeout says what it last saw" "$out" "timed out after 3s" "runs/43 attempt 1 in_progress"
ci_reply run-44 404 "" '{"message":"Not Found"}'
cigh --run 44 >/dev/null 2>&1; rc=$?
check "ci wait: exit 4 for a run GitHub does not have" 4 "$rc"
out=$(cigh --run 41 --json 2>&1); rc=$?
check "ci wait: a settled run is read from its record, GitHub not asked" "0 2" "$rc $(calls_of run-41)"
has "ci wait: --json prints the record" "$out" '"run_id":41' '"conclusion":"success"' '"run_attempt":1'
cigh --run 41 --sha "$(printf 'a%.0s' $(seq 40))" >/dev/null 2>&1; rc=$?
check "ci wait: --run with --sha is exit 1" 1 "$rc"

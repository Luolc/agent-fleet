// `ci wait` against a fake `gh`: one watcher per run, rate limits waited
// out, and the outcomes mapped to exit codes.
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeGh answers `gh api --include ... <path>` from files in <dir>/gh: a
// run's path from `run-<id>.<n>` on its n-th call, else `run-<id>`; its
// jobs from `jobs-<id>[.<n>]`; a workflow's runs from `list[.<n>]`. Each
// call appends `<caller pid> <time> <kind>` to <dir>/gh/calls, then sleeps
// the seconds in `sleep-<kind>` when that file exists.
const fakeGh = `#!/bin/sh
dir=$(dirname "$0")/../gh
for path; do :; done
case "$path" in
  */jobs\?*) id=${path%/jobs*}; kind=jobs-${id##*/} ;;
  */actions/runs/*) kind=run-${path##*/} ;;
  */workflows/*) kind=list ;;
  *) echo "fake gh: unexpected path $path" >&2; exit 2 ;;
esac
echo "$PPID $(date +%s.%N) $kind" >> "$dir/calls"
[ -f "$dir/sleep-$kind" ] && sleep "$(cat "$dir/sleep-$kind")"
n=$(grep -c " $kind\$" "$dir/calls")
f="$dir/$kind.$n"
[ -f "$f" ] || f="$dir/$kind"
cat "$f"
case "$(head -n 1 "$f")" in "HTTP/2.0 2"*) exit 0 ;; *) exit 1 ;; esac
`

// ghReplies writes the fake gh and its replies (file name to reply).
func (w *world) ghReplies(replies map[string]string) {
	w.t.Helper()
	dir := filepath.Join(w.dir, "gh")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.dir, "fake-herdr", "gh"), []byte(fakeGh), 0o755); err != nil {
		w.t.Fatal(err)
	}
	for name, reply := range replies {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(reply), 0o644); err != nil {
			w.t.Fatal(err)
		}
	}
}

// ghCall is one call the fake gh logged.
type ghCall struct {
	pid  string
	at   float64
	kind string
}

func (w *world) ghCalls() []ghCall {
	w.t.Helper()
	data, err := os.ReadFile(filepath.Join(w.dir, "gh", "calls"))
	if err != nil && !os.IsNotExist(err) {
		w.t.Fatal(err)
	}
	var calls []ghCall
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		at, _ := strconv.ParseFloat(f[1], 64)
		calls = append(calls, ghCall{f[0], at, f[2]})
	}
	return calls
}

func reply(status int, headers, body string) string {
	return fmt.Sprintf("HTTP/2.0 %d X\r\nContent-Type: application/json\r\n%s\r\n%s", status, headers, body)
}

func runReply(id, attempt int, status, conclusion string) string {
	c := "null"
	if conclusion != "" {
		c = strconv.Quote(conclusion)
	}
	return reply(200, "", fmt.Sprintf(`{"id":%d,"name":"ci","head_sha":"%s","event":"push","status":"%s","conclusion":%s,`+
		`"run_attempt":%d,"html_url":"https://github.example.test/o/r/actions/runs/%d","updated_at":"2026-10-23T10:00:00Z"}`,
		id, strings.Repeat("a", 40), status, c, attempt, id))
}

// jobsReply is the jobs list; each job is "name:status:conclusion".
func jobsReply(jobs ...string) string {
	var parts []string
	for _, j := range jobs {
		f := strings.Split(j, ":")
		c := "null"
		if f[2] != "" {
			c = strconv.Quote(f[2])
		}
		parts = append(parts, fmt.Sprintf(`{"name":%q,"status":%q,"conclusion":%s,"completed_at":null}`, f[0], f[1], c))
	}
	return reply(200, "", `{"total_count":1,"jobs":[`+strings.Join(parts, ",")+`]}`)
}

var rateLimited = reply(403, "", `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`)

// started is a `fleet` running in the background.
type started struct {
	cmd            *exec.Cmd
	stdout, stderr bytes.Buffer
}

func (w *world) start(args ...string) *started {
	w.t.Helper()
	s := &started{cmd: w.bin(args)}
	s.cmd.Stdout, s.cmd.Stderr = &s.stdout, &s.stderr
	if err := s.cmd.Start(); err != nil {
		w.t.Fatal(err)
	}
	return s
}

func (s *started) wait(t *testing.T) result {
	t.Helper()
	err := s.cmd.Wait()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = exitErr.ExitCode()
	}
	return result{code, s.stdout.String(), s.stderr.String()}
}

func pids(calls []ghCall) map[string]int {
	seen := map[string]int{}
	for _, c := range calls {
		seen[c.pid]++
	}
	return seen
}

func TestCIWaitTwoCallersOneWatcher(t *testing.T) {
	w := newWorld(t)
	w.ghReplies(map[string]string{
		"run-11.1": runReply(11, 1, "in_progress", ""),
		"run-11.2": runReply(11, 1, "in_progress", ""),
		"run-11.3": runReply(11, 1, "in_progress", ""),
		"run-11":   runReply(11, 1, "completed", "success"),
		"jobs-11":  jobsReply("lint:completed:success"),
	})
	args := []string{"ci", "wait", "--repo", "o/r", "--run", "11", "--interval", "300ms"}
	a, b := w.start(args...), w.start(args...)
	ra, rb := a.wait(t), b.wait(t)
	want := "run https://github.example.test/o/r/actions/runs/11 attempt 1: success\nlint\tsuccess\n"
	if ra.code != 0 || rb.code != 0 || ra.stdout != want || rb.stdout != want {
		t.Fatalf("both callers end with the run's success:\n%+v\n%+v", ra, rb)
	}
	calls := w.ghCalls()
	if got := pids(calls); len(got) != 1 {
		t.Errorf("one process asks GitHub, got the calls of %d: %v", len(got), got)
	}
	runs := 0
	for _, c := range calls {
		if c.kind == "run-11" {
			runs++
		}
	}
	if runs != 4 {
		t.Errorf("the watcher looks at the run 4 times (3 in progress, then completed), got %d: %+v", runs, calls)
	}
}

func TestCIWaitNextCallerTakesOverFromAKilledWatcher(t *testing.T) {
	w := newWorld(t)
	replies := map[string]string{
		"run-18":  runReply(18, 1, "completed", "success"),
		"jobs-18": jobsReply("lint:completed:success"),
	}
	for n := 1; n <= 3; n++ {
		replies[fmt.Sprintf("run-18.%d", n)] = runReply(18, 1, "in_progress", "")
	}
	w.ghReplies(replies)
	args := []string{"ci", "wait", "--repo", "o/r", "--run", "18", "--interval", "300ms", "--timeout", "20s"}
	a := w.start(args...)
	for deadline := time.Now().Add(10 * time.Second); len(w.ghCalls()) == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			_ = a.cmd.Process.Kill()
			t.Fatal("the first caller asked GitHub nothing within 10 s")
		}
	}
	b := w.start(args...)
	time.Sleep(100 * time.Millisecond)
	if err := a.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = a.cmd.Wait()
	rb := b.wait(t)
	if rb.code != 0 || !strings.Contains(rb.stdout, "attempt 1: success") {
		t.Fatalf("the second caller ends with the run's success: %+v", rb)
	}
	if got := pids(w.ghCalls()); len(got) != 2 || got[strconv.Itoa(b.cmd.Process.Pid)] == 0 {
		t.Errorf("the second caller asks GitHub once the first is killed, got the calls of %v", got)
	}
}

func TestCIWaitWaitsOutRateLimits(t *testing.T) {
	w := newWorld(t)
	w.ghReplies(map[string]string{
		"run-12.1": reply(403, "Retry-After: 2\r\n", `{"message":"You have exceeded a secondary rate limit."}`),
		"run-12":   runReply(12, 1, "completed", "success"),
		"jobs-12":  jobsReply("lint:completed:success"),
		"run-14":   runReply(14, 1, "completed", "success"),
		"jobs-14":  jobsReply("lint:completed:success"),
		"run-13.1": rateLimited,
		"run-13.2": rateLimited,
		"run-13":   runReply(13, 1, "completed", "success"),
		"jobs-13":  jobsReply("lint:completed:success"),
	})
	a := w.start("ci", "wait", "--repo", "o/r", "--run", "12")
	time.Sleep(500 * time.Millisecond)
	// Another run's watcher holds off while the machine is rate-limited.
	b := w.start("ci", "wait", "--repo", "o/r", "--run", "14")
	ra, rb := a.wait(t), b.wait(t)
	if ra.code != 0 || rb.code != 0 || !strings.Contains(ra.stderr, "rate limit (HTTP 403), waiting 2s") {
		t.Fatalf("both end with success after the wait:\n%+v\n%+v", ra, rb)
	}
	var first, again12, first14 float64
	for _, c := range w.ghCalls() {
		switch {
		case c.kind == "run-12" && first == 0:
			first = c.at
		case c.kind == "run-12":
			again12 = c.at
		case c.kind == "run-14" && first14 == 0:
			first14 = c.at
		}
	}
	if again12-first < 2 || first14-first < 2 {
		t.Errorf("no request within Retry-After's 2 s: run 12 again after %.2fs, run 14 first after %.2fs", again12-first, first14-first)
	}

	// Without Retry-After: the floor (1 s in the test binary), doubled at the next hit.
	rc := w.run("", []string{"ci", "wait", "--repo", "o/r", "--run", "13"})
	if rc.code != 0 || !strings.Contains(rc.stderr, "waiting 1s") || !strings.Contains(rc.stderr, "waiting 2s") {
		t.Fatalf("run 13 ends with success after waiting 1s then 2s: %+v", rc)
	}
	var at []float64
	for _, c := range w.ghCalls() {
		if c.kind == "run-13" {
			at = append(at, c.at)
		}
	}
	if len(at) != 3 || at[1]-at[0] < 1 || at[2]-at[1] < 2 {
		t.Errorf("run 13 asked 3 times, 1 s then 2 s apart, got %v", at)
	}
}

func TestCIWaitOutcomes(t *testing.T) {
	w := newWorld(t)
	w.ghReplies(map[string]string{
		"run-15":       runReply(15, 1, "completed", "failure"),
		"jobs-15":      jobsReply("lint:completed:failure", "race:completed:success"),
		"run-16":       runReply(16, 1, "in_progress", ""),
		"jobs-16":      jobsReply("lint:completed:failure", "race:in_progress:"),
		"run-17":       runReply(17, 1, "in_progress", ""),
		"jobs-17":      jobsReply("lint:in_progress:"),
		"run-19":       reply(404, "", `{"message":"Not Found"}`),
		"run-23":       runReply(23, 1, "completed", "success"),
		"sleep-run-23": "30",
		"run-20":       reply(403, "", `{"message":"Resource not accessible by integration"}`),
		"run-22.1":     runReply(22, 1, "completed", "failure"),
		"run-22":       runReply(22, 2, "completed", "success"),
		"jobs-22":      jobsReply("lint:completed:success"),
	})
	ciWait := func(args ...string) result {
		return w.run("", append([]string{"ci", "wait", "--repo", "o/r", "--interval", "100ms"}, args...))
	}
	if out := ciWait("--run", "15"); out.code != 7 || out.stdout != "run https://github.example.test/o/r/actions/runs/15 attempt 1: failure\nlint\tfailure\nrace\tsuccess\n" {
		t.Errorf("a failed run is exit 7 with its jobs: %+v", out)
	}
	if out := ciWait("--run", "16", "--timeout", "10s"); out.code != 7 || !strings.Contains(out.stdout, "attempt 1: failing\nlint\tfailure\nrace\tin_progress\n") {
		t.Errorf("a job that failed while the run runs is exit 7 at once: %+v", out)
	}
	if out := ciWait("--run", "17", "--timeout", "1s"); out.code != 2 || !strings.Contains(out.stderr, "timed out after 1s") {
		t.Errorf("a run still in progress at --timeout is exit 2: %+v", out)
	}
	start := time.Now()
	if out := ciWait("--run", "23", "--timeout", "1500ms"); out.code != 2 || time.Since(start) > 4*time.Second {
		t.Errorf("a gh call that hangs is cut at --timeout, exit 2, got after %v: %+v", time.Since(start), out)
	}
	if out := ciWait("--run", "19"); out.code != 4 {
		t.Errorf("no such run is exit 4: %+v", out)
	}
	if out := ciWait("--run", "20"); out.code != 5 || !strings.Contains(out.stderr, "HTTP 403: Resource not accessible") {
		t.Errorf("a 403 that is not the rate limit is exit 5: %+v", out)
	}
	if out := ciWait("--run", "22"); out.code != 7 {
		t.Errorf("attempt 1 of run 22 failed: %+v", out)
	}
	if out := ciWait("--run", "22"); out.code != 7 {
		t.Errorf("the record of attempt 1 answers again: %+v", out)
	}
	if out := ciWait("--run", "22", "--attempt", "2", "--json"); out.code != 0 || !strings.Contains(out.stdout, `"run_attempt":2`) {
		t.Errorf("--attempt 2 waits past the record of attempt 1: %+v", out)
	}
	if n := len(w.ghCalls()); n == 0 {
		t.Fatal("no call logged")
	}
	runs22 := 0
	for _, c := range w.ghCalls() {
		if c.kind == "run-22" {
			runs22++
		}
	}
	if runs22 != 2 {
		t.Errorf("run 22 asked twice (attempt 1, then for --attempt 2), got %d", runs22)
	}
	record, err := os.ReadFile(filepath.Join(w.dir, "home", ".local", "state", "fleet", "ci", "o", "r", "run-15.json"))
	if err != nil || !strings.Contains(string(record), `"conclusion":"failure"`) {
		t.Errorf("the record of run 15: %s %v", record, err)
	}
}

func TestCIWaitBySHA(t *testing.T) {
	w := newWorld(t)
	sha := strings.Repeat("b", 40)
	w.ghReplies(map[string]string{
		"list.1":  reply(200, "", `{"total_count":0,"workflow_runs":[]}`),
		"list":    reply(200, "", `{"total_count":2,"workflow_runs":[{"id":30},{"id":31}]}`),
		"run-31":  runReply(31, 1, "completed", "success"),
		"jobs-31": jobsReply("lint:completed:success"),
	})
	args := []string{"ci", "wait", "--repo", "o/r", "--sha", sha, "--workflow", "ci.yml", "--interval", "200ms"}
	a, b := w.start(args...), w.start(args...)
	ra, rb := a.wait(t), b.wait(t)
	if ra.code != 0 || rb.code != 0 || !strings.Contains(ra.stdout, "runs/31 attempt 1: success") || ra.stdout != rb.stdout {
		t.Fatalf("both callers wait for the newest run of the SHA:\n%+v\n%+v", ra, rb)
	}
	lists := 0
	for _, c := range w.ghCalls() {
		if c.kind == "list" {
			lists++
		}
	}
	if got := pids(w.ghCalls()); len(got) != 1 || lists != 2 {
		t.Errorf("one process looks the run up, twice (none yet, then two runs): %d lists, the calls of %v", lists, got)
	}
	countLists := func() int {
		n := 0
		for _, c := range w.ghCalls() {
			if c.kind == "list" {
				n++
			}
		}
		return n
	}
	// A newer run of the SHA (a reopened PR, a dispatch) after the lookup
	// went stale (CILookupFresh is 500 ms in the test binary): looked up again.
	time.Sleep(600 * time.Millisecond)
	w.ghReplies(map[string]string{
		"list":    reply(200, "", `{"total_count":3,"workflow_runs":[{"id":33},{"id":31},{"id":30}]}`),
		"run-33":  runReply(33, 1, "completed", "failure"),
		"jobs-33": jobsReply("lint:completed:failure"),
	})
	if out := w.run("", args); out.code != 7 || !strings.Contains(out.stdout, "runs/33 attempt 1: failure") || countLists() != 3 {
		t.Errorf("a stale lookup is made again and finds the newer run 33 (%d lists): %+v", countLists(), out)
	}
	// With --event, even one named any, a lookup of its own, not the fresh one without.
	if out := w.run("", append(args, "--event", "any")); out.code != 7 || countLists() != 4 {
		t.Errorf("--event any looks up apart from no --event (%d lists): %+v", countLists(), out)
	}
}

func TestCIWaitRefusesWhatNamesNoRun(t *testing.T) {
	w := newWorld(t)
	w.ghReplies(nil)
	for _, args := range [][]string{
		{"--repo", "o/r"},
		{"--repo", "o/r", "--run", "1", "--sha", strings.Repeat("a", 40)},
		{"--repo", "o/r", "--sha", "abc", "--workflow", "ci.yml"},
		{"--repo", "o/r", "--sha", strings.Repeat("a", 40)},
		{"--repo", "o/../r", "--run", "1"},
		{"--repo", "o/..", "--run", "1"},
		{"--repo", "o/r", "--sha", strings.Repeat("a", 40), "--workflow", "../ci.yml"},
		{"--repo", "o/r", "--run", "1", "--interval", "50ms"},
		{"--repo", "o/r", "--run", "x"},
		{"--run", "1"},
	} {
		if out := w.run("", append([]string{"ci", "wait"}, args...)); out.code != 1 {
			t.Errorf("%v: want exit 1, got %+v", args, out)
		}
	}
	if out := w.run("", []string{"--scope", "x", "ci", "wait", "--repo", "o/r", "--run", "1"}); out.code != 1 {
		t.Errorf("--scope: %+v", out)
	}
	if calls := w.ghCalls(); len(calls) != 0 {
		t.Errorf("a refusal asks GitHub nothing: %+v", calls)
	}
}

// Integration tests for `worker end` over the binary, against real git
// worktrees, a fake herdr and a fake atb.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// workerEndWorld is job item-1 with its lead and the workers item-1-a
// (work order EX-13) and item-1-b (EX-14), each in a worktree of its own
// recorded for the job, in panes p5 and p6 whose tabs are t5 and t6, and a
// fake herdr listing item-1-a until its tab is closed; item-1-b never
// started, so herdr lists no agent in its pane. `tab close` and `agent
// prompt <target>` are logged to <dir>/calls with atb's calls, the prompt's
// text to <dir>/prompt-<target>; a prompt to a target with <dir>/gone-<target>
// is agent_not_found; `agent get <name>` prints <dir>/get-<name>, and `pane
// process-info --pane <pane>` <dir>/procinfo-<pane>, none found without. Returns the worktrees' directory and the main checkout.
func workerEndWorld(t *testing.T) (w *world, wt, checkout string) {
	t.Helper()
	w = newWorld(t)
	checkout, _ = w.origin()
	wt = filepath.Join(w.dir, "home", "wt", dataset)
	for _, name := range []string{"a", "b"} {
		if out := w.worktree("item-1", dataset, "--name", name); out.code != 0 {
			t.Fatalf("%+v", out)
		}
	}
	conn := w.ledger()
	for _, stmt := range []string{
		"INSERT INTO jobs (job, repo, lead_cwd, state, started_at) VALUES ('item-1', 'example-dataset', '" + checkout + "', 'open', 0)",
		"INSERT INTO agents (name, role, job, parent, state, started_at, cwd) VALUES ('item-1-lead', 'lead', 'item-1', 'thread-1', 'active', 0, '" + checkout + "')",
		"INSERT INTO agents (name, role, job, parent, state, started_at, cwd, issue, pane_id) VALUES ('item-1-a', 'worker', 'item-1', 'item-1-lead', 'active', 0, '" + filepath.Join(wt, "item-1-a") + "', 'EX-13', 'p5')",
		"INSERT INTO agents (name, role, job, parent, state, started_at, cwd, issue, pane_id) VALUES ('item-1-b', 'worker', 'item-1', 'item-1-lead', 'starting', 0, '" + filepath.Join(wt, "item-1-b") + "', 'EX-14', 'p6')",
		"INSERT INTO agents (name, role, job, parent, state, started_at, cwd) VALUES ('watch-w-1', 'worker', '', 'watch', 'active', 0, '/w/watch')",
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	conn.Close()
	script := `#!/bin/sh
if [ "$1" = --session ]; then shift 2; fi
dir="$(dirname "$0")/.."
case "$1 $2" in
  "workspace list") echo '{"result":{"workspaces":[]}}' ;;
  "agent list")
    a=',{"name":"item-1-a","pane_id":"p5","workspace_id":"w1","cwd":"` + filepath.Join(wt, "item-1-a") + `"}'
    [ -e "$dir/closed-t5" ] && a=
    echo '{"result":{"agents":[{"name":"item-1-lead","pane_id":"p0","workspace_id":"w1","cwd":"` + checkout + `"}'"$a"']}}' ;;
  "pane get")
    tab="t${3#p}"
    if [ -e "$dir/closed-$tab" ]; then echo '{"error":{"code":"pane_not_found","message":"no pane"}}'; else
      echo '{"result":{"pane":{"pane_id":"'"$3"'","tab_id":"'"$tab"'"}}}'; fi ;;
  "tab get")
    if [ -e "$dir/closed-$3" ]; then echo '{"error":{"code":"tab_not_found","message":"no tab"}}'; else
      echo '{"result":{"tab":{"tab_id":"'"$3"'"}}}'; fi ;;
  "tab close") echo "herdr $1 $2 $3" >> "$dir/calls"; touch "$dir/closed-$3"; echo '{"result":{}}' ;;
  "agent prompt")
    if [ -e "$dir/gone-$3" ]; then echo '{"error":{"code":"agent_not_found","message":"no agent"}}'; else
      echo "herdr $1 $2 $3" >> "$dir/calls"; printf '%s' "$4" > "$dir/prompt-$3"
      echo '{"result":{"type":"agent_prompted"}}'; fi ;;
  "agent get") cat "$dir/get-$3" 2>/dev/null || echo '{"error":{"code":"agent_not_found","message":"no agent"}}' ;;
  "pane process-info")
    cat "$dir/procinfo-$4" 2>/dev/null || echo '{"result":{"process_info":{"foreground_processes":[]}}}' ;;
  *) echo "fake herdr: unexpected command: $*" >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(w.dir, "fake-herdr", "herdr"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	w.fakeAtb("")
	return w, wt, checkout
}

// endWorker runs `worker end` as item-1-lead with the Linear key.
func (w *world) endWorker(args ...string) result {
	w.t.Helper()
	return w.run("", append([]string{"worker", "end"}, args...),
		"FLEET_AGENT=item-1-lead", "FLEET_ROLE=lead", "FLEET_PARENT=thread-1", "FLEET_SCOPE="+scope,
		"FLEET_JOB=item-1", linearKey)
}

func TestWorkerEndReleasesClosesRemovesItsWorktreeAndUnblocksJobEnd(t *testing.T) {
	w, wt, checkout := workerEndWorld(t)
	report := task(w, "report.md", "stuck at the gateway error\n")
	// A commit on no remote yet: the branch that holds it is kept.
	work := filepath.Join(wt, "item-1-a", "work.txt")
	if err := os.WriteFile(work, []byte("half done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.git("-C", filepath.Join(wt, "item-1-a"), "add", "work.txt")
	w.git("-C", filepath.Join(wt, "item-1-a"), "commit", "-q", "-m", "half done")
	commit := w.git("-C", filepath.Join(wt, "item-1-a"), "rev-parse", "HEAD")
	out := w.endWorker("item-1-a", "--report-file", report)
	if out.code != 0 || !strings.Contains(out.stdout, "ended worker item-1-a of job item-1") ||
		!strings.Contains(out.stdout, "kept branch item-1-a") {
		t.Fatalf("%+v", out)
	}
	calls := strings.Split(strings.TrimSpace(w.calls()), "\n")
	want := []string{
		"atb linear comment EX-13 --body-file " + report + " key=set",
		"atb linear release EX-13 --agent item-1-a --reason abandoned --abandon key=set",
		"herdr tab close t5",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls = %q, want %q", calls, want)
	}
	if dirExists(filepath.Join(wt, "item-1-a")) {
		t.Error("the worker's worktree is still there")
	}
	if got := w.git("-C", checkout, "rev-parse", "item-1-a"); got != commit {
		t.Errorf("branch item-1-a is at %q, want the worker's commit %s", got, commit)
	}
	if !dirExists(filepath.Join(wt, "item-1-b")) {
		t.Error("another worker's worktree was removed")
	}
	wantRows := []string{
		filepath.Join(wt, "item-1-a") + "|" + dataset + "|item-1-a|item-1|item-1-a|removed",
		filepath.Join(wt, "item-1-b") + "|" + dataset + "|item-1-b|item-1|item-1-a|live",
	}
	if got := w.worktreeRows(); strings.Join(got, "\n") != strings.Join(wantRows, "\n") {
		t.Errorf("rows = %q, want %q", got, wantRows)
	}
	if got := state(w, "item-1-a"); got != "ended" {
		t.Errorf("state = %s", got)
	}
	// job end names only the worker still live.
	jobEnd := w.asAgent("item-1-lead", "lead", "thread-1", "item-1", "job", "end", "--report-file", report)
	if jobEnd.code != 1 || !strings.Contains(jobEnd.stderr, "live workers: item-1-b;") {
		t.Errorf("job end: %+v", jobEnd)
	}

	// From a shell, a worker herdr lists no agent for (it never started):
	// a fixed line on its work order, and its tab closed all the same.
	out = w.run("", []string{"worker", "end", "item-1-b"}, "FLEET_SCOPE="+scope, linearKey)
	if out.code != 0 || state(w, "item-1-b") != "ended" || dirExists(filepath.Join(wt, "item-1-b")) {
		t.Fatalf("%+v", out)
	}
	if got := w.bodies(); !strings.Contains(got, "item-1-b was ended by a shell with `fleet worker end`, without a report.") {
		t.Errorf("bodies = %q", got)
	}
	if !strings.Contains(w.calls(), "herdr tab close t6") {
		t.Errorf("calls = %q", w.calls())
	}
	// The lead did not end it, so the lead is told.
	want = []string{"[FROM: a shell]\na shell ended your worker item-1-b with `fleet worker end`; do not wait for its `fleet done`.",
		"Its work order EX-14 is released as abandoned.", "Its branch item-1-b is kept."}
	if got := w.prompt("item-1-lead"); got != strings.Join(want, "\n")+"\n" || !strings.Contains(out.stdout, "told item-1-lead") {
		t.Errorf("prompt = %q, stdout %q", got, out.stdout)
	}
	// No live worker refuses job end any more for its workers.
	jobEnd = w.asAgent("item-1-lead", "lead", "thread-1", "item-1", "job", "end", "--report-file", report)
	if strings.Contains(jobEnd.stderr, "live workers") {
		t.Errorf("job end: %+v", jobEnd)
	}
}

func TestWorkerEndRefusesTheWrongCallerAndTheWrongRow(t *testing.T) {
	w, wt, _ := workerEndWorld(t)
	conn := w.ledger()
	if _, err := conn.Exec("INSERT INTO jobs (job, repo, lead_cwd, state, started_at) VALUES ('item-2', 'example-dataset', '/c', 'open', 0)"); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	asWorker := w.asAgent("item-1-b", "worker", "item-1-lead", "item-1", "worker", "end", "item-1-a")
	otherLead := w.asAgent("item-2-lead", "lead", "thread-1", "item-2", "worker", "end", "item-1-a")
	for name, c := range map[string]struct {
		out  result
		code int
		want string
	}{
		"worker":            {asWorker, 1, "a worker cannot end an agent"},
		"lead of other job": {otherLead, 1, "item-1-a is in job item-1, not yours (item-2)"},
		"a lead row":        {w.asThread("worker", "end", "item-1-lead"), 1, "item-1-lead is a lead"},
		"watch worker":      {w.asThread("worker", "end", "watch-w-1"), 1, "has no job"},
		"no such worker":    {w.endWorker("item-1-z"), 4, "no live agent item-1-z"},
		"bad report":        {w.endWorker("item-1-a", "--report-file", filepath.Join(w.dir, "missing.md")), 1, "cannot read"},
	} {
		if c.out.code != c.code || !strings.Contains(c.out.stderr, c.want) {
			t.Errorf("%s: %+v", name, c.out)
		}
	}
	for _, name := range []string{"item-1-a", "watch-w-1", "item-1-lead"} {
		if got := state(w, name); got != "active" {
			t.Errorf("state(%s) = %s", name, got)
		}
	}
	if got := w.calls(); got != "" || !dirExists(filepath.Join(wt, "item-1-a")) {
		t.Errorf("something changed: calls %q", got)
	}
}

func TestWorkerEndStopsAtUncommittedChangesAndResumesAfterAFailedStep(t *testing.T) {
	w, wt, _ := workerEndWorld(t)
	dirty := filepath.Join(wt, "item-1-a", "notes.txt")
	if err := os.WriteFile(dirty, []byte("draft\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := w.endWorker("item-1-a")
	if out.code != 5 || !strings.Contains(out.stderr, "not ending item-1-a: in the way: uncommitted changes in "+filepath.Join(wt, "item-1-a")) {
		t.Errorf("%+v", out)
	}
	if _, err := os.Stat(dirty); err != nil || w.calls() != "" || state(w, "item-1-a") != "active" {
		t.Errorf("something changed: calls %q, state %s", w.calls(), state(w, "item-1-a"))
	}
	if err := os.Remove(dirty); err != nil {
		t.Fatal(err)
	}

	// The release fails: the pane stays, the row stays live; again, the
	// comment already recorded is not written twice.
	w.fakeAtb("release")
	if out := w.endWorker("item-1-a"); out.code != 5 || state(w, "item-1-a") != "active" || strings.Contains(w.calls(), "tab close") {
		t.Errorf("failed release: %+v, calls %q", out, w.calls())
	}
	w.fakeAtb("")
	if out := w.endWorker("item-1-a"); out.code != 0 || state(w, "item-1-a") != "ended" {
		t.Errorf("retry: %+v", out)
	}
	if got := strings.Count(w.calls(), "atb linear comment"); got != 1 {
		t.Errorf("comments = %d, calls %q", got, w.calls())
	}
}

// prompt is the text the fake herdr last prompted `target` with.
func (w *world) prompt(target string) string {
	w.t.Helper()
	data, err := os.ReadFile(filepath.Join(w.dir, "prompt-"+target))
	if err != nil {
		w.t.Fatal(err)
	}
	return string(data)
}

func TestWorkerEndByAThreadAgentTellsTheLeadAndAGoneLeadIsOnlyAWarning(t *testing.T) {
	w, _, _ := workerEndWorld(t)
	report := task(w, "report.md", "stuck\n")
	out := w.run("", []string{"worker", "end", "item-1-a", "--report-file", report},
		"FLEET_AGENT=thread-1", "FLEET_ROLE=thread", "FLEET_SCOPE="+scope, linearKey)
	if out.code != 0 || !strings.Contains(out.stdout, "told item-1-lead") {
		t.Fatalf("%+v", out)
	}
	want := "[FROM: thread-1]\nthread-1 ended your worker item-1-a with `fleet worker end`; do not wait for its `fleet done`.\n" +
		"Its work order EX-13 is released as abandoned.\nIts branch item-1-a is kept.\nReport: " + report + "\n"
	if got := w.prompt("item-1-lead"); got != want {
		t.Errorf("prompt = %q, want %q", got, want)
	}

	// The lead is gone: the worker is ended all the same, with a warning.
	if err := os.WriteFile(filepath.Join(w.dir, "gone-item-1-lead"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out = w.run("", []string{"worker", "end", "item-1-b"}, "FLEET_AGENT=thread-1", "FLEET_ROLE=thread",
		"FLEET_SCOPE="+scope, linearKey)
	if out.code != 0 || state(w, "item-1-b") != "ended" ||
		out.stderr != "warning: item-1-b is ended, but its parent item-1-lead was not told: it is not found\n" {
		t.Errorf("%+v", out)
	}
}

// A lead paused on Claude Code's `Session paused` dialog is typed nothing:
// the worker is ended all the same, with a warning.
func TestWorkerEndTypesNothingIntoALeadPausedOnItsDialog(t *testing.T) {
	w, _, _ := workerEndWorld(t)
	w.write(filepath.Join(w.dir, "get-item-1-lead"),
		`{"result":{"agent":{"name":"item-1-lead","agent":"claude","agent_status":"idle","pane_id":"p0"}}}`)
	w.claudeSession("p0", 4100, sessionThread, pausedRecords()...)
	out := w.run("", []string{"worker", "end", "item-1-b"}, "FLEET_AGENT=thread-1", "FLEET_ROLE=thread",
		"FLEET_SCOPE="+scope, linearKey)
	if out.code != 0 || state(w, "item-1-b") != "ended" || strings.Contains(w.calls(), "agent prompt") ||
		out.stderr != "warning: item-1-b is ended, but its parent item-1-lead was not told: it is paused on "+
			"Claude Code's `Session paused` dialog, so nothing was typed (press no key there)\n" {
		t.Errorf("%+v, calls %q", out, w.calls())
	}
}

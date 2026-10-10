// Integration tests for `job start`, `spawn`, `done` and `job end` over
// the binary: the refusals that happen before anything is created, and
// `done` and `job end` against the fake herdr. Hermetic, see main_test.go.
// The full job start → spawn → done → job end chain runs against the real
// herdr in the judge.
package main

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Luolc/agent-fleet/internal/db"
)

// asAgent runs the binary with the given caller identity in the test's
// target.
func (w *world) asAgent(agent, role, parent, job string, args ...string) result {
	w.t.Helper()
	return w.run("", args,
		"FLEET_AGENT="+agent, "FLEET_ROLE="+role, "FLEET_PARENT="+parent, "FLEET_SCOPE="+scope, "FLEET_JOB="+job)
}

// asThread runs the binary as the thread agent `thread-1`.
func (w *world) asThread(args ...string) result {
	w.t.Helper()
	return w.asAgent("thread-1", "thread", "", "", args...)
}

// ledger opens the world's ledger.
func (w *world) ledger() *sql.DB {
	w.t.Helper()
	path, err := db.PathUnder(filepath.Join(w.dir, "home", ".local", "state"), scope)
	if err != nil {
		w.t.Fatal(err)
	}
	conn, err := db.OpenAt(path)
	if err != nil {
		w.t.Fatal(err)
	}
	return conn
}

// ledgerWith is the world's ledger with the given live rows, each with a
// cwd, and an open job for every job named.
func ledgerWith(w *world, rows []struct{ name, role, job string }) {
	w.t.Helper()
	conn := w.ledger()
	defer conn.Close()
	jobs := map[string]bool{}
	for _, r := range rows {
		if r.job != "" && !jobs[r.job] {
			jobs[r.job] = true
			if _, err := conn.Exec(
				"INSERT INTO jobs (job, repo, lead_cwd, state, started_at) VALUES (?1, 'example-dataset', ?2, 'open', 0)",
				r.job, filepath.Join(w.dir, "home", "dev", "example-dataset")); err != nil {
				w.t.Fatal(err)
			}
		}
		if _, err := conn.Exec(
			"INSERT INTO agents (name, role, job, parent, state, started_at, cwd) VALUES (?1, ?2, ?3, '', 'active', 0, '/w/item-1')",
			r.name, r.role, r.job); err != nil {
			w.t.Fatal(err)
		}
	}
}

// state is the state of the newest row named `name`.
func state(w *world, name string) string {
	w.t.Helper()
	conn := w.ledger()
	defer conn.Close()
	var got string
	if err := conn.QueryRow(
		"SELECT state FROM agents WHERE name = ?1 ORDER BY id DESC LIMIT 1", name).Scan(&got); err != nil {
		w.t.Fatal(err)
	}
	return got
}

// jobState is the state, and the outcome once ended, of the newest job row named `job`, or "none".
func jobState(w *world, job string) string {
	w.t.Helper()
	conn := w.ledger()
	defer conn.Close()
	var state, outcome string
	if err := conn.QueryRow(
		"SELECT state, outcome FROM jobs WHERE job = ?1 ORDER BY id DESC LIMIT 1", job).Scan(&state, &outcome); err != nil {
		return "none"
	}
	return strings.TrimSpace(state + " " + outcome)
}

func task(w *world, name, body string) string {
	w.t.Helper()
	path := filepath.Join(w.dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		w.t.Fatal(err)
	}
	return path
}

// dir makes a directory under the world and returns it.
func dir(w *world, name string) string {
	w.t.Helper()
	path := filepath.Join(w.dir, name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		w.t.Fatal(err)
	}
	return path
}

// longJob is 28 characters: `<job>-lead` is 33, one over herdr's limit.
const longJob = "abcdefghij-abcdefghij-abcdef"

// longWorker is 33 characters with `item-1-` in front.
const longWorker = "abcdefghij-abcdefghij-abcd"

func TestSpawnRefusalsHappenBeforeHerdrIsCalled(t *testing.T) {
	w := newWorld(t)
	w.configure(`{"max_agents_per_job": 4}`)
	taskFile := task(w, "task.md", "build item 1\n")
	cwd := dir(w, "wt")
	ledgerWith(w, []struct{ name, role, job string }{
		{"item-1-lead", "lead", "item-1"},
		{"item-1-a", "worker", "item-1"},
		{"item-1-b", "worker", "item-1"},
		{"item-1-c", "worker", "item-1"},
		{"item-2-lead", "lead", "item-2"},
	})
	// The fake herdr's `agent list` answers with no JSON here, so a spawn
	// that reached herdr would exit 5, not 1.
	cases := []struct {
		agent, role, job string
		args             []string
		says             string
	}{
		{"item-1-a", "worker", "item-1", []string{"x", "--cwd", cwd}, "cannot spawn"},
		{"thread-1", "thread", "", []string{"x", "--cwd", cwd}, "cannot spawn"},
		{"item-2-lead", "lead", "item-2", []string{"Item_2", "--cwd", cwd}, "[a-z0-9-]"},
		{"item-2-lead", "lead", "item-2", []string{"watch", "--cwd", cwd}, "reserved"},
		{"item-2-lead", "lead", "", []string{"a", "--cwd", cwd}, "FLEET_JOB"},
		{"item-1-lead", "lead", "item-1", []string{"a", "--cwd", cwd}, "already live"},
		{"item-1-lead", "lead", "item-1", []string{"d", "--cwd", cwd}, "cap is 4"},
		{"item-1-lead", "lead", "item-1", []string{longWorker, "--cwd", cwd}, "at most 32"},
		{"item-2-lead", "lead", "item-2", []string{"a", "--cwd", filepath.Join(w.dir, "missing")}, "cannot use --cwd"},
		{"item-2-lead", "lead", "item-2", []string{"a", "--cwd", taskFile}, "not a directory"},
		{"item-3-lead", "lead", "item-3", []string{"a", "--cwd", cwd}, "job item-3 is not open"},
	}
	for _, c := range cases {
		args := append([]string{"spawn"}, c.args...)
		args = append(args, "--task-file", taskFile)
		out := w.asAgent(c.agent, c.role, "", c.job, args...)
		if out.code != 1 {
			t.Errorf("%s %v: %+v", c.agent, c.args, out)
		}
		if !strings.Contains(out.stderr, c.says) {
			t.Errorf("%v: stderr %q does not say %q", c.args, out.stderr, c.says)
		}
	}
	if _, err := os.Stat(filepath.Join(w.dir, "argv")); err == nil {
		t.Error("herdr was prompted")
	}
	conn := w.ledger()
	defer conn.Close()
	var rows int
	if err := conn.QueryRow("SELECT count(*) FROM agents").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 5 {
		t.Errorf("a refused spawn wrote a row: %d rows", rows)
	}
}

func TestSpawnRefusesAnUnreadableOrEmptyTaskFileAndNeedsItsFlags(t *testing.T) {
	w := newWorld(t)
	ledgerWith(w, []struct{ name, role, job string }{{"item-1-lead", "lead", "item-1"}})
	empty := task(w, "empty.md", "")
	cwd := dir(w, "wt")
	for _, file := range []string{filepath.Join(w.dir, "missing.md"), empty} {
		out := w.asAgent("item-1-lead", "lead", "thread-1", "item-1", "spawn", "a", "--cwd", cwd, "--task-file", file)
		if out.code != 1 {
			t.Errorf("%s: %+v", file, out)
		}
	}
	out := w.run("", []string{"spawn", "a"})
	if out.code != 1 || !strings.Contains(out.stderr, "--task-file <PATH>") || !strings.Contains(out.stderr, "--cwd <DIR>") {
		t.Errorf("%+v", out)
	}
	out = w.asAgent("item-1-lead", "lead", "thread-1", "item-1", "spawn", "a", "--task-file", empty, "--branch", "x")
	if out.code != 1 || !strings.Contains(out.stderr, "-branch") {
		t.Errorf("--branch: %+v", out)
	}
}

func TestJobStartRefusalsHappenBeforeHerdrIsCalled(t *testing.T) {
	w := newWorld(t)
	taskFile := task(w, "task.md", "build item 1\n")
	dir(w, filepath.Join("home", "dev", "example-dataset"))
	ledgerWith(w, []struct{ name, role, job string }{{"item-1-lead", "lead", "item-1"}})
	conn := w.ledger()
	if _, err := conn.Exec("UPDATE jobs SET key = 'PR-7' WHERE job = 'item-1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec("UPDATE agents SET parent_issue = 'EX-10' WHERE name = 'item-1-lead'"); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	// Linear off everywhere here (no config), so the parent flags are
	// refused before the ledger is read.
	cases := []struct {
		agent, role, job string
		args             []string
		says             string
	}{
		{"item-1-lead", "lead", "item-1", []string{"item-2"}, "cannot start a job"},
		{"item-1-a", "worker", "item-1", []string{"item-2"}, "cannot start a job"},
		{"thread-1", "thread", "", []string{"Item_2"}, "[a-z0-9-]"},
		{"thread-1", "thread", "", []string{"watch"}, "reserved"},
		{"thread-1", "thread", "", []string{"1"}, "start with a letter"},
		{"thread-1", "thread", "", []string{longJob}, "at most 32"},
		{"thread-1", "thread", "", []string{"item-2", "--parent-issue", "EX-1", "--new-parent", "T"}, "exclude each other"},
		{"thread-1", "thread", "", []string{"item-2", "--parent-issue", "ex-1"}, "not a Linear issue identifier"},
		{"thread-1", "thread", "", []string{"item-2", "--new-parent", " "}, "needs a title"},
		{"thread-1", "thread", "", []string{"item-2", "--key", ""}, "--key must not be empty"},
		{"thread-1", "thread", "", []string{"item-2", "--key", "a\tb"}, "--key must not be empty"},
		{"thread-1", "thread", "", []string{"item-2", "--repo", "../x"}, "directory name"},
		{"thread-1", "thread", "", []string{"item-2", "--repo", "no-such-repo"}, "no checkout"},
		{"thread-1", "thread", "", []string{"item-2", "--new-parent", "T"}, "single-repo jobs"},
		{"thread-1", "thread", "", []string{"item-2", "--repo", "example-dataset", "--parent-issue", "EX-1"}, "sets no linear"},
		{"thread-1", "thread", "", []string{"item-2", "--repo", "example-dataset", "--new-parent", "T"}, "sets no linear"},
		{"thread-1", "thread", "", []string{"item-1", "--repo", "example-dataset"}, "already open"},
		{"thread-1", "thread", "", []string{"item-2", "--repo", "example-dataset", "--key", "PR-7"}, "item-1 is open with the same key"},
	}
	for _, c := range cases {
		args := append([]string{"job", "start"}, c.args...)
		args = append(args, "--task-file", taskFile)
		out := w.asAgent(c.agent, c.role, "", c.job, args...)
		if out.code != 1 {
			t.Errorf("%s %v: %+v", c.agent, c.args, out)
		}
		if !strings.Contains(out.stderr, c.says) {
			t.Errorf("%v: stderr %q does not say %q", c.args, out.stderr, c.says)
		}
	}
	if _, err := os.Stat(filepath.Join(w.dir, "argv")); err == nil {
		t.Error("herdr was prompted")
	}
	if got := jobState(w, "item-2"); got != "none" {
		t.Errorf("a refused start wrote a job row: %s", got)
	}
	if _, err := os.Stat(filepath.Join(w.dir, "home", "x-repo")); err == nil {
		t.Error("a refused start made a cross-repo directory")
	}
	out := w.run("", []string{"job", "start", "item-2"})
	if out.code != 1 || !strings.Contains(out.stderr, "--task-file <PATH>") {
		t.Errorf("%+v", out)
	}
}

func TestDoneReportsToTheParentAndEndsTheRow(t *testing.T) {
	w := newWorld(t)
	ledgerWith(w, []struct{ name, role, job string }{{"item-1-a", "worker", "item-1"}})
	report := task(w, "report.md", "ok\n")
	w.herdr(`{"id":"cli:agent:prompt","result":{"type":"agent_prompted"}}`, "", false)
	w.fakeAtb("")
	out := w.asAgent("item-1-a", "worker", "item-1-lead", "item-1", "done", "--report-file", report)
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	resolved, err := filepath.EvalSymlinks(report)
	if err != nil {
		t.Fatal(err)
	}
	argv, _ := os.ReadFile(filepath.Join(w.dir, "argv"))
	expected := "agent\nprompt\nitem-1-lead\n[FROM: item-1-a]\nitem-1-a is done. Report: " + resolved +
		"\n\n--wait\n--until\nworking\n--timeout\n20000\n"
	if string(argv) != expected {
		t.Errorf("argv = %q, want %q", argv, expected)
	}
	if got := w.calls(); got != "herdr agent prompt\n" {
		t.Errorf("without FLEET_ISSUE atb was called: %q", got)
	}
	if got := state(w, "item-1-a"); got != "ended" {
		t.Errorf("state = %s", got)
	}
}

func TestDoneKeepsTheRowLiveWhenDeliveryIsUnclear(t *testing.T) {
	w := newWorld(t)
	ledgerWith(w, []struct{ name, role, job string }{{"item-1-a", "worker", "item-1"}})
	w.herdr(`{"error":{"code":"agent_prompt_stalled","message":"no state change"}}`, "", false)
	out := w.asAgent("item-1-a", "worker", "item-1-lead", "item-1", "done")
	if out.code != 2 {
		t.Errorf("%+v", out)
	}
	if got := state(w, "item-1-a"); got != "active" {
		t.Errorf("state = %s", got)
	}
}

func TestDoneIsForWorkersOnly(t *testing.T) {
	w := newWorld(t)
	out := w.asThread("done")
	if out.code != 1 || !strings.Contains(out.stderr, "a thread does not report with `done`") {
		t.Errorf("thread: %+v", out)
	}
	out = w.asAgent("item-1-lead", "lead", "thread-1", "item-1", "done")
	if out.code != 1 || !strings.Contains(out.stderr, "a lead ends its job with `fleet job end`") {
		t.Errorf("lead: %+v", out)
	}
	out = w.asAgent("item-1-a", "worker", "", "item-1", "done")
	if out.code != 1 || !strings.Contains(out.stderr, "FLEET_PARENT") {
		t.Errorf("worker without parent: %+v", out)
	}
	if _, err := os.Stat(filepath.Join(w.dir, "argv")); err == nil {
		t.Error("herdr was prompted")
	}
}

func TestJobEndRefusesTheWrongCallerLiveWorkersAndAMissingReport(t *testing.T) {
	w := newWorld(t)
	ledgerWith(w, []struct{ name, role, job string }{{"item-1-lead", "lead", "item-1"}, {"item-1-a", "worker", "item-1"}})
	report := task(w, "report.md", "what I did\n")
	asLead := func(args ...string) result {
		return w.asAgent("item-1-lead", "lead", "thread-1", "item-1", append([]string{"job", "end"}, args...)...)
	}
	for name, c := range map[string]struct {
		out  result
		want string
	}{
		"thread without --force":      {w.asThread("job", "end", "item-1"), "a thread cannot end a job"},
		"worker":                      {w.asAgent("item-1-a", "worker", "item-1-lead", "item-1", "job", "end", "--report-file", report), "a worker cannot end a job"},
		"lead with --force":           {asLead("item-1", "--force"), "a lead cannot reclaim a job"},
		"lead without report":         {asLead(), "--report-file is required"},
		"lead of another job":         {asLead("item-2", "--report-file", report), "your job is item-1, not item-2"},
		"lead with live workers":      {asLead("--report-file", report), "live workers: item-1-a"},
		"--force without job":         {w.asThread("job", "end", "--force"), "--force needs the job"},
		"--force with report":         {w.asThread("job", "end", "item-1", "--force", "--report-file", report), "--force takes no report"},
		"--force with --close-parent": {w.asThread("job", "end", "item-1", "--force", "--close-parent", "done"), "no --close-parent"},
		"lead with a bad --close-parent": {asLead("--report-file", report, "--close-parent", "closed"),
			"--close-parent is `done` or `abandoned`"},
	} {
		if c.out.code != 1 || !strings.Contains(c.out.stderr, c.want) {
			t.Errorf("%s: %+v", name, c.out)
		}
	}
	for _, name := range []string{"item-1-lead", "item-1-a"} {
		if got := state(w, name); got != "active" {
			t.Errorf("state(%s) = %s", name, got)
		}
	}
	if got := jobState(w, "item-1"); got != "open" {
		t.Errorf("job state = %s", got)
	}
}

func TestJobEndForceEndsTheJobAndRemovesTheCrossRepoDirectory(t *testing.T) {
	w := newWorld(t)
	crossRepo := dir(w, filepath.Join("home", "x-repo", "general", "item-7"))
	if err := os.WriteFile(filepath.Join(crossRepo, "notes.md"), []byte("scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	conn := w.ledger()
	for _, stmt := range []string{
		"INSERT INTO jobs (job, repo, lead_cwd, state, started_at) VALUES ('item-7', '', '" + crossRepo + "', 'open', 0)",
		"INSERT INTO agents (name, role, job, parent, state, started_at, cwd) VALUES ('item-7-lead', 'lead', 'item-7', 'thread-1', 'starting', 0, '" + crossRepo + "')",
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	conn.Close()
	// An agent in the directory blocks the removal.
	w.closeHerdr(`{"name":"squatter","pane_id":"p1","cwd":"` + filepath.Join(crossRepo, "sub") + `"}`)
	out := w.asThread("job", "end", "item-7", "--force")
	if out.code != 5 || !strings.Contains(out.stderr, "agent squatter") {
		t.Errorf("%+v", out)
	}
	if !dirExists(crossRepo) {
		t.Error("the directory was removed while in use")
	}
	// A git checkout inside it with uncommitted changes blocks the removal
	// too, and the job stays open until it is clean.
	w.closeHerdr("")
	nested := dir(w, filepath.Join("home", "x-repo", "general", "item-7", "clone"))
	if out, err := exec.Command("git", "init", "-q", nested).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(nested, "draft.md"), []byte("unsaved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out = w.asThread("job", "end", "item-7", "--force")
	if out.code != 5 || !strings.Contains(out.stderr, "uncommitted changes in "+nested) {
		t.Errorf("dirty checkout: %+v", out)
	}
	if !fileExists(filepath.Join(nested, "draft.md")) || jobState(w, "item-7") != "open" {
		t.Errorf("dirty checkout: draft kept %v, job %s", fileExists(filepath.Join(nested, "draft.md")), jobState(w, "item-7"))
	}
	if err := os.Remove(filepath.Join(nested, "draft.md")); err != nil {
		t.Fatal(err)
	}
	out = w.asThread("job", "end", "item-7", "--force")
	if out.code != 0 || !strings.Contains(out.stdout, "removed directory "+crossRepo) || !strings.Contains(out.stdout, "1 rows ended") {
		t.Errorf("%+v", out)
	}
	if dirExists(crossRepo) {
		t.Error("the directory is still there")
	}
	if got := state(w, "item-7-lead"); got != "ended" {
		t.Errorf("state = %s", got)
	}
	if got := jobState(w, "item-7"); got != "ended abandoned" {
		t.Errorf("job state = %s", got)
	}
	// Again, from a shell with no identity: nothing to do, nothing to end.
	out = w.run("", []string{"job", "end", "item-7", "--force"}, "FLEET_SCOPE="+scope)
	if out.code != 0 || !strings.Contains(out.stdout, "0 rows ended") {
		t.Errorf("again: %+v", out)
	}
}

// jobEndHerdr is a fake herdr for a lead ending job item-1: workspace w1
// labelled item-1 holds the lead (pane p0) and `other` (pane p9); an
// agent is gone from the list once its pane or workspace is closed;
// `pane close` and `workspace close` are
// logged to <dir>/calls, in order with atb's calls. The commands named
// in `failing` ("pane close") exit 2 instead.
func (w *world) jobEndHerdr(failing ...string) {
	w.t.Helper()
	script := `#!/bin/sh
if [ "$1" = --session ]; then shift 2; fi
dir="$(dirname "$0")/.."
case "$1 $2" in
  "` + strings.Join(failing, `"|"`) + `") echo "fake herdr: $1 $2 failing" >&2; exit 2 ;;
  "workspace list")
    if [ -e "$dir/closed-w1" ]; then echo '{"result":{"workspaces":[]}}'; else echo '{"result":{"workspaces":[{"workspace_id":"w1","label":"item-1"}]}}'; fi ;;
  "agent list")
    other=',{"name":"other","pane_id":"p9","workspace_id":"w1","cwd":"/elsewhere"}'
    [ -e "$dir/closed-p9" ] && other=
    agents='{"name":"item-1-lead","pane_id":"p0","workspace_id":"w1","cwd":"/c"}'"$other"
    [ -e "$dir/closed-w1" ] && agents=
    echo '{"result":{"agents":['"$agents"']}}' ;;
  "pane close"|"workspace close") echo "herdr $1 $2 $3" >> "$dir/calls"; touch "$dir/closed-$3"; echo '{"result":{}}' ;;
  *) echo "fake herdr: unexpected command: $*" >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(w.dir, "fake-herdr", "herdr"), []byte(script), 0o755); err != nil {
		w.t.Fatal(err)
	}
}

// endJob runs `job end` as item-1-lead with work order EX-12 and the
// Linear key.
func (w *world) endJob(args ...string) result {
	w.t.Helper()
	return w.run("", append([]string{"job", "end"}, args...),
		"FLEET_AGENT=item-1-lead", "FLEET_ROLE=lead", "FLEET_PARENT=thread-1", "FLEET_SCOPE="+scope,
		"FLEET_JOB=item-1", "FLEET_ISSUE=EX-12", linearKey)
}

func TestJobEndReportsReleasesCleansUpAndClosesTheWorkspaceLast(t *testing.T) {
	// --abandon is the job's own outcome (the work order's release); the
	// parent is closed only with --close-parent, else its claim is given
	// back and it returns to its state from before the job.
	for _, c := range []struct {
		args                            []string
		outcome, release, parentRelease string
	}{
		{nil, "done", "--reason done --done", "--reason job item-1 ended"},
		{[]string{"--abandon"}, "abandoned", "--reason abandoned --abandon", "--reason job item-1 ended"},
		{[]string{"--close-parent", "done"}, "done", "--reason done --done", "--reason done --done"},
		{[]string{"--abandon", "--close-parent", "abandoned"}, "abandoned", "--reason abandoned --abandon", "--reason abandoned --abandon"},
	} {
		w := newWorld(t)
		openJobWithLead(w, "EX-10")
		report := task(w, "report.md", "what the job did\n")
		resolved, err := filepath.EvalSymlinks(report)
		if err != nil {
			t.Fatal(err)
		}
		w.jobEndHerdr()
		w.fakeAtb("")
		outcome, release := c.outcome, c.release
		out := w.endJob(append([]string{"--report-file", report}, c.args...)...)
		if out.code != 0 {
			t.Fatalf("args=%v: %+v", c.args, out)
		}
		conclusion := "Job item-1 ended: " + outcome + ".\nLead: item-1-lead. Work order: EX-12. Report: " + resolved + "\n"
		if !strings.Contains(out.stdout, "ended job item-1: "+outcome+", 1 rows ended\n"+conclusion) {
			t.Errorf("args=%v: stdout %q", c.args, out.stdout)
		}
		// The report and the release, the conclusion and the release, the
		// other agent's pane, and the workspace last.
		calls := w.calls()
		want := []string{
			"atb linear comment EX-12 --body-file " + resolved + " key=set\n",
			"atb linear release EX-12 --agent item-1-lead " + release + " key=set\n",
			"atb linear comment EX-10 --body-file ",
			"atb linear release EX-10 --agent item-1-lead " + c.parentRelease + " key=set\n",
			"herdr pane close p9\n",
			"herdr workspace close w1\n",
		}
		at := 0
		for _, step := range want {
			i := strings.Index(calls[at:], step)
			if i < 0 {
				t.Fatalf("args=%v: %q not after position %d in calls:\n%s", c.args, step, at, calls)
			}
			at += i + len(step)
		}
		if strings.Contains(calls, "pane close p0") {
			t.Errorf("the lead's own pane was closed: %s", calls)
		}
		// The report went to the work order, the conclusion to the parent.
		if got := w.bodies(); got != "what the job did\n"+conclusion {
			t.Errorf("args=%v: bodies written = %q, want the report then %q", c.args, got, conclusion)
		}
		if got := state(w, "item-1-lead"); got != "ended" {
			t.Errorf("state = %s", got)
		}
		if got := jobState(w, "item-1"); got != "ended "+outcome {
			t.Errorf("job state = %s", got)
		}
	}
}

func TestJobEndRefusesToCloseAParentTheJobDoesNotHave(t *testing.T) {
	w := newWorld(t)
	openJobWithLead(w, "")
	w.jobEndHerdr()
	w.fakeAtb("")
	out := w.endJob("--report-file", task(w, "report.md", "what the job did\n"), "--close-parent", "done")
	if out.code != 1 || !strings.Contains(out.stderr, "job item-1 has no parent issue to close") {
		t.Errorf("%+v", out)
	}
	if calls := w.calls(); calls != "" {
		t.Errorf("calls before the refusal: %s", calls)
	}
}

func TestJobEndStopsWhenAnAtbStepFails(t *testing.T) {
	w := newWorld(t)
	openJobWithLead(w, "EX-10")
	report := task(w, "report.md", "what the job did\n")
	w.jobEndHerdr()
	w.fakeAtb("release EX-10")
	out := w.endJob("--report-file", report)
	if out.code != 5 || !strings.Contains(out.stderr, "atb linear release EX-10 failed") {
		t.Errorf("%+v", out)
	}
	if strings.Contains(out.stderr, "lin_api") || strings.Contains(out.stdout, "lin_api") {
		t.Error("the key leaked")
	}
	if strings.Contains(w.calls(), "herdr") {
		t.Errorf("herdr was touched: %s", w.calls())
	}
	if got := state(w, "item-1-lead"); got != "active" {
		t.Errorf("state = %s", got)
	}
	if got := jobState(w, "item-1"); got != "open" {
		t.Errorf("job state = %s", got)
	}
}

func TestJobEndResumesAfterAPartialFailure(t *testing.T) {
	w := newWorld(t)
	openJobWithLead(w, "EX-10")
	report := task(w, "report.md", "what the job did\n")
	// First: the parent's release fails after the work order was commented
	// and released.
	w.jobEndHerdr()
	w.fakeAtb("release EX-10")
	if out := w.endJob("--report-file", report); out.code != 5 {
		t.Fatalf("first: %+v", out)
	}
	// Second: atb now refuses the work order's release, as the real one
	// does for an issue nobody holds; the retry must not ask for it. The
	// cleanup then fails at herdr.
	w.fakeAtb("release EX-12")
	w.jobEndHerdr("pane close")
	before := w.calls()
	out := w.endJob("--report-file", report)
	if out.code != 5 || !strings.Contains(out.stderr, "pane close") {
		t.Fatalf("second: %+v", out)
	}
	second := strings.TrimPrefix(w.calls(), before)
	if strings.Count(second, "atb ") != 1 || !strings.Contains(second, "atb linear release EX-10 --agent item-1-lead --reason job item-1 ended") {
		t.Errorf("second run's atb calls = %q, want only the parent's release", second)
	}
	if got := jobState(w, "item-1"); got != "open" {
		t.Errorf("job state after the second run = %s", got)
	}
	// Third: every Linear step is recorded, so atb is not asked (the
	// calls log shows it); herdr works again.
	w.fakeAtb("release")
	w.jobEndHerdr()
	before = w.calls()
	out = w.endJob("--report-file", report)
	if out.code != 0 {
		t.Fatalf("third: %+v", out)
	}
	third := strings.TrimPrefix(w.calls(), before)
	if strings.Contains(third, "atb ") || !strings.Contains(third, "herdr workspace close w1") {
		t.Errorf("third run's calls = %q, want herdr only, ending with the workspace", third)
	}
	if got := jobState(w, "item-1"); got != "ended done" {
		t.Errorf("job state = %s", got)
	}
	if got := state(w, "item-1-lead"); got != "ended" {
		t.Errorf("state = %s", got)
	}
}

func TestJobEndRetriesAfterTheWorkspaceCloseFailed(t *testing.T) {
	w := newWorld(t)
	openJobWithLead(w, "EX-10")
	report := task(w, "report.md", "what the job did\n")
	w.fakeAtb("")
	w.jobEndHerdr("workspace close")
	out := w.endJob("--report-file", report)
	if out.code != 5 || !strings.Contains(out.stderr, "workspace close") || !strings.Contains(out.stdout, "Job item-1 ended: done.") {
		t.Fatalf("first: %+v", out)
	}
	if got := jobState(w, "item-1"); got != "ended done" {
		t.Errorf("job state after the first run = %s", got)
	}
	// The retry closes the workspace and nothing else: no atb call, rows
	// not ended again, the conclusion not printed again, outcome kept.
	w.fakeAtb("release")
	w.jobEndHerdr()
	before := w.calls()
	out = w.endJob("--report-file", report)
	if out.code != 0 || strings.Contains(out.stdout, "rows ended") || strings.Contains(out.stdout, "Job item-1 ended") {
		t.Fatalf("retry: %+v", out)
	}
	if retry := strings.TrimPrefix(w.calls(), before); retry != "herdr workspace close w1\n" {
		t.Errorf("retry's calls = %q", retry)
	}
	if got := jobState(w, "item-1"); got != "ended done" {
		t.Errorf("job state = %s", got)
	}
	// A third run has nothing to resume: the job is not open.
	if out := w.endJob("--report-file", report); out.code != 1 || !strings.Contains(out.stderr, "not open") {
		t.Errorf("third: %+v", out)
	}
}

func TestJobEndForceFinishesLocallyAndReportsTheLinearStepsNotDone(t *testing.T) {
	w := newWorld(t)
	openJobWithLead(w, "EX-10")
	conn := w.ledger()
	if _, err := conn.Exec("UPDATE agents SET issue = 'EX-12' WHERE name = 'item-1-lead'"); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	report := task(w, "report.md", "what the job did\n")
	// Nothing recorded yet: every Linear step is pending.
	w.closeHerdr("")
	out := w.asThread("job", "end", "item-1", "--force")
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	want := "reclaimed job item-1: 0 left, 1 rows ended\nLinear steps not done for job item-1; finish them by hand:\n" +
		"  - comment the report on work order EX-12\n  - release work order EX-12 (agent item-1-lead)\n" +
		"  - comment the conclusion on parent EX-10\n  - release parent EX-10 (agent item-1-lead)\n"
	if !strings.HasSuffix(out.stdout, want) {
		t.Errorf("stdout = %q, want it to end with %q", out.stdout, want)
	}
	// A second job: the lead's end stops at the parent's release, which
	// keeps failing; the reclaim ends everything local and names only
	// that step.
	openJobWithLead(w, "EX-10")
	w.jobEndHerdr()
	w.fakeAtb("release EX-10")
	for range 2 {
		if out := w.endJob("--report-file", report); out.code != 5 {
			t.Fatalf("lead: %+v", out)
		}
	}
	out = w.asThread("job", "end", "item-1", "--force")
	if out.code != 0 {
		t.Fatalf("force: %+v", out)
	}
	want = "reclaimed job item-1: 0 left, 1 rows ended\nLinear steps not done for job item-1; finish them by hand:\n" +
		"  - release parent EX-10 (agent item-1-lead)\n"
	if !strings.HasSuffix(out.stdout, want) || strings.Contains(out.stdout, "EX-12") {
		t.Errorf("stdout = %q, want it to end with %q", out.stdout, want)
	}
	if !strings.Contains(w.calls(), "herdr workspace close w1") {
		t.Errorf("workspace not closed: %s", w.calls())
	}
	if got := jobState(w, "item-1"); got != "ended abandoned" {
		t.Errorf("job state = %s", got)
	}
	if got := state(w, "item-1-lead"); got != "ended" {
		t.Errorf("state = %s", got)
	}
	// Nothing left: a reclaim again has nothing to report.
	out = w.asThread("job", "end", "item-1", "--force")
	if out.code != 0 || strings.Contains(out.stdout, "Linear") {
		t.Errorf("again: %+v", out)
	}
}

func TestJobEndWithoutLinearSkipsAtb(t *testing.T) {
	w := newWorld(t)
	openJobWithLead(w, "")
	report := task(w, "report.md", "what the job did\n")
	w.jobEndHerdr()
	out := w.asAgent("item-1-lead", "lead", "thread-1", "item-1", "job", "end", "--report-file", report)
	if out.code != 0 || !strings.Contains(out.stdout, "Job item-1 ended: done.\nLead: item-1-lead. Report: ") {
		t.Errorf("%+v", out)
	}
	if strings.Contains(w.calls(), "atb") {
		t.Errorf("atb was called: %s", w.calls())
	}
	if got := jobState(w, "item-1"); got != "ended done" {
		t.Errorf("job state = %s", got)
	}
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// fakeAtb puts a stand-in for atb on PATH. Each call appends its argv, one
// line, to <dir>/calls, with `key=set` when LINEAR_API_KEY reached it. The
// subcommand named by failOn (`comment`, `release`, or a subcommand with
// its first argument such as `claim EX-12`) prints the key it got to
// stdout and stderr and exits 4. Every body file commented is appended to
// <dir>/bodies.
func (w *world) fakeAtb(failOn string) {
	w.t.Helper()
	script := `#!/bin/sh
key=unset
[ -n "$LINEAR_API_KEY" ] && key=set
echo "atb $* key=$key" >> "$(dirname "$0")/../calls"
[ "$2" = comment ] && cat "$5" >> "$(dirname "$0")/../bodies"
if [ "$2" = "` + failOn + `" ] || [ "$2 $3" = "` + failOn + `" ]; then
  echo "error: refused: no holder, key $LINEAR_API_KEY" >&2
  echo "key $LINEAR_API_KEY"
  exit 4
fi
`
	if err := os.WriteFile(filepath.Join(w.dir, "fake-herdr", "atb"), []byte(script), 0o755); err != nil {
		w.t.Fatal(err)
	}
}

// bodies is what every `atb linear comment` was given.
func (w *world) bodies() string {
	w.t.Helper()
	data, _ := os.ReadFile(filepath.Join(w.dir, "bodies"))
	return string(data)
}

func (w *world) calls() string {
	w.t.Helper()
	data, _ := os.ReadFile(filepath.Join(w.dir, "calls"))
	return string(data)
}

// The key is a made-up value; it must reach atb and nothing else.
const linearKey = "LINEAR_API_KEY=lin_api_fake0xK3Y"

// doneWithIssue runs `done` as worker item-1-a with FLEET_ISSUE set to
// issue and the Linear key in the environment.
func (w *world) doneWithIssue(issue string, args ...string) result {
	w.t.Helper()
	return w.run("", append([]string{"done"}, args...),
		"FLEET_AGENT=item-1-a", "FLEET_ROLE=worker", "FLEET_PARENT=item-1-lead", "FLEET_SCOPE="+scope,
		"FLEET_JOB=item-1", "FLEET_ISSUE="+issue, linearKey)
}

func TestDoneWritesTheReportAndReleasesBeforeDelivering(t *testing.T) {
	for _, abandon := range []bool{false, true} {
		w := newWorld(t)
		ledgerWith(w, []struct{ name, role, job string }{{"item-1-a", "worker", "item-1"}})
		report := task(w, "report.md", "what I did\n")
		w.herdr(`{"id":"cli:agent:prompt","result":{"type":"agent_prompted"}}`, "", false)
		w.fakeAtb("")
		resolved, err := filepath.EvalSymlinks(report)
		if err != nil {
			t.Fatal(err)
		}
		args := []string{"--report-file", report}
		release, body := "--reason done --done", "item-1-a is done. Issue: EX-7. Report: "+resolved+"\n"
		if abandon {
			args = append(args, "--abandon")
			release = "--reason abandoned --abandon"
			body = "item-1-a abandoned the task. Issue: EX-7. Report: " + resolved + "\n"
		}
		out := w.doneWithIssue("EX-7", args...)
		if out.code != 0 {
			t.Fatalf("abandon=%v: %+v", abandon, out)
		}
		expected := "atb linear comment EX-7 --body-file " + resolved + " key=set\n" +
			"atb linear release EX-7 --agent item-1-a " + release + " key=set\n" +
			"herdr agent prompt\n"
		if got := w.calls(); got != expected {
			t.Errorf("abandon=%v: calls = %q, want %q", abandon, got, expected)
		}
		argv, _ := os.ReadFile(filepath.Join(w.dir, "argv"))
		if want := "item-1-lead\n[FROM: item-1-a]\n" + body + "\n--wait"; !strings.Contains(string(argv), want) {
			t.Errorf("abandon=%v: argv = %q, want %q in it", abandon, argv, want)
		}
		if got := state(w, "item-1-a"); got != "ended" {
			t.Errorf("abandon=%v: state = %s", abandon, got)
		}
	}
}

func TestDoneStopsWhenAnAtbStepFails(t *testing.T) {
	for _, step := range []string{"comment", "release"} {
		w := newWorld(t)
		ledgerWith(w, []struct{ name, role, job string }{{"item-1-a", "worker", "item-1"}})
		report := task(w, "report.md", "what I did\n")
		w.herdr(`{"id":"cli:agent:prompt","result":{"type":"agent_prompted"}}`, "", false)
		w.fakeAtb(step)
		out := w.doneWithIssue("EX-7", "--report-file", report)
		if out.code != 5 || !strings.Contains(out.stderr, "atb linear "+step+" EX-7 failed (exit status: 4)") {
			t.Errorf("%s: %+v", step, out)
		}
		if strings.Contains(out.stderr+out.stdout, "fake0xK3Y") {
			t.Errorf("%s: the key leaked: %+v", step, out)
		}
		if strings.Contains(w.calls(), "herdr") {
			t.Errorf("%s: the parent was prompted: %q", step, w.calls())
		}
		if got := state(w, "item-1-a"); got != "active" {
			t.Errorf("%s: state = %s", step, got)
		}
	}
}

func TestDoneRefusesBadReportArguments(t *testing.T) {
	w := newWorld(t)
	ledgerWith(w, []struct{ name, role, job string }{{"item-1-a", "worker", "item-1"}})
	report := task(w, "report.md", "what I did\n")
	w.fakeAtb("")
	for _, c := range []struct {
		issue string
		args  []string
		says  string
	}{
		{"EX-7", nil, "--report-file is required"},
		{"EX-7", []string{"--abandon"}, "--report-file is required"},
		{"", []string{"--abandon"}, "FLEET_ISSUE, which is empty"},
		{"", []string{"--report-file", report, "--abandon"}, "FLEET_ISSUE, which is empty"},
		{"EX-7", []string{"--report-file", filepath.Join(w.dir, "missing.md")}, "cannot read"},
	} {
		out := w.doneWithIssue(c.issue, c.args...)
		if out.code != 1 || !strings.Contains(out.stderr, c.says) {
			t.Errorf("FLEET_ISSUE=%s %v: %+v", c.issue, c.args, out)
		}
	}
	if w.calls() != "" {
		t.Errorf("calls = %q", w.calls())
	}
	if got := state(w, "item-1-a"); got != "active" {
		t.Errorf("state = %s", got)
	}
}

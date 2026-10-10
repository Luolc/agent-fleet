// Integration tests for the starts and their work orders: `job start` and
// `spawn` against a fake herdr that lets a start run to the end and a
// fake atb, both logging every call to <dir>/calls in order. Hermetic,
// see main_test.go.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startHerdr is a fake herdr that lets a start run to the end: a workspace
// labelled item-1, plus one labelled wire once <dir>/wire-workspace
// exists (none for any other label), an agent that
// starts at its input box, and a prompt that is delivered. `workspace
// create`, `tab create` and `agent prompt` keep their argv in
// <dir>/workspace-argv, <dir>/tab-argv and <dir>/argv.
const startHerdr = `#!/bin/sh
if [ "$1" = --session ]; then shift 2; fi
dir="$(dirname "$0")/.."
echo "herdr $1 $2" >> "$dir/calls"
case "$1 $2" in
  "agent list") echo '{"result":{"agents":[]}}' ;;
  "workspace list")
    if [ -e "$dir/wire-workspace" ]; then
      echo '{"result":{"workspaces":[{"workspace_id":"w1","label":"item-1"},{"workspace_id":"w3","label":"wire"}]}}'
    else
      echo '{"result":{"workspaces":[{"workspace_id":"w1","label":"item-1"}]}}'
    fi ;;
  "workspace create")
    printf '%s\n' "$@" > "$dir/workspace-argv"
    echo '{"result":{"root_pane":{"workspace_id":"w2","tab_id":"t2","pane_id":"p2"}}}' ;;
  "tab create")
    printf '%s\n' "$@" > "$dir/tab-argv"
    echo '{"result":{"root_pane":{"workspace_id":"w1","tab_id":"t1","pane_id":"p1"}}}' ;;
  "tab rename"|"agent start"|"pane rename") echo '{"result":{}}' ;;
  "agent read") printf '%s\n' "────────────" "❯ " "────────────" ;;
  "agent get") echo '{"result":{"agent":{"agent_status":"idle"}}}' ;;
  "agent prompt")
    printf '%s\n' "$@" > "$dir/argv"
    echo '{"id":"cli:agent:prompt","result":{"type":"agent_prompted"}}' ;;
  *) echo "fake herdr: unexpected command: $*" >&2; exit 2 ;;
esac
`

// useStartHerdr puts startHerdr on PATH.
func (w *world) useStartHerdr() {
	w.t.Helper()
	if err := os.WriteFile(filepath.Join(w.dir, "fake-herdr", "herdr"), []byte(startHerdr), 0o755); err != nil {
		w.t.Fatal(err)
	}
}

// configure writes the repo's .fleet/config.json under the world's ~/dev
// and returns the checkout.
func (w *world) configure(body string) string {
	w.t.Helper()
	checkout := filepath.Join(w.dir, "home", "dev", "example-dataset")
	if err := os.MkdirAll(filepath.Join(checkout, ".fleet"), 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, ".fleet", "config.json"), []byte(body), 0o644); err != nil {
		w.t.Fatal(err)
	}
	return checkout
}

const withLinear = `{"resource_check": false, "linear": {"team": "EX", "project": "Example project"}}`

// fakeAtbCreating is fakeAtb whose `create` prints the JSON of EX-12 (or
// EX-11 for a top-level issue, one without --parent) and whose `query`
// prints a team and a project.
func (w *world) fakeAtbCreating(failOn string) {
	w.t.Helper()
	w.fakeAtb(failOn)
	path := filepath.Join(w.dir, "fake-herdr", "atb")
	script, err := os.ReadFile(path)
	if err != nil {
		w.t.Fatal(err)
	}
	script = append(script, []byte(`if [ "$2" = create ]; then
  case " $* " in
    *" --parent "*) echo '{"identifier":"EX-12","url":"https://linear.example.test/EX-12"}' ;;
    *) echo '{"identifier":"EX-11","url":"https://linear.example.test/EX-11"}' ;;
  esac
fi
[ "$2" = query ] && echo '{"issue":{"team":{"key":"QT"},"project":{"name":"Queried project"}}}'
exit 0
`)...)
	if err := os.WriteFile(path, script, 0o755); err != nil {
		w.t.Fatal(err)
	}
}

// openJobWithLead puts job item-1 (repo example-dataset, the given parent
// issue) in the ledger as open with item-1-lead live.
func openJobWithLead(w *world, parentIssue string) {
	w.t.Helper()
	conn := w.ledger()
	defer conn.Close()
	checkout := filepath.Join(w.dir, "home", "dev", "example-dataset")
	for _, stmt := range []string{
		"INSERT INTO jobs (job, parent_issue, repo, lead_cwd, state, started_at) VALUES ('item-1', ?1, 'example-dataset', ?2, 'open', 0)",
		"INSERT INTO agents (name, role, job, parent, state, started_at, cwd, parent_issue) " +
			"VALUES ('item-1-lead', 'lead', 'item-1', 'thread-1', 'active', 0, ?2, ?1)",
	} {
		if _, err := conn.Exec(stmt, parentIssue, checkout); err != nil {
			w.t.Fatal(err)
		}
	}
}

// row is the issue, parent issue, state and cwd of the newest row named
// name, or "none".
func row(w *world, name string) string {
	w.t.Helper()
	conn := w.ledger()
	defer conn.Close()
	var issue, parentIssue, state, cwd string
	if err := conn.QueryRow("SELECT issue, parent_issue, state, cwd FROM agents WHERE name = ?1 ORDER BY id DESC LIMIT 1",
		name).Scan(&issue, &parentIssue, &state, &cwd); err != nil {
		return "none"
	}
	return issue + " " + parentIssue + " " + state + " " + cwd
}

// jobRow is the parent issue, key, repo, lead cwd and state of the newest
// job named job, or "none".
func jobRow(w *world, job string) string {
	w.t.Helper()
	conn := w.ledger()
	defer conn.Close()
	var parent, key, repo, cwd, state string
	if err := conn.QueryRow("SELECT parent_issue, key, repo, lead_cwd, state FROM jobs WHERE job = ?1 ORDER BY id DESC LIMIT 1",
		job).Scan(&parent, &key, &repo, &cwd, &state); err != nil {
		return "none"
	}
	return strings.Join([]string{parent, key, repo, cwd, state}, " ")
}

func (w *world) spawnWorker(args ...string) result {
	w.t.Helper()
	return w.run("", append([]string{"spawn", "a"}, args...),
		"FLEET_AGENT=item-1-lead", "FLEET_ROLE=lead", "FLEET_PARENT=thread-1", "FLEET_SCOPE="+scope,
		"FLEET_JOB=item-1", linearKey)
}

func (w *world) startJob(args ...string) result {
	w.t.Helper()
	return w.run("", append([]string{"job", "start"}, args...),
		"FLEET_AGENT=thread-1", "FLEET_ROLE=thread", "FLEET_SCOPE="+scope, linearKey)
}

func TestSpawnCreatesAndClaimsTheWorkOrderBeforeAnythingElse(t *testing.T) {
	w := newWorld(t)
	w.configure(withLinear)
	openJobWithLead(w, "EX-10")
	w.useStartHerdr()
	w.fakeAtbCreating("")
	cwd := dir(w, "wt")
	taskFile := task(w, "task.md", "\n# Import the A table\nall rows\n")
	out := w.spawnWorker("--task-file", taskFile, "--cwd", cwd)
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	resolved, err := filepath.EvalSymlinks(taskFile)
	if err != nil {
		t.Fatal(err)
	}
	// The checks only read herdr; the work order comes before anything is
	// created.
	want := []string{
		"herdr agent list",
		"herdr workspace list",
		"atb linear create --team EX --project Example project --parent EX-10 --label worker --title Import the A table " +
			"--description-file " + resolved + " --json key=set",
		"atb linear claim EX-12 --agent item-1-a --source item-1-lead --scope example-dataset: job item-1 key=set",
		"herdr tab create",
		"herdr agent start",
		"herdr pane rename",
		"herdr agent read",
		"herdr agent get",
		"herdr agent prompt",
	}
	if got := strings.Split(strings.TrimSpace(w.calls()), "\n"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls = %q, want %q", got, want)
	}
	tabArgv, _ := os.ReadFile(filepath.Join(w.dir, "tab-argv"))
	for _, want := range []string{"--workspace\nw1\n", "--label\na\n", "--cwd\n" + cwd + "\n",
		"--env\nFLEET_AGENT=item-1-a\n", "--env\nFLEET_ROLE=worker\n", "--env\nFLEET_PARENT=item-1-lead\n",
		"--env\nFLEET_SCOPE=" + scope + "\n", "--env\nFLEET_JOB=item-1\n", "--env\nFLEET_ISSUE=EX-12\n"} {
		if !strings.Contains(string(tabArgv), want) {
			t.Errorf("tab create argv = %q, want %q in it", tabArgv, want)
		}
	}
	if strings.Contains(string(tabArgv), "FLEET_REPO") {
		t.Errorf("tab create argv still has FLEET_REPO: %q", tabArgv)
	}
	argv, _ := os.ReadFile(filepath.Join(w.dir, "argv"))
	// The worker's first message: the worker prompt, then the task under
	// the work order's URL.
	for _, want := range []string{"agent\nprompt\nitem-1-a\n[FROM: item-1-lead]\nYou are a worker run by fleet: item-1-a, " +
		"one task in the job item-1 (scope " + scope + "), for your lead item-1-lead. You run in " + cwd + ".",
		"FLEET_ISSUE=EX-12 (your work order, https://linear.example.test/EX-12)",
		"\n## Your task\n\nWork order: https://linear.example.test/EX-12\n\n\n# Import the A table\nall rows\n\n--wait\n"} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("delivered argv = %q, want %q in it", argv, want)
		}
	}
	if got := row(w, "item-1-a"); got != "EX-12 EX-10 active "+cwd {
		t.Errorf("row = %s", got)
	}
	if !strings.Contains(out.stdout, "started item-1-a in job item-1 ("+cwd+")") {
		t.Errorf("stdout = %q", out.stdout)
	}
}

func TestSpawnStopsWhenAnAtbStepFails(t *testing.T) {
	for _, step := range []string{"create", "claim"} {
		w := newWorld(t)
		w.configure(withLinear)
		openJobWithLead(w, "EX-10")
		w.useStartHerdr()
		w.fakeAtbCreating(step)
		out := w.spawnWorker("--task-file", task(w, "task.md", "import\n"), "--cwd", dir(w, "wt"))
		if out.code != 5 {
			t.Errorf("%s: %+v", step, out)
		}
		failed := "atb linear create failed (exit status: 4)"
		if step == "claim" {
			failed = "atb linear claim EX-12 failed (exit status: 4)"
			if !strings.Contains(out.stderr, "work order EX-12 (https://linear.example.test/EX-12), not claimed") {
				t.Errorf("claim: the created issue is not listed: %q", out.stderr)
			}
		}
		if !strings.Contains(out.stderr, failed) {
			t.Errorf("%s: stderr %q does not say %q", step, out.stderr, failed)
		}
		if strings.Contains(out.stderr+out.stdout, "fake0xK3Y") {
			t.Errorf("%s: the key leaked: %+v", step, out)
		}
		if _, after, _ := strings.Cut(w.calls(), "atb linear "+step); strings.Contains(after, "herdr") {
			t.Errorf("%s: herdr was called after atb: %q", step, w.calls())
		}
		// The reserved row stays for `job end --force`, keeping the work order
		// once that was created.
		want := " EX-10 starting " + dir(w, "wt")
		if step == "claim" {
			want = "EX-12" + want
		}
		if got := row(w, "item-1-a"); got != want {
			t.Errorf("%s: row = %s, want %s", step, got, want)
		}
		if !strings.Contains(out.stderr, "ledger row item-1-a (state starting)") || !strings.Contains(out.stderr, "find out why from the error above first; the cleanup is the lead's call, and `fleet job end item-1 --force`") {
			t.Errorf("%s: the row and the cleanup are not listed: %q", step, out.stderr)
		}
	}
}

func TestSpawnChecksTheConfigAndTheParentIssueBeforeAnythingIsCreated(t *testing.T) {
	cases := []struct {
		name, config, parent string
		task                 string
		says                 string
	}{
		{"a job without a parent issue", withLinear, "", "import\n", "has no parent issue"},
		{"a title-less task", withLinear, "EX-10", "#\n##\n", "gives no title"},
		{"an invalid config", `{"linear": {"team": "EX"}}`, "EX-10", "import\n", "config.json: linear.project"},
		{"a null linear", `{"linear": null}`, "EX-10", "import\n", "linear: must not be null"},
		{"the job cap", `{"max_agents_per_job": 1}`, "EX-10", "import\n", "the cap is 1"},
	}
	for _, c := range cases {
		w := newWorld(t)
		w.configure(c.config)
		openJobWithLead(w, c.parent)
		w.fakeAtbCreating("")
		out := w.spawnWorker("--task-file", task(w, "task.md", c.task), "--cwd", dir(w, "wt"))
		if out.code != 1 || !strings.Contains(out.stderr, c.says) {
			t.Errorf("%s: %+v, want exit 1 saying %q", c.name, out, c.says)
		}
		if w.calls() != "" {
			t.Errorf("%s: calls = %q", c.name, w.calls())
		}
	}
}

func TestSpawnWithoutLinearCallsNoAtb(t *testing.T) {
	w := newWorld(t)
	w.configure(`{"resource_check": false}`)
	openJobWithLead(w, "")
	w.useStartHerdr()
	w.fakeAtbCreating("")
	out := w.spawnWorker("--task-file", task(w, "task.md", "# Import\n"), "--cwd", dir(w, "wt"))
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if strings.Contains(w.calls(), "atb") {
		t.Errorf("calls = %q", w.calls())
	}
	argv, _ := os.ReadFile(filepath.Join(w.dir, "argv"))
	if strings.Contains(string(argv), "Work order") {
		t.Errorf("argv = %q", argv)
	}
}

func TestJobStartCreatesTheParentClaimsItAndMakesTheWorkOrderFirst(t *testing.T) {
	w := newWorld(t)
	checkout := w.configure(withLinear)
	w.useStartHerdr()
	w.fakeAtbCreating("")
	taskFile := task(w, "task.md", "# Import the B table\nall rows\n")
	out := w.startJob("item-2", "--repo", "example-dataset", "--new-parent", "Import B", "--key", "B-1",
		"--task-file", taskFile, "--model", "opus")
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	resolved, err := filepath.EvalSymlinks(taskFile)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"herdr agent list",
		"herdr workspace list",
		"atb linear create --team EX --project Example project --title Import B --description-file " + resolved + " --json key=set",
		"atb linear claim EX-11 --agent item-2-lead --source thread-1 --scope example-dataset: job item-2 key=set",
		"atb linear create --team EX --project Example project --parent EX-11 --label lead --title Import the B table " +
			"--description-file " + resolved + " --json key=set",
		"atb linear claim EX-12 --agent item-2-lead --source thread-1 --scope example-dataset: job item-2 key=set",
		"herdr workspace create",
		"herdr tab rename",
		"herdr agent start",
		"herdr pane rename",
		"herdr agent read",
		"herdr agent get",
		"herdr agent prompt",
	}
	if got := strings.Split(strings.TrimSpace(w.calls()), "\n"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls = %q, want %q", got, want)
	}
	wsArgv, _ := os.ReadFile(filepath.Join(w.dir, "workspace-argv"))
	for _, want := range []string{"--label\nitem-2\n", "--cwd\n" + checkout + "\n",
		"--env\nFLEET_AGENT=item-2-lead\n", "--env\nFLEET_ROLE=lead\n", "--env\nFLEET_PARENT=thread-1\n",
		"--env\nFLEET_SCOPE=" + scope + "\n", "--env\nFLEET_JOB=item-2\n", "--env\nFLEET_ISSUE=EX-12\n"} {
		if !strings.Contains(string(wsArgv), want) {
			t.Errorf("workspace create argv = %q, want %q in it", wsArgv, want)
		}
	}
	argv, _ := os.ReadFile(filepath.Join(w.dir, "argv"))
	// The lead's first message: the lead prompt, the latest message of the
	// home thread (none: the caller has no thread), then the task under
	// the work order's URL.
	for _, want := range []string{"agent\nprompt\nitem-2-lead\n[FROM: thread-1]\nYou are a lead run by fleet: you run the job item-2 " +
		"(scope " + scope + ") for the people in its home thread. The job is single-repo: its repo is example-dataset",
		"FLEET_ISSUE=EX-12 (your work order, https://linear.example.test/EX-12). The job's parent issue is EX-11;",
		"\n## Latest message from a person in the home thread\n\nfleet has no message from a person recorded for this " +
			"job's home thread; the task below is all there is.\n\n## Your task\n\nWork order: https://linear.example.test/EX-12\n\n# Import the B table\nall rows\n\n--wait\n"} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("delivered argv = %q, want %q in it", argv, want)
		}
	}
	if got := jobRow(w, "item-2"); got != "EX-11 B-1 example-dataset "+checkout+" open" {
		t.Errorf("job row = %s", got)
	}
	if got := row(w, "item-2-lead"); got != "EX-12 EX-11 active "+checkout {
		t.Errorf("row = %s", got)
	}
	if !strings.Contains(out.stdout, "started item-2-lead in job item-2 ("+checkout+")") {
		t.Errorf("stdout = %q", out.stdout)
	}
	if _, err := os.Stat(filepath.Join(w.dir, "home", "x-repo")); err == nil {
		t.Error("a single-repo job made a cross-repo directory")
	}
	// The same key, parent or name again: refused before herdr or atb.
	w.herdr("unused", "", false)
	w.fakeAtb("")
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"item-3", "--key", "B-1"}, "job item-2 is open with the same key \"B-1\""},
		{[]string{"item-3", "--parent-issue", "EX-11"}, "item-2-lead is live on parent issue EX-11; talk to it"},
		{[]string{"item-2", "--parent-issue", "EX-20"}, "job item-2 is already open"},
	} {
		args := append(c.args, "--repo", "example-dataset", "--task-file", taskFile)
		if c.args[0] == "item-3" && len(c.args) == 3 && c.args[1] == "--key" {
			args = append(args, "--parent-issue", "EX-21")
		}
		out := w.startJob(args...)
		if out.code != 1 || !strings.Contains(out.stderr, c.says) {
			t.Errorf("%v: %+v, want exit 1 saying %q", c.args, out, c.says)
		}
	}
	if w.calls() != "" {
		t.Errorf("a refused start called something: %q", w.calls())
	}
}

func TestJobStartOfACrossRepoJobReadsTheParentsTeamAndProject(t *testing.T) {
	w := newWorld(t)
	w.useStartHerdr()
	w.fakeAtbCreating("")
	taskFile := task(w, "task.md", "Wire the two repos\n")
	dir(w, filepath.Join("home", "x-repo", "general"))
	crossRepo := filepath.Join(w.dir, "home", "x-repo", "general", "wire")
	// No config anywhere: the defaults, with the resource check on, so the
	// machine's load decides between 0 and a refusal.
	out := w.startJob("wire", "--parent-issue", "EX-10", "--task-file", taskFile)
	if out.code != 0 {
		if strings.Contains(out.stderr, "try again later") {
			t.Skip("the machine is loaded; the resource check refused")
		}
		t.Fatalf("%+v", out)
	}
	resolved, err := filepath.EvalSymlinks(taskFile)
	if err != nil {
		t.Fatal(err)
	}
	// The parent is read last, after every other check.
	want := []string{
		"herdr agent list",
		"herdr workspace list",
		"atb linear query { issue(id: \"EX-10\") { team { key } project { name } } } key=set",
		"atb linear claim EX-10 --agent wire-lead --source thread-1 --scope cross-repo: job wire key=set",
		"atb linear create --team QT --project Queried project --parent EX-10 --label lead --title Wire the two repos " +
			"--description-file " + resolved + " --json key=set",
		"atb linear claim EX-12 --agent wire-lead --source thread-1 --scope cross-repo: job wire key=set",
		"herdr workspace create",
		"herdr tab rename",
		"herdr agent start",
		"herdr pane rename",
		"herdr agent read",
		"herdr agent get",
		"herdr agent prompt",
	}
	if got := strings.Split(strings.TrimSpace(w.calls()), "\n"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls = %q, want %q", got, want)
	}
	if !dirExists(crossRepo) {
		t.Errorf("%s was not made", crossRepo)
	}
	wsArgv, _ := os.ReadFile(filepath.Join(w.dir, "workspace-argv"))
	if !strings.Contains(string(wsArgv), "--cwd\n"+crossRepo+"\n") {
		t.Errorf("workspace create argv = %q", wsArgv)
	}
	if got := jobRow(w, "wire"); got != "EX-10  "+" "+crossRepo+" open" {
		t.Errorf("job row = %q", got)
	}
	// A worker of the cross-repo job: its work order goes where the parent
	// says, read again from Linear.
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	if err := os.WriteFile(filepath.Join(w.dir, "wire-workspace"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cwd := dir(w, "wt")
	out = w.run("", []string{"spawn", "a", "--task-file", taskFile, "--cwd", cwd},
		"FLEET_AGENT=wire-lead", "FLEET_ROLE=lead", "FLEET_PARENT=thread-1", "FLEET_SCOPE="+scope,
		"FLEET_JOB=wire", linearKey)
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	want = []string{
		"herdr agent list",
		"herdr workspace list",
		"atb linear query { issue(id: \"EX-10\") { team { key } project { name } } } key=set",
		"atb linear create --team QT --project Queried project --parent EX-10 --label worker --title Wire the two repos " +
			"--description-file " + resolved + " --json key=set",
		"atb linear claim EX-12 --agent wire-a --source wire-lead --scope cross-repo: job wire key=set",
		"herdr tab create",
	}
	if got := strings.Split(strings.TrimSpace(w.calls()), "\n"); strings.Join(got[:6], "\n") != strings.Join(want, "\n") {
		t.Errorf("spawn calls = %q, want %q first", got, want)
	}
	if got := row(w, "wire-a"); got != "EX-12 EX-10 active "+cwd {
		t.Errorf("row = %s", got)
	}
	// A refusal of a cross-repo start never needs Linear: the live lead
	// on the parent is found in the ledger first.
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	out = w.startJob("wire-2", "--parent-issue", "EX-10", "--task-file", taskFile)
	if out.code != 1 || !strings.Contains(out.stderr, "wire-lead is live on parent issue EX-10") {
		t.Errorf("%+v", out)
	}
	if strings.Contains(w.calls(), "atb") {
		t.Errorf("a refused start called atb: %q", w.calls())
	}
}

func TestJobStartOfACrossRepoJobWithoutAParentRunsWithLinearOff(t *testing.T) {
	w := newWorld(t)
	w.useStartHerdr()
	w.fakeAtbCreating("")
	// The lead's directory goes into the checkout of x-repo-general, which
	// fleet does not make.
	out := w.startJob("wire", "--task-file", task(w, "task.md", "Wire\n"))
	if out.code != 1 || !strings.Contains(out.stderr, "no checkout at "+filepath.Join(w.dir, "home", "x-repo", "general")) {
		t.Errorf("no checkout: %+v", out)
	}
	if dirExists(filepath.Join(w.dir, "home", "x-repo")) || w.calls() != "" {
		t.Errorf("a refused start made something: calls %q", w.calls())
	}
	dir(w, filepath.Join("home", "x-repo", "general"))
	out = w.startJob("wire", "--task-file", task(w, "task.md", "Wire\n"))
	if out.code != 0 {
		if strings.Contains(out.stderr, "try again later") {
			t.Skip("the machine is loaded; the resource check refused")
		}
		t.Fatalf("%+v", out)
	}
	if strings.Contains(w.calls(), "atb") {
		t.Errorf("calls = %q", w.calls())
	}
	if got := row(w, "wire-lead"); !strings.HasPrefix(got, "  active ") {
		t.Errorf("row = %q", got)
	}
	// No config in the checkout: the defaults.
	if argv := w.file("argv"); !strings.Contains(argv, "The job's cap is 16 live agents") {
		t.Errorf("argv = %q", argv)
	}
}

func TestACrossRepoJobReadsTheConfigInItsInitiativeCheckout(t *testing.T) {
	w := newWorld(t)
	w.useStartHerdr()
	w.fakeAtbCreating("")
	checkout := dir(w, filepath.Join("home", "x-repo", "general"))
	cfg := filepath.Join(checkout, ".fleet", "config.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(body string) {
		if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"max_agents_per_job": 0}`)
	out := w.startJob("wire", "--task-file", task(w, "task.md", "Wire\n"))
	if out.code != 1 || !strings.Contains(out.stderr, cfg+": max_agents_per_job") {
		t.Errorf("invalid config: %+v", out)
	}
	// Its linear is not used: without a parent issue Linear stays off.
	write(`{"max_agents_per_job": 1, "resource_check": false, "linear": {"team": "EX", "project": "Example project"}}`)
	out = w.startJob("wire", "--task-file", task(w, "task.md", "Wire\n"))
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if strings.Contains(w.calls(), "atb") {
		t.Errorf("calls = %q", w.calls())
	}
	if argv := w.file("argv"); !strings.Contains(argv, "The job's cap is 1 live agents") {
		t.Errorf("argv = %q", argv)
	}
	// spawn reads the same file: the lead alone fills a cap of 1.
	out = w.asAgent("wire-lead", "lead", "", "wire", "spawn", "a", "--cwd", checkout,
		"--task-file", task(w, "a.md", "a\n"))
	if out.code != 1 || !strings.Contains(out.stderr, "the cap is 1") {
		t.Errorf("spawn: %+v", out)
	}
}

func TestJobStartStopsWhenAnAtbStepFails(t *testing.T) {
	for _, step := range []string{"claim", "create"} {
		w := newWorld(t)
		w.configure(withLinear)
		w.useStartHerdr()
		w.fakeAtbCreating(step)
		out := w.startJob("item-2", "--repo", "example-dataset", "--parent-issue", "EX-10",
			"--task-file", task(w, "task.md", "import\n"))
		if out.code != 5 {
			t.Errorf("%s: %+v", step, out)
		}
		failed := "atb linear claim EX-10 failed (exit status: 4)"
		if step == "create" {
			failed = "atb linear create failed (exit status: 4)"
			if !strings.Contains(out.stderr, "parent issue EX-10 claimed by item-2-lead") {
				t.Errorf("create: the claimed parent is not listed: %q", out.stderr)
			}
		}
		if !strings.Contains(out.stderr, failed) {
			t.Errorf("%s: stderr %q does not say %q", step, out.stderr, failed)
		}
		if strings.Contains(out.stderr+out.stdout, "fake0xK3Y") {
			t.Errorf("%s: the key leaked: %+v", step, out)
		}
		if strings.Contains(w.calls(), "herdr workspace create") {
			t.Errorf("%s: the workspace was created: %q", step, w.calls())
		}
		// The reservation stays for `job end --force`, and is listed.
		if got := jobRow(w, "item-2"); got != "EX-10  example-dataset "+filepath.Join(w.dir, "home", "dev", "example-dataset")+" open" {
			t.Errorf("%s: job row = %s", step, got)
		}
		if got := row(w, "item-2-lead"); !strings.HasPrefix(got, " EX-10 starting ") {
			t.Errorf("%s: row = %s", step, got)
		}
		for _, want := range []string{"job item-2 (open)", "ledger row item-2-lead (state starting)", "find out why from the error above first; then clean up with: fleet job end item-2 --force"} {
			if !strings.Contains(out.stderr, want) {
				t.Errorf("%s: %q missing from %q", step, want, out.stderr)
			}
		}
	}
}

// gatedAtb is fakeAtbCreating whose claim of `held` touches <dir>/at-claim
// and then waits until <dir>/go exists, so a test can hold a start between
// its reservation and its Linear steps.
func (w *world) gatedAtb(held string) {
	w.t.Helper()
	w.fakeAtbCreating("")
	path := filepath.Join(w.dir, "fake-herdr", "atb")
	script, err := os.ReadFile(path)
	if err != nil {
		w.t.Fatal(err)
	}
	gate := `if [ "$2" = claim ] && [ "$3" = ` + held + ` ]; then
  : > "$(dirname "$0")/../at-claim"
  while [ ! -e "$(dirname "$0")/../go" ]; do sleep 0.05; done
fi
`
	// The gate goes before the create/query answers, after the call log.
	parts := strings.SplitN(string(script), "if [ \"$2\" = create ]", 2)
	if len(parts) != 2 {
		w.t.Fatal("unexpected fake atb")
	}
	if err := os.WriteFile(path, []byte(parts[0]+gate+"if [ \"$2\" = create ]"+parts[1]), 0o755); err != nil {
		w.t.Fatal(err)
	}
}

func TestJobStartReservesTheLeadSoAConcurrentStartOnTheParentIsRefused(t *testing.T) {
	for _, newParent := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing parent", true: "new parent"}[newParent], func(t *testing.T) {
			concurrentStarts(t, newParent)
		})
	}
}

// concurrentStarts holds a start at its parent claim, with the parent given
// (EX-10) or created by --new-parent (EX-11, bound to the rows as soon as
// it exists), and starts a second job on that parent meanwhile.
func concurrentStarts(t *testing.T, newParent bool) {
	w := newWorld(t)
	w.configure(withLinear)
	w.useStartHerdr()
	parent := "EX-10"
	if newParent {
		parent = "EX-11"
	}
	w.gatedAtb(parent)
	taskFile := task(w, "task.md", "import\n")
	start := func(job string, flags ...string) *exec.Cmd {
		args := append([]string{"job", "start", job, "--repo", "example-dataset", "--task-file", taskFile}, flags...)
		return w.bin(args, "FLEET_AGENT=thread-1", "FLEET_ROLE=thread", "FLEET_SCOPE="+scope, linearKey)
	}
	onParent := []string{"--parent-issue", parent}
	alphaFlags := onParent
	if newParent {
		alphaFlags = []string{"--new-parent", "Import"}
	}
	// alpha reserves its rows and is held at the parent claim.
	alpha := start("alpha", alphaFlags...)
	var alphaOut bytes.Buffer
	alpha.Stdout, alpha.Stderr = &alphaOut, &alphaOut
	if err := alpha.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(w.dir, "at-claim")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("alpha never reached the parent claim")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := jobRow(w, "alpha"); got != parent+"  example-dataset "+filepath.Join(w.dir, "home", "dev", "example-dataset")+" open" {
		t.Errorf("alpha's job row while held = %s", got)
	}
	// beta, on the same parent, while alpha is held: refused by the ledger,
	// naming alpha's lead, without touching Linear.
	beta := start("beta", onParent...)
	var betaOut bytes.Buffer
	beta.Stdout, beta.Stderr = &betaOut, &betaOut
	err := beta.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || !strings.Contains(betaOut.String(), "alpha-lead is live on parent issue "+parent) {
		t.Errorf("beta: err %v, output %q", err, betaOut.String())
	}
	if strings.Contains(w.calls(), "beta") {
		t.Errorf("beta reached atb: %q", w.calls())
	}
	if strings.Contains(betaOut.String(), "parent issue "+parent+" (") {
		t.Errorf("beta created a parent: %q", betaOut.String())
	}
	if got := jobRow(w, "beta"); got != "none" {
		t.Errorf("beta's job row = %s", got)
	}
	// Released, alpha finishes.
	if err := os.WriteFile(filepath.Join(w.dir, "go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := alpha.Wait(); err != nil {
		t.Fatalf("alpha: %v, output %q", err, alphaOut.String())
	}
	if got := row(w, "alpha-lead"); !strings.HasPrefix(got, "EX-12 "+parent+" active ") {
		t.Errorf("alpha's row = %s", got)
	}
	// Control: once alpha's lead has ended, the parent is reused.
	conn := w.ledger()
	for _, stmt := range []string{
		"UPDATE agents SET state = 'ended', ended_at = 1 WHERE name = 'alpha-lead'",
		"UPDATE jobs SET state = 'ended', ended_at = 1 WHERE job = 'alpha'",
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	conn.Close()
	gamma := start("gamma", onParent...)
	var gammaOut bytes.Buffer
	gamma.Stdout, gamma.Stderr = &gammaOut, &gammaOut
	if err := gamma.Run(); err != nil {
		t.Fatalf("gamma: %v, output %q", err, gammaOut.String())
	}
	if got := row(w, "gamma-lead"); !strings.HasPrefix(got, "EX-12 "+parent+" active ") {
		t.Errorf("gamma's row = %s", got)
	}
}

func TestJobStartKeepsTheIdentifiersWrittenBeforeAnAtbStepFailed(t *testing.T) {
	checkout := filepath.Join("home", "dev", "example-dataset")
	for _, c := range []struct {
		failOn, jobParent, leadRow string
		listed                     []string
	}{
		// The parent is created and bound, then its claim fails.
		{"claim EX-11", "EX-11", " EX-11 starting ", []string{"parent issue EX-11 (https://linear.example.test/EX-11)"}},
		// The parent is claimed and the work order created and bound, then
		// its claim fails.
		{"claim EX-12", "EX-11", "EX-12 EX-11 starting ", []string{"parent issue EX-11 claimed by item-2-lead",
			"work order EX-12 (https://linear.example.test/EX-12), not claimed"}},
	} {
		w := newWorld(t)
		w.configure(withLinear)
		w.useStartHerdr()
		w.fakeAtbCreating(c.failOn)
		out := w.startJob("item-2", "--repo", "example-dataset", "--new-parent", "Import",
			"--task-file", task(w, "task.md", "import\n"))
		if out.code != 5 || !strings.Contains(out.stderr, "atb linear "+c.failOn+" failed") {
			t.Errorf("%s: %+v", c.failOn, out)
		}
		if got := jobRow(w, "item-2"); got != c.jobParent+"  example-dataset "+filepath.Join(w.dir, checkout)+" open" {
			t.Errorf("%s: job row = %s", c.failOn, got)
		}
		if got := row(w, "item-2-lead"); !strings.HasPrefix(got, c.leadRow) {
			t.Errorf("%s: row = %q, want prefix %q", c.failOn, got, c.leadRow)
		}
		for _, want := range append(c.listed, "find out why from the error above first; then clean up with: fleet job end item-2 --force") {
			if !strings.Contains(out.stderr, want) {
				t.Errorf("%s: %q missing from %q", c.failOn, want, out.stderr)
			}
		}
		if strings.Contains(w.calls(), "herdr workspace create") {
			t.Errorf("%s: the workspace was created", c.failOn)
		}
	}
}

func TestJobStartChecksTheParentFlagsAgainstTheConfig(t *testing.T) {
	cases := []struct {
		name, config string
		args         []string
		says         string
	}{
		{"neither parent flag with Linear on", withLinear, nil, "--parent-issue or --new-parent is required"},
		{"a title-less task", withLinear, []string{"--parent-issue", "EX-10", "--task-file", "HASHES"}, "gives no title"},
		{"an invalid config", `{"linear": {"team": "EX"}}`, []string{"--parent-issue", "EX-10"}, "config.json: linear.project"},
		{"a parent flag with Linear off", `{}`, []string{"--parent-issue", "EX-10"}, "sets no linear"},
	}
	for _, c := range cases {
		w := newWorld(t)
		w.configure(c.config)
		w.fakeAtbCreating("")
		args := append([]string{"item-2", "--repo", "example-dataset"}, c.args...)
		if len(args) > 0 && args[len(args)-1] == "HASHES" {
			args[len(args)-1] = task(w, "hashes.md", "#\n##\n")
		} else {
			args = append(args, "--task-file", task(w, "task.md", "import\n"))
		}
		out := w.startJob(args...)
		if out.code != 1 || !strings.Contains(out.stderr, c.says) {
			t.Errorf("%s: %+v, want exit 1 saying %q", c.name, out, c.says)
		}
		if w.calls() != "" {
			t.Errorf("%s: calls = %q", c.name, w.calls())
		}
	}
}

func TestJobListPrintsTheOpenJobs(t *testing.T) {
	w := newWorld(t)
	conn := w.ledger()
	for _, stmt := range []string{
		"INSERT INTO jobs (job, parent_issue, key, repo, lead_cwd, state, started_at) VALUES " +
			"('item-1', 'EX-10', 'PR-7', 'example-dataset', '/d/example-dataset', 'open', 10), " +
			"('wire', 'EX-20', '', '', '/c/wire', 'open', 20), " +
			"('old', 'EX-5', '', 'example-dataset', '/d/example-dataset', 'ended', 5)",
		"INSERT INTO agents (name, role, job, parent, state, started_at) VALUES " +
			"('item-1-lead', 'lead', 'item-1', 'thread-1', 'active', 10), " +
			"('item-1-a', 'worker', 'item-1', 'item-1-lead', 'active', 11), " +
			"('item-1-b', 'worker', 'item-1', 'item-1-lead', 'ended', 12), " +
			"('item-1-c', 'worker', 'item-1', 'item-1-lead', 'starting', 13), " +
			"('wire-lead', 'lead', 'wire', 'thread-1', 'ended', 20)",
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	conn.Close()
	out := w.run("", []string{"job", "list", "--scope", scope})
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	want := "item-1\tEX-10\tPR-7\texample-dataset\titem-1-lead\titem-1-a,item-1-c\n" +
		"wire\tEX-20\t-\t-\t-\t-\n"
	if out.stdout != want {
		t.Errorf("stdout = %q, want %q", out.stdout, want)
	}
	out = w.run("", []string{"job", "list", "--json"}, "FLEET_SCOPE="+scope)
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	var lines []map[string]any
	if err := json.Unmarshal([]byte(out.stdout), &lines); err != nil {
		t.Fatalf("%v in %q", err, out.stdout)
	}
	if len(lines) != 2 || lines[0]["lead"] != "item-1-lead" || lines[1]["lead"] != nil ||
		lines[1]["lead_cwd"] != "/c/wire" || lines[0]["started_at"] != float64(10) {
		t.Errorf("json = %v", lines)
	}
	if workers, _ := json.Marshal(lines[1]["workers"]); string(workers) != "[]" {
		t.Errorf("wire workers = %s", workers)
	}
	// An empty ledger prints nothing.
	conn = w.ledger()
	if _, err := conn.Exec("UPDATE jobs SET state = 'ended'"); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	out = w.run("", []string{"job", "list", "--scope", scope})
	if out.code != 0 || out.stdout != "" {
		t.Errorf("%+v", out)
	}
	out = w.run("", []string{"job", "list", "--scope", "no-such-scope"})
	if out.code != 5 || !strings.Contains(out.stderr, "no ledger at") {
		t.Errorf("%+v", out)
	}
}

func TestJobListShowsTheJobsOfTheCallersChannelUnlessAll(t *testing.T) {
	w := newWorld(t)
	conn := w.ledger()
	for _, stmt := range []string{
		"INSERT INTO threads (thread, slug, mapping, cwd, created_at) VALUES " +
			"('C1/1.1', 'c1-1-1', 'repo-example-dataset', '/d', 0), ('C2/2.2', 'c2-2-2', 'x-repo-example-init', '/x', 0)",
		"INSERT INTO jobs (job, lead_cwd, home_thread, state, started_at) VALUES " +
			"('in-repo', '/a', 'C1/1.1', 'open', 1), ('in-x-repo', '/b', 'C2/2.2', 'open', 2), ('homeless', '/c', '', 'open', 3)",
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	conn.Close()
	jobs := func(out result) string {
		var names []string
		for _, line := range strings.Split(strings.TrimSpace(out.stdout), "\n") {
			name, _, _ := strings.Cut(line, "\t")
			names = append(names, name)
		}
		return strings.Join(names, " ")
	}
	for _, c := range []struct {
		label string
		args  []string
		env   []string
		want  string
	}{
		{"a thread agent of the repo", nil, []string{"FLEET_THREAD=C1/1.1"}, "in-repo"},
		{"a lead whose job's home thread is the initiative's", nil, []string{"FLEET_JOB=in-x-repo"}, "in-x-repo"},
		{"--all", []string{"--all"}, []string{"FLEET_THREAD=C1/1.1"}, "in-repo in-x-repo homeless"},
		{"a plain shell", nil, nil, "in-repo in-x-repo homeless"},
	} {
		out := w.run("", append([]string{"job", "list"}, c.args...), append([]string{"FLEET_SCOPE=" + scope}, c.env...)...)
		if out.code != 0 || jobs(out) != c.want {
			t.Errorf("%s: %+v, want %q", c.label, out, c.want)
		}
	}
}

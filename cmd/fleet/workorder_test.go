// Integration tests for spawn's settings and work order: `.fleet/config.json`
// in the main checkout, a fake atb and a fake herdr that log every call to
// <dir>/calls in order. Hermetic, see main_test.go.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Luolc/agent-fleet/internal/db"
)

// spawnHerdr is a fake herdr that lets a worker spawn run to the end: one
// workspace labelled item-1, an agent that starts at its input box, and a
// prompt that is delivered. `tab create` and `agent prompt` keep their argv
// in <dir>/tab-argv and <dir>/argv.
const spawnHerdr = `#!/bin/sh
dir="$(dirname "$0")/.."
echo "herdr $1 $2" >> "$dir/calls"
case "$1 $2" in
  "agent list") echo '{"result":{"agents":[]}}' ;;
  "workspace list") echo '{"result":{"workspaces":[{"workspace_id":"w1","label":"item-1"}]}}' ;;
  "tab create")
    printf '%s\n' "$@" > "$dir/tab-argv"
    echo '{"result":{"root_pane":{"workspace_id":"w1","tab_id":"t1","pane_id":"p1"}}}' ;;
  "agent start"|"pane rename") echo '{"result":{}}' ;;
  "agent read") printf '%s\n' "────────────" "❯ " "────────────" ;;
  "agent get") echo '{"result":{"agent":{"agent_status":"idle"}}}' ;;
  "agent prompt")
    printf '%s\n' "$@" > "$dir/argv"
    echo '{"id":"cli:agent:prompt","result":{"type":"agent_prompted"}}' ;;
  *) echo "fake herdr: unexpected command: $*" >&2; exit 2 ;;
esac
`

// configure writes the repo's .fleet/config.json under the world's ~/dev.
func (w *world) configure(body string) string {
	w.t.Helper()
	dir := filepath.Join(w.dir, "home", "dev", "example-dataset", ".fleet")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		w.t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		w.t.Fatal(err)
	}
	return path
}

const withLinear = `{"linear": {"team": "EX", "project": "Example project"}}`

// fakeAtbCreating is fakeAtb whose `create` prints the JSON of EX-12.
func (w *world) fakeAtbCreating(failOn string) {
	w.t.Helper()
	w.fakeAtb(failOn)
	path := filepath.Join(w.dir, "fake-herdr", "atb")
	script, err := os.ReadFile(path)
	if err != nil {
		w.t.Fatal(err)
	}
	script = append(script, []byte(`[ "$2" = create ] && echo '{"identifier":"EX-12","url":"https://linear.example.test/EX-12"}'
exit 0
`)...)
	if err := os.WriteFile(path, script, 0o755); err != nil {
		w.t.Fatal(err)
	}
}

// liveLead puts item-1-lead in the ledger as live, with a worktree and
// the given parent issue.
func liveLead(w *world, parentIssue string) {
	w.t.Helper()
	path, err := db.PathUnder(filepath.Join(w.dir, "home"), repo)
	if err != nil {
		w.t.Fatal(err)
	}
	conn, err := db.OpenAt(path)
	if err != nil {
		w.t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Exec(
		"INSERT INTO agents (name, role, job, parent, state, started_at, worktree, parent_issue) "+
			"VALUES ('item-1-lead', 'lead', 'item-1', 'orchestra', 'active', 0, ?1, ?2)",
		filepath.Join(w.dir, "wt"), parentIssue); err != nil {
		w.t.Fatal(err)
	}
}

// row is the issue, parent issue and state of the newest row named name,
// or "none".
func row(w *world, name string) string {
	w.t.Helper()
	path, err := db.PathUnder(filepath.Join(w.dir, "home"), repo)
	if err != nil {
		w.t.Fatal(err)
	}
	conn, err := db.OpenAt(path)
	if err != nil {
		w.t.Fatal(err)
	}
	defer conn.Close()
	var issue, parentIssue, state string
	if err := conn.QueryRow("SELECT issue, parent_issue, state FROM agents WHERE name = ?1 ORDER BY id DESC LIMIT 1",
		name).Scan(&issue, &parentIssue, &state); err != nil {
		return "none"
	}
	return issue + " " + parentIssue + " " + state
}

func (w *world) spawnWorker(args ...string) result {
	w.t.Helper()
	return w.run("", append([]string{"spawn", "a"}, args...),
		"FLEET_AGENT=item-1-lead", "FLEET_ROLE=lead", "FLEET_PARENT=orchestra", "FLEET_REPO="+repo,
		"FLEET_JOB=item-1", linearKey)
}

func TestSpawnCreatesAndClaimsTheWorkOrderBeforeAnythingElse(t *testing.T) {
	w := newWorld(t)
	w.configure(`{"resource_check": false, "linear": {"team": "EX", "project": "Example project"}}`)
	liveLead(w, "EX-10")
	if err := os.WriteFile(filepath.Join(w.dir, "fake-herdr", "herdr"), []byte(spawnHerdr), 0o755); err != nil {
		t.Fatal(err)
	}
	w.fakeAtbCreating("")
	taskFile := task(w, "task.md", "\n# Import the A table\nall rows\n")
	out := w.spawnWorker("--task-file", taskFile)
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
		"atb linear create --team EX --project Example project --parent EX-10 --title Import the A table " +
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
	if !strings.Contains(string(tabArgv), "--env\nFLEET_ISSUE=EX-12\n") {
		t.Errorf("tab create argv = %q", tabArgv)
	}
	argv, _ := os.ReadFile(filepath.Join(w.dir, "argv"))
	if want := "[FROM: item-1-lead]\nWork order: https://linear.example.test/EX-12\n\n\n# Import the A table\n"; !strings.Contains(string(argv), want) {
		t.Errorf("delivered argv = %q, want %q in it", argv, want)
	}
	if got := row(w, "item-1-a"); got != "EX-12 EX-10 active" {
		t.Errorf("row = %s", got)
	}
}

func TestSpawnStopsWhenAnAtbStepFails(t *testing.T) {
	for _, step := range []string{"create", "claim"} {
		w := newWorld(t)
		w.configure(withLinear)
		liveLead(w, "EX-10")
		if err := os.WriteFile(filepath.Join(w.dir, "fake-herdr", "herdr"), []byte(spawnHerdr), 0o755); err != nil {
			t.Fatal(err)
		}
		w.fakeAtbCreating(step)
		out := w.spawnWorker("--task-file", task(w, "task.md", "import\n"))
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
		if got := row(w, "item-1-a"); got != "none" {
			t.Errorf("%s: row = %s", step, got)
		}
	}
}

func TestSpawnChecksTheConfigAndTheParentIssueBeforeAnythingIsCreated(t *testing.T) {
	cases := []struct {
		name, config, lead string
		agent, role, job   string
		args               []string
		says               string
	}{
		{"lead without --parent-issue", withLinear, "", "orchestra", "orchestra", "", []string{"item-2"}, "--parent-issue is required"},
		{"--parent-issue without linear", `{}`, "", "orchestra", "orchestra", "", []string{"item-2", "--parent-issue", "EX-10"}, "sets no linear"},
		{"worker with --parent-issue", withLinear, "EX-10", "item-1-lead", "lead", "item-1", []string{"a", "--parent-issue", "EX-10"}, "leads only"},
		{"lead row without a parent issue", withLinear, "", "item-1-lead", "lead", "item-1", []string{"a"}, "no parent issue in the ledger"},
		{"a title-less task", withLinear, "EX-10", "item-1-lead", "lead", "item-1", []string{"a", "--task-file", "HASHES"}, "gives no title"},
		{"an invalid config", `{"linear": {"team": "EX"}}`, "EX-10", "item-1-lead", "lead", "item-1", []string{"a"}, "config.json: linear.project"},
		{"a null linear", `{"linear": null}`, "", "orchestra", "orchestra", "", []string{"item-2"}, "linear: must not be null"},
		{"the job cap", `{"max_agents_per_job": 1}`, "EX-10", "item-1-lead", "lead", "item-1", []string{"a"}, "the cap is 1"},
	}
	for _, c := range cases {
		w := newWorld(t)
		w.configure(c.config)
		liveLead(w, c.lead)
		w.fakeAtbCreating("")
		args := append([]string{"spawn"}, c.args...)
		if args[len(args)-1] == "HASHES" {
			args[len(args)-1] = task(w, "hashes.md", "#\n##\n")
		} else {
			args = append(args, "--task-file", task(w, "task.md", "import\n"))
		}
		out := w.asAgent(c.agent, c.role, "", c.job, args...)
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
	liveLead(w, "")
	if err := os.WriteFile(filepath.Join(w.dir, "fake-herdr", "herdr"), []byte(spawnHerdr), 0o755); err != nil {
		t.Fatal(err)
	}
	w.fakeAtbCreating("")
	out := w.run("", []string{"spawn", "a", "--task-file", task(w, "task.md", "# Import\n")},
		"FLEET_AGENT=item-1-lead", "FLEET_ROLE=lead", "FLEET_PARENT=orchestra", "FLEET_REPO="+repo, "FLEET_JOB=item-1")
	// The machine's load decides between 0 and a resource refusal; either
	// way atb is never called.
	if out.code != 0 && !strings.Contains(out.stderr, "try again later") {
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

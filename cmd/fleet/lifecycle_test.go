// Integration tests for `spawn`, `done` and `close` over the binary: the
// refusals that happen before anything is created, and `done` against the
// fake herdr. Hermetic, see main_test.go. The full spawn → done → close
// chain runs against the real herdr in the judge.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Luolc/agent-fleet/internal/db"
)

// asAgent runs the binary with the given caller identity.
func (w *world) asAgent(agent, role, parent, job string, args ...string) result {
	w.t.Helper()
	return w.run("", args,
		"FLEET_AGENT="+agent, "FLEET_ROLE="+role, "FLEET_PARENT="+parent, "FLEET_REPO="+repo, "FLEET_JOB="+job)
}

// ledgerWith is the world's ledger with the given live rows, each with a
// worktree.
func ledgerWith(w *world, rows []struct{ name, role, job string }) {
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
	for _, r := range rows {
		if _, err := conn.Exec(
			"INSERT INTO agents (name, role, job, parent, state, started_at, worktree) VALUES (?1, ?2, ?3, '', 'active', 0, '/w/item-1')",
			r.name, r.role, r.job); err != nil {
			w.t.Fatal(err)
		}
	}
}

// state is the state of the newest row named `name`.
func state(w *world, name string) string {
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
	var got string
	if err := conn.QueryRow(
		"SELECT state FROM agents WHERE name = ?1 ORDER BY id DESC LIMIT 1", name).Scan(&got); err != nil {
		w.t.Fatal(err)
	}
	return got
}

func task(w *world, name, body string) string {
	w.t.Helper()
	path := filepath.Join(w.dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
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
	taskFile := task(w, "task.md", "build item 1\n")
	ledgerWith(w, []struct{ name, role, job string }{
		{"item-1-lead", "lead", "item-1"},
		{"item-1-a", "worker", "item-1"},
		{"item-1-b", "worker", "item-1"},
		{"item-1-c", "worker", "item-1"},
	})
	// The fake herdr's `agent list` answers with no JSON here, so a spawn
	// that reached herdr would exit 5, not 1.
	cases := []struct {
		agent, role, job string
		args             []string
		says             string
	}{
		{"item-1-a", "worker", "item-1", []string{"x"}, "cannot spawn"},
		{"human-interface", "human-interface", "", []string{"x"}, "cannot spawn"},
		{"orchestra", "orchestra", "", []string{"Item_2"}, "[a-z0-9-]"},
		{"orchestra", "orchestra", "", []string{"cron"}, "reserved"},
		{"orchestra", "orchestra", "", []string{"item-1"}, "already live"},
		{"item-1-lead", "lead", "item-1", []string{"d", "--branch", "x"}, "leads only"},
		{"item-1-lead", "lead", "item-1", []string{"d"}, "cap is 4"},
		{"orchestra", "orchestra", "", []string{"1"}, "start with a letter"},
		{"orchestra", "orchestra", "", []string{longJob}, "at most 32"},
		{"item-1-lead", "lead", "item-1", []string{longWorker}, "at most 32"},
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
	path, err := db.PathUnder(filepath.Join(w.dir, "home"), repo)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var rows int
	if err := conn.QueryRow("SELECT count(*) FROM agents").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 4 {
		t.Errorf("a refused spawn wrote a row: %d rows", rows)
	}
}

func TestSpawnRefusesAnUnreadableOrEmptyTaskFile(t *testing.T) {
	w := newWorld(t)
	empty := task(w, "empty.md", "")
	for _, file := range []string{filepath.Join(w.dir, "missing.md"), empty} {
		out := w.asAgent("orchestra", "orchestra", "", "", "spawn", "item-2", "--task-file", file)
		if out.code != 1 {
			t.Errorf("%s: %+v", file, out)
		}
	}
}

func TestSpawnRequiresTheTaskFileFlag(t *testing.T) {
	w := newWorld(t)
	out := w.run("", []string{"spawn", "item-2"})
	if out.code != 1 || !strings.Contains(out.stderr, "--task-file <PATH>") {
		t.Errorf("%+v", out)
	}
}

func TestDoneReportsToTheParentAndEndsTheRow(t *testing.T) {
	w := newWorld(t)
	ledgerWith(w, []struct{ name, role, job string }{{"item-1-a", "worker", "item-1"}})
	resultFile := task(w, "result.md", "ok\n")
	w.herdr(`{"id":"cli:agent:prompt","result":{"type":"agent_prompted"}}`, "", false)
	out := w.asAgent("item-1-a", "worker", "item-1-lead", "item-1", "done", "--result-file", resultFile)
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	resolved, err := filepath.EvalSymlinks(resultFile)
	if err != nil {
		t.Fatal(err)
	}
	argv, _ := os.ReadFile(filepath.Join(w.dir, "argv"))
	expected := "agent\nprompt\nitem-1-lead\n[FROM: item-1-a]\nitem-1-a is done. Result: " + resolved +
		"\n\n--wait\n--until\nworking\n--timeout\n20000\n"
	if string(argv) != expected {
		t.Errorf("argv = %q, want %q", argv, expected)
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

func TestDoneRefusesWithoutAParent(t *testing.T) {
	w := newWorld(t)
	out := w.asAgent("orchestra", "orchestra", "", "", "done")
	if out.code != 1 || !strings.Contains(out.stderr, "FLEET_PARENT") {
		t.Errorf("%+v", out)
	}
	if _, err := os.Stat(filepath.Join(w.dir, "argv")); err == nil {
		t.Error("herdr was prompted")
	}
}

func TestCloseRefusesLiveRowsWithoutForceAndNonOrchestraCallers(t *testing.T) {
	w := newWorld(t)
	ledgerWith(w, []struct{ name, role, job string }{{"item-1-lead", "lead", "item-1"}})
	out := w.asAgent("orchestra", "orchestra", "", "", "close", "item-1")
	if out.code != 1 || !strings.Contains(out.stderr, "item-1-lead") {
		t.Errorf("%+v", out)
	}
	out = w.asAgent("item-1-lead", "lead", "orchestra", "item-1", "close", "item-1", "--force")
	if out.code != 1 {
		t.Errorf("%+v", out)
	}
	if got := state(w, "item-1-lead"); got != "active" {
		t.Errorf("state = %s", got)
	}
}

// fakeAtb puts a stand-in for atb on PATH. Each call appends its argv, one
// line, to <dir>/calls, with `key=set` when LINEAR_API_KEY reached it. The
// subcommand named by failOn (`comment` or `release`) prints the key it
// got to stdout and stderr and exits 4.
func (w *world) fakeAtb(failOn string) {
	w.t.Helper()
	script := `#!/bin/sh
key=unset
[ -n "$LINEAR_API_KEY" ] && key=set
echo "atb $* key=$key" >> "$(dirname "$0")/../calls"
if [ "$2" = "` + failOn + `" ]; then
  echo "error: refused: no holder, key $LINEAR_API_KEY" >&2
  echo "key $LINEAR_API_KEY"
  exit 4
fi
`
	if err := os.WriteFile(filepath.Join(w.dir, "fake-herdr", "atb"), []byte(script), 0o755); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) calls() string {
	w.t.Helper()
	data, _ := os.ReadFile(filepath.Join(w.dir, "calls"))
	return string(data)
}

// The key is a made-up value; it must reach atb and nothing else.
const linearKey = "LINEAR_API_KEY=lin_api_fake0xK3Y"

func (w *world) doneWithReport(args ...string) result {
	w.t.Helper()
	return w.run("", append([]string{"done"}, args...),
		"FLEET_AGENT=item-1-a", "FLEET_ROLE=worker", "FLEET_PARENT=item-1-lead", "FLEET_REPO="+repo,
		"FLEET_JOB=item-1", linearKey)
}

func TestDoneWritesTheReportAndReleasesBeforeDelivering(t *testing.T) {
	for _, abandon := range []bool{false, true} {
		w := newWorld(t)
		ledgerWith(w, []struct{ name, role, job string }{{"item-1-a", "worker", "item-1"}})
		report := task(w, "report.md", "what I did\n")
		w.herdr(`{"id":"cli:agent:prompt","result":{"type":"agent_prompted"}}`, "", false)
		w.fakeAtb("")
		args := []string{"--report-file", report, "--issue", "EX-7"}
		release, body := "--done", "item-1-a is done. Report: EX-7.\n"
		if abandon {
			args = append(args, "--abandon")
			release, body = "--abandon", "item-1-a abandoned the task. Report: EX-7.\n"
		}
		out := w.doneWithReport(args...)
		if out.code != 0 {
			t.Fatalf("abandon=%v: %+v", abandon, out)
		}
		expected := "atb linear comment EX-7 --body-file " + report + " key=set\n" +
			"atb linear release EX-7 --agent item-1-a --reason done " + release + " key=set\n" +
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
		out := w.doneWithReport("--report-file", report, "--issue", "EX-7")
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
	for _, args := range [][]string{
		{"--report-file", report},
		{"--issue", "EX-7"},
		{"--abandon"},
		{"--report-file", filepath.Join(w.dir, "missing.md"), "--issue", "EX-7"},
	} {
		if out := w.doneWithReport(args...); out.code != 1 {
			t.Errorf("%v: %+v", args, out)
		}
	}
	if w.calls() != "" {
		t.Errorf("calls = %q", w.calls())
	}
	if got := state(w, "item-1-a"); got != "active" {
		t.Errorf("state = %s", got)
	}
}

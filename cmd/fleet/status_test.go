// `fleet status` against the fake herdr, with ledger rows inserted
// directly. Hermetic, see main_test.go.
package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Luolc/agent-fleet/internal/db"
)

const target = "example-dataset"

// statusReport is the shape of `status --json`.
type statusReport struct {
	Jobs   []map[string]any `json:"jobs"`
	Agents []map[string]any `json:"agents"`
}

// statusWorld is a ledger with two open jobs, five live rows and one
// ended, and a herdr that knows four of them.
func statusWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	path, err := db.PathUnder(filepath.Join(w.dir, "home", ".local", "state"), target)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	started := db.Now() - 600
	for _, job := range []string{"x", "y"} {
		if _, err := conn.Exec(
			"INSERT INTO jobs (job, parent_issue, repo, lead_cwd, state, started_at) VALUES (?1, 'EX-1', 'example-dataset', '/c', 'open', ?2)",
			job, started); err != nil {
			t.Fatal(err)
		}
	}
	rows := []struct{ name, role, job, parent, state string }{
		{"thread-1", "thread", "", "", "active"},
		{"x-lead", "lead", "x", "thread-1", "active"},
		{"x-w1", "worker", "x", "x-lead", "active"},
		{"x-w2", "worker", "x", "x-lead", "active"},
		{"y-lead", "lead", "y", "thread-1", "active"},
		{"z-lead", "lead", "z", "thread-1", "ended"},
	}
	for _, r := range rows {
		if _, err := conn.Exec(
			"INSERT INTO agents (name, role, job, parent, state, started_at) VALUES (?1, ?2, ?3, ?4, ?5, ?6)",
			r.name, r.role, r.job, r.parent, r.state, started); err != nil {
			t.Fatal(err)
		}
	}
	agents := []string{}
	for _, a := range []struct {
		name, status string
		seq          int
	}{{"thread-1", "idle", 1}, {"x-lead", "idle", 2}, {"x-w1", "blocked", 3}, {"y-lead", "working", 4}} {
		agents = append(agents, fmt.Sprintf(
			`{"agent":"claude","name":"%s","agent_status":"%s","state_change_seq":%d}`, a.name, a.status, a.seq))
	}
	w.herdr("unused", `{"id":"cli:agent:list","result":{"agents":[`+strings.Join(agents, ",")+`]}}`, false)
	return w
}

func TestStatusFlagsIdleDebtorsBlockedAndMissingAgents(t *testing.T) {
	w := statusWorld(t)
	out := w.run("", []string{"status", "--target", target, "--json"})
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	var report statusReport
	if err := json.Unmarshal([]byte(out.stdout), &report); err != nil {
		t.Fatalf("%v in %q", err, out.stdout)
	}
	lines := report.Agents
	if len(report.Jobs) != 2 || report.Jobs[0]["job"] != "x" || report.Jobs[1]["job"] != "y" {
		t.Errorf("jobs = %v", report.Jobs)
	}
	workers, _ := json.Marshal(report.Jobs[0]["workers"])
	if report.Jobs[0]["lead"] != "x-lead" || string(workers) != `["x-w1","x-w2"]` {
		t.Errorf("job x = %v", report.Jobs[0])
	}
	find := func(name string) map[string]any {
		for _, l := range lines {
			if l["name"] == name {
				return l
			}
		}
		return nil
	}
	flags := func(name string) string {
		l := find(name)
		if l == nil {
			return "<absent>"
		}
		raw, _ := json.Marshal(l["flags"])
		return string(raw)
	}
	// The idle thread agent owes no work; the ended row is not shown.
	for name, want := range map[string]string{
		"thread-1": "[]", "x-lead": `["owes-work"]`, "x-w1": `["blocked"]`,
		"x-w2": `["missing"]`, "y-lead": "[]", "z-lead": "<absent>",
	} {
		if got := flags(name); got != want {
			t.Errorf("flags(%s) = %s, want %s", name, got, want)
		}
	}
	xw1 := find("x-w1")
	if xw1["parent"] != "x-lead" || xw1["herdr_status"] != "blocked" || xw1["since_change_secs"] != nil {
		t.Errorf("x-w1 = %v", xw1)
	}
	if !strings.Contains(out.stdout, "\"since_change_secs\": null") {
		t.Errorf("since_change_secs is not null: %s", out.stdout)
	}
}

func TestStatusFiltersByJobAndPrintsATable(t *testing.T) {
	w := statusWorld(t)
	out := w.run("", []string{"status", "--job", "x"}, "FLEET_TARGET="+target)
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	text := out.stdout
	if !strings.HasPrefix(text, "JOB ") || !strings.Contains(text, "\nNAME ") {
		t.Errorf("%s", text)
	}
	for _, name := range []string{"x-lead", "x-w1", "x-w2"} {
		if !strings.Contains(text, name) {
			t.Errorf("%s missing from:\n%s", name, text)
		}
	}
	if strings.Contains(text, "y-lead") || strings.Contains(text, "\ny ") {
		t.Errorf("%s", text)
	}
	if !strings.Contains(text, "owes-work") || !strings.Contains(text, "missing") {
		t.Errorf("%s", text)
	}
}

func TestStatusReadsTheDefaultTargetAndNeedsAnExistingLedger(t *testing.T) {
	w := statusWorld(t)
	// No FLEET_TARGET and no --target: the default target, whose ledger
	// does not exist here.
	out := w.run("", []string{"status"})
	if out.code != 5 || !strings.Contains(out.stderr, "/fleet/default/fleet.db") {
		t.Errorf("%+v", out)
	}
	out = w.run("", []string{"status", "--target", "no-such-target"})
	if out.code != 5 || !strings.Contains(out.stderr, "no ledger at") {
		t.Errorf("%+v", out)
	}
	for _, bad := range []string{"a/b", "..", "."} {
		out = w.run("", []string{"status", "--target", bad})
		if out.code != 1 || !strings.Contains(out.stderr, "directory name") {
			t.Errorf("--target %q: %+v", bad, out)
		}
	}
	out = w.run("", []string{"status", "--json"}, "FLEET_TARGET=")
	if out.code != 5 || !strings.Contains(out.stderr, "/fleet/default/fleet.db") {
		t.Errorf("empty FLEET_TARGET: %+v", out)
	}
}

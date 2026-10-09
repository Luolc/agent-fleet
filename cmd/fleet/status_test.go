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

const repo = "acme/example-dataset"

// statusWorld is a ledger with five live rows and one ended, and a herdr
// that knows four of them.
func statusWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	path, err := db.PathUnder(filepath.Join(w.dir, "home"), repo)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	started := db.Now() - 600
	rows := []struct{ name, role, job, parent, state string }{
		{"orchestra", "orchestra", "", "", "active"},
		{"x-lead", "lead", "x", "orchestra", "active"},
		{"x-w1", "worker", "x", "x-lead", "active"},
		{"x-w2", "worker", "x", "x-lead", "active"},
		{"y-lead", "lead", "y", "orchestra", "active"},
		{"z-lead", "lead", "z", "orchestra", "ended"},
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
	}{{"orchestra", "idle", 1}, {"x-lead", "idle", 2}, {"x-w1", "blocked", 3}, {"y-lead", "working", 4}} {
		agents = append(agents, fmt.Sprintf(
			`{"agent":"claude","name":"%s","agent_status":"%s","state_change_seq":%d}`, a.name, a.status, a.seq))
	}
	w.herdr("unused", `{"id":"cli:agent:list","result":{"agents":[`+strings.Join(agents, ",")+`]}}`, false)
	return w
}

func TestStatusFlagsIdleDebtorsBlockedAndMissingAgents(t *testing.T) {
	w := statusWorld(t)
	out := w.run("", []string{"status", "--repo", repo, "--json"})
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	var lines []map[string]any
	if err := json.Unmarshal([]byte(out.stdout), &lines); err != nil {
		t.Fatalf("%v in %q", err, out.stdout)
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
	// The idle orchestra owes no work; the ended row is not shown.
	for name, want := range map[string]string{
		"orchestra": "[]", "x-lead": `["owes-work"]`, "x-w1": `["blocked"]`,
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
	out := w.run("", []string{"status", "--job", "x"}, "FLEET_REPO="+repo)
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	text := out.stdout
	if !strings.HasPrefix(text, "NAME") {
		t.Errorf("%s", text)
	}
	for _, name := range []string{"x-lead", "x-w1", "x-w2"} {
		if !strings.Contains(text, name) {
			t.Errorf("%s missing from:\n%s", name, text)
		}
	}
	if strings.Contains(text, "y-lead") {
		t.Errorf("%s", text)
	}
	if !strings.Contains(text, "owes-work") || !strings.Contains(text, "missing") {
		t.Errorf("%s", text)
	}
}

func TestStatusNeedsARepoAndAnExistingLedger(t *testing.T) {
	w := statusWorld(t)
	out := w.run("", []string{"status"})
	if out.code != 1 {
		t.Errorf("%+v", out)
	}
	out = w.run("", []string{"status", "--repo", "acme/no-such-dataset"})
	if out.code != 5 || !strings.Contains(out.stderr, "no ledger at") {
		t.Errorf("%+v", out)
	}
}

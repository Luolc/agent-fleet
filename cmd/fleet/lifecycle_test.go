// Integration tests for `spawn` over the binary: the refusals that happen
// before anything is created. Hermetic, see main_test.go. The full spawn
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

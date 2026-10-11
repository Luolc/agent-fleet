package cmd

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Luolc/agent-fleet/internal/atb"
	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/identity"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/prompts")

// TestBuiltInPromptsMatchTheirGoldenFiles renders the first messages of
// every role from fixed contexts and compares them byte for byte with
// testdata/prompts: a change to a prompt or to how it is filled shows up as
// a diff of the golden files. The contexts are the fewest that take every
// branch of the filling. `go test ./internal/cmd -run Golden -update`
// rewrites the files.
func TestBuiltInPromptsMatchTheirGoldenFiles(t *testing.T) {
	sc, err := config.ParseScope([]byte(`{}`), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	withSocket, err := config.ParseScope([]byte(`{"fednet": {"socket": "/run/example/fednet.sock"}}`), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	lead := &identity.Identity{Agent: "wire-lead", Role: identity.Lead, Parent: "thread-x", Scope: "main", Job: "wire"}
	worker := &identity.Identity{Agent: "wire-a", Role: identity.Worker, Parent: "wire-lead", Scope: "main", Job: "wire"}
	issue := atb.Issue{Identifier: "QT-12", URL: "https://linear.example.test/QT-12"}

	thread := func(cfg *config.Scope, mapping string, ticket atb.Issue, context string, notes []string) string {
		s := &threadStart{cfg: cfg, mapping: mapping, cwd: MappingDir(cfg, mapping), ticket: ticket, notes: notes,
			id: &identity.Identity{Agent: "thread-c1-1-0", Role: identity.Thread, Scope: "main", Thread: "C1/1.0"},
			msg: inboundMessage{Thread: "C1/1.0", Text: "import the A table", User: "U0ABC", TS: "1700000000.000",
				Context: context}}
		return s.prompt()
	}

	conn, err := db.OpenAt(filepath.Join(t.TempDir(), "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Exec(`INSERT INTO jobs (job, parent_issue, lead_cwd, home_thread, state, outcome, started_at, ended_at)
		VALUES ('a', 'EX-1', '/c', 'C1/1.0', 'ended', 'done', 1699000000, 1699100000)`); err != nil {
		t.Fatal(err)
	}
	unblocker := func(cfg *config.Scope) *unblocking {
		return &unblocking{watchRun: &watchRun{conn: conn, scope: "main", cfg: cfg, now: 1700000000},
			dir: "/home/u/.local/state/fleet/main-unblock"}
	}
	revisit := func(cfg *config.Scope, id string, project bool) string {
		p := parent(id, "started", 80*time.Hour, 1)
		if project {
			p.Team = &struct{ Key string }{"EX"}
			p.Project = &struct{ Name, Description, Content string }{"Example project", "", "Close a parent after a week.\n"}
		}
		got, err := unblocker(cfg).revisitBody(p, revisitName(id))
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	row := stopRow{ID: 7, Name: "wire-a", Role: "worker", Job: "wire", Parent: "wire-lead", Cwd: "/home/u/wt/r/wire",
		State: "active"}
	helper := func(ticket string) string {
		got, err := unblocker(sc).helperPrompt(row, helperName(row.ID), bashPrompt, ticket)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	for name, got := range map[string]string{
		"lead-single-issue":  rolePrompt(sc, lead, "/home/u/dev/example-dataset", "example-dataset", "/home/u/dev/example-dataset", 4, issue, "QT-10"),
		"lead-cross":         rolePrompt(sc, lead, "/home/u/x-repo/example-init/wire", "", "/home/u/x-repo/example-init/wire", 3, atb.Issue{}, ""),
		"worker-single":      rolePrompt(sc, worker, "/home/u/wt/example-dataset/wire", "example-dataset", "/home/u/dev/example-dataset", 4, atb.Issue{}, ""),
		"worker-cross-issue": rolePrompt(sc, worker, "/home/u/x-repo/example-init/wire", "", "/home/u/x-repo/example-init/wire", 3, issue, "QT-10"),
		"thread-repo-socket-ticket": thread(withSocket, "repo-example-dataset", issue, "The channel of example-dataset.",
			[]string{"Session 1: imported the B table."}),
		"thread-initiative":      thread(sc, "x-repo-example-init", atb.Issue{}, "", nil),
		"revisit-thread-project": revisit(withSocket, "EX-1", true),
		"revisit":                revisit(sc, "EX-2", false),
		"unblock-ticket":         helper("SC-9"),
		"unblock":                helper(""),
	} {
		file := filepath.Join("testdata", "prompts", name+".golden")
		if *update {
			if err := os.WriteFile(file, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal([]byte(got), want) {
			t.Errorf("%s differs from %s: rerun with -update and read `git diff`", name, file)
		}
	}
}

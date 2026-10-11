package cmd

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Luolc/agent-fleet/internal/atb"
	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/db"
)

// parent is a parent issue last changed `ago` before t0, with `children`
// sub-issues.
func parent(id, stateType string, ago time.Duration, children int) atb.Parent {
	p := atb.Parent{Identifier: id, Title: "Title of " + id, URL: "https://linear.example.test/" + id,
		UpdatedAt: time.Unix(1700000000, 0).Add(-ago), State: atb.State{Name: "In Progress", Type: stateType}}
	for range children {
		p.Children.Nodes = append(p.Children.Nodes, atb.Child{Identifier: "EX-99", Title: "a try",
			State: atb.State{Name: "Done", Type: "completed"}})
	}
	return p
}

func TestOnlyParentsStartedWithSubIssuesAndUnchangedPastTheLimitAreStale(t *testing.T) {
	sc, err := config.ParseScope([]byte(`{"watch": {"parent_stale": "48h"}}`), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	r := &watchRun{cfg: sc, now: 1700000000}
	got := r.staleParents([]atb.Parent{
		parent("EX-1", "started", 50*time.Hour, 2),
		parent("EX-2", "started", 47*time.Hour, 2),
		parent("EX-3", "canceled", 90*time.Hour, 2),
		parent("EX-4", "started", 90*time.Hour, 0),
		parent("EX-5", "started", 90*time.Hour, 1),
		parent("EX-6", "started", 48*time.Hour, 1),
	})
	var ids []string
	for _, p := range got {
		ids = append(ids, p.Identifier)
	}
	// The longest unchanged first; the limit itself counts.
	if strings.Join(ids, " ") != "EX-5 EX-1 EX-6" {
		t.Errorf("%q", ids)
	}
}

func TestTheRevisitAgentGetsTheIssueTheJobsTheInstructionsAndWhereToAsk(t *testing.T) {
	conn, err := db.OpenAt(filepath.Join(t.TempDir(), "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Exec(`INSERT INTO jobs (job, parent_issue, lead_cwd, home_thread, state, outcome, started_at, ended_at)
		VALUES ('a', 'EX-1', '/c', 'C1/1.0', 'ended', 'done', 1699000000, 1699100000),
		       ('b', 'EX-1', '/c', '', 'ended', 'abandoned', 1699200000, 1699300000)`); err != nil {
		t.Fatal(err)
	}
	sc, err := config.ParseScope([]byte(`{"fednet": {"socket": "/run/example/fednet.sock"}}`), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	u := &unblocking{watchRun: &watchRun{conn: conn, scope: "main", cfg: sc, now: 1700000000},
		dir: "/home/u/.local/state/fleet/main-unblock"}
	p := parent("EX-1", "started", 80*time.Hour, 1)
	p.Team = &struct{ Key string }{"EX"}
	p.Project = &struct{ Name, Description, Content string }{"Example project", "", "Close a parent after a week.\n\n## Not a section"}
	got, err := u.revisitBody(p, revisitName("EX-1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"You are revisit-ex-1, a revisit agent", "in ~/.local/state/fleet/main-unblock,",
		"has not changed in Linear for 3d08h", "no other agent for EX-1 until 3d00h have passed, and then only once it has gone 3d00h without a change", "`atb linear claim EX-1 --agent revisit-ex-1 --source watch --scope " +
			"'fleet watch revisit agent: closing EX-1'`", "When the claim exits 3",
		"atb linear create --team EX --project 'Example project' --parent EX-1 --title",
		"/home/u/.local/state/fleet/main-unblock/revisit-ex-1-question.md", "fleet posts it to the thread C1/1.0",
		"EX-1: Title of EX-1 (https://linear.example.test/EX-1), state In Progress, last changed 2023-11-11T14:13:20Z.",
		"- EX-99: a try (), state Done (completed)",
		"- a: ended, done, started 2023-11-03T08:26:40Z, ended 2023-11-04T12:13:20Z, reporting to thread C1/1.0\n- b: ended, abandoned",
		"## Its project: Example project", "to the end of this message:\n\nClose a parent after a week.\n\n## Not a section\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q not in the prompt", want)
		}
	}
	if strings.Contains(got, "{{") || strings.Contains(got, "needs-user") {
		t.Errorf("a placeholder, or the issue as the place to ask: %s", got)
	}
	// No home thread: the question goes on the issue.
	if _, err := conn.Exec("UPDATE jobs SET home_thread = ''"); err != nil {
		t.Fatal(err)
	}
	got, err = u.revisitBody(p, revisitName("EX-1"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "`atb linear edit EX-1 --add-label needs-user`") || strings.Contains(got, "question.md") {
		t.Errorf("no thread: %s", got)
	}
}

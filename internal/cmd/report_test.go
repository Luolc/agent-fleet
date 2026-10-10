package cmd

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Luolc/agent-fleet/internal/db"
)

func ledger(t *testing.T, statements ...string) *sql.DB {
	t.Helper()
	conn, err := db.OpenAt(filepath.Join(t.TempDir(), "example.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	for _, s := range statements {
		if _, err := conn.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return conn
}

// reportJSON is attentionOf as `report --json` prints it.
func reportJSON(t *testing.T, conn *sql.DB) string {
	t.Helper()
	report, err := attentionOf(conn)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestAnEmptyLedgerReportsNothingAsked(t *testing.T) {
	got := reportJSON(t, ledger(t))
	want := `{"jobs":[],"threads":[],"total":{"asked":0,"answered":0,"pending":0,"wait_median_secs":null,"wait_max_secs":null}}`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// A job name reused after the first job ended: each question counts for
// the job that was running when it was asked.
func TestQuestionsCountForTheirJobOrThreadWithWaits(t *testing.T) {
	conn := ledger(t,
		"INSERT INTO jobs (job, lead_cwd, state, outcome, started_at, ended_at) VALUES ('x', '/c', 'ended', 'done', 100, 400)",
		"INSERT INTO jobs (job, lead_cwd, state, started_at) VALUES ('x', '/c', 'open', 500)",
		"INSERT INTO jobs (job, lead_cwd, state, started_at) VALUES ('y', '/c', 'open', 550)",
		"INSERT INTO threads (thread, slug, ticket, created_at) VALUES ('C0123/1.1', 'c0123-1-1', 'EX-1', 0)",
		"INSERT INTO questions (job, thread, asked_by, text, state, asked_at, answered_at) VALUES "+
			"('x', 'C0123/1.1', 'x-lead', 'a', 'answered', 200, 260), "+
			"('x', 'C0123/1.1', 'x-lead', 'b', 'pending', 600, NULL), "+
			"('x', 'C0123/1.1', 'x-lead', 'c', 'answered', 700, 1000), "+
			"('', 'C0123/1.1', 'thread-c0123-1-1', 'd', 'answered', 50, 60), "+
			"('', 'C0123/1.1', 'thread-c0123-1-1', 'e', 'answered', 70, 100), "+
			"('', 'D0456/2.2', 'thread-d0456-2-2', 'f', 'pending', 80, NULL)")
	got := reportJSON(t, conn)
	want := `{"jobs":[` +
		`{"job":"x","state":"done","started_at":100,"asked":1,"answered":1,"pending":0,"wait_median_secs":60,"wait_max_secs":60},` +
		`{"job":"x","state":"open","started_at":500,"asked":2,"answered":1,"pending":1,"wait_median_secs":300,"wait_max_secs":300},` +
		`{"job":"y","state":"open","started_at":550,"asked":0,"answered":0,"pending":0,"wait_median_secs":null,"wait_max_secs":null}],` +
		`"threads":[` +
		`{"thread":"C0123/1.1","ticket":"EX-1","asked":2,"answered":2,"pending":0,"wait_median_secs":20,"wait_max_secs":30},` +
		`{"thread":"D0456/2.2","ticket":"","asked":1,"answered":0,"pending":1,"wait_median_secs":null,"wait_max_secs":null}],` +
		`"total":{"asked":6,"answered":4,"pending":2,"wait_median_secs":45,"wait_max_secs":300}}`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

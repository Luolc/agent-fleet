// `fleet report` on a ledger with rows inserted directly. Hermetic, see
// main_test.go.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reportWorld has two open jobs, one with a pending question and one that
// asked nothing, and a thread agent's question answered 90 s later.
func reportWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.exec(
		"INSERT INTO jobs (job, lead_cwd, state, started_at) VALUES ('x', '/c', 'open', 100), ('y', '/c', 'open', 200)",
		"INSERT INTO threads (thread, slug, ticket, created_at) VALUES ('C0123/1.1', 'c0123-1-1', 'EX-1', 0)",
		"INSERT INTO questions (job, thread, asked_by, text, state, asked_at, answered_at) VALUES "+
			"('x', 'C0123/1.1', 'x-lead', 'Merge?', 'pending', 300, NULL), "+
			"('', 'C0123/1.1', 'thread-c0123-1-1', 'Which repo?', 'answered', 10, 100)")
	return w
}

func TestReportCountsQuestionsPerJobAndThreadWithoutHerdr(t *testing.T) {
	w := reportWorld(t)
	// No herdr on PATH: `status` needs it, `report` does not.
	if err := os.Remove(filepath.Join(w.dir, "fake-herdr", "herdr")); err != nil {
		t.Fatal(err)
	}
	if out := w.run("", []string{"status"}); out.code == 0 {
		t.Fatalf("status ran without herdr: %+v", out)
	}
	out := w.run("", []string{"report"})
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	want := "JOB  STATE  ASKED  ANSWERED  PENDING  WAIT-MEDIAN  WAIT-MAX\n" +
		"x    open   1      0         1        -            -\n" +
		"y    open   0      0         0        -            -\n" +
		"\n" +
		"THREAD     TICKET  ASKED  ANSWERED  PENDING  WAIT-MEDIAN  WAIT-MAX\n" +
		"C0123/1.1  EX-1    1      1         0        1m           1m\n" +
		"\n" +
		"total: 2 asked, 1 answered, 1 pending; wait median 1m, longest 1m\n"
	if out.stdout != want {
		t.Errorf("got:\n%s\nwant:\n%s", out.stdout, want)
	}
	out = w.run("", []string{"report", "--json"})
	var report struct {
		Jobs    []map[string]any `json:"jobs"`
		Threads []map[string]any `json:"threads"`
		Total   map[string]any   `json:"total"`
	}
	if err := json.Unmarshal([]byte(out.stdout), &report); err != nil {
		t.Fatalf("%v in %q", err, out.stdout)
	}
	if len(report.Jobs) != 2 || report.Jobs[0]["pending"] != 1.0 || report.Jobs[1]["asked"] != 0.0 ||
		len(report.Threads) != 1 || report.Threads[0]["wait_max_secs"] != 90.0 || report.Total["asked"] != 2.0 {
		t.Errorf("%s", out.stdout)
	}
}

func TestReportOnALedgerWithNoJobs(t *testing.T) {
	w := newWorld(t)
	w.exec() // creates the ledger, with no rows
	out := w.run("", []string{"report"})
	want := "no jobs\n\nno questions from thread agents\n\ntotal: 0 asked, 0 answered, 0 pending; wait median -, longest -\n"
	if out.code != 0 || out.stdout != want {
		t.Errorf("%+v", out)
	}
	if out := w.run("", []string{"report", "--scope", "no-such-scope"}); out.code != 5 || !strings.Contains(out.stderr, "no ledger at") {
		t.Errorf("%+v", out)
	}
	if out := w.run("", []string{"help", "report"}); out.code != 0 || !strings.Contains(out.stdout, "not necessarily the answer") {
		t.Errorf("%+v", out)
	}
}

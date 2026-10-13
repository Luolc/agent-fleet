package cmd

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/herdr"
)

func TestAWorkerNameIsTheWatchAndTheKeyOrCutWithAHash(t *testing.T) {
	if got := workerName("unblock", "12"); got != "watch-unblock-12" {
		t.Errorf("plain: %s", got)
	}
	for _, key := range []string{"PR/12", "EX-1", "a-very-long-subject-key-of-a-ci-run"} {
		got := workerName("ci", key)
		if len(got) > 32 || !strings.HasPrefix(got, "watch-ci-") || got != workerName("ci", key) || CheckAgentName(got) != nil {
			t.Errorf("%s: %s", key, got)
		}
	}
	// Keys with the same slug, and the same key of two watches, get names
	// of their own.
	if workerName("ci", "PR/12") == workerName("ci", "pr-12") || workerName("ci", "EX-1") == workerName("ci", "ex-1") ||
		workerName("a", "k/1") == workerName("b", "k/1") {
		t.Error("two subjects share a worker")
	}
}

func workersAt(t *testing.T) (*workers, *sql.DB) {
	t.Helper()
	conn, err := db.OpenAt(filepath.Join(t.TempDir(), "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	sc, err := config.ParseScope([]byte(`{}`), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	return testWorkers(conn, sc), conn
}

// A worker watch's subject gets no worker while its worker is live or that
// worker's question waits; once the people answered after the last start,
// it gets one whatever its text and `again` say, and not again after that
// start.
func TestAWorkerWatchWaitsForItsWorkerAndThePeople(t *testing.T) {
	w, conn := workersAt(t)
	e := config.Entry{Name: "fix", Do: "worker"}
	s := subject{Key: "p1", Text: "PR 9", Job: "j"}
	acted := watchSubject{acts: 1, lastActed: sql.NullInt64{Int64: 1700000000, Valid: true}}
	due := func(byText bool) bool {
		t.Helper()
		got, err := w.dueFor(e, s, acted, byText)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if !due(true) || due(false) {
		t.Error("without a worker or a question, the text decides")
	}
	w.busy["watch-fix-p1"] = true
	if due(true) {
		t.Error("due while its worker is live")
	}
	w.busy = map[string]bool{}
	for _, stmt := range []string{
		"INSERT INTO agents (name, role, parent, state, started_at) VALUES ('watch-fix-p1', 'worker', 'watch', 'ended', 100)",
		"INSERT INTO questions (job, thread, asked_by, text, state, asked_at) VALUES ('j', 'C1/1.0', 'watch-fix-p1', 'q', 'pending', 150)",
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if due(true) {
		t.Error("due while its question waits")
	}
	if _, err := conn.Exec("UPDATE questions SET state = 'answered', answered_at = 200"); err != nil {
		t.Fatal(err)
	}
	if !due(false) {
		t.Error("not due once the people answered")
	}
	if _, err := conn.Exec("INSERT INTO agents (name, role, parent, state, started_at) VALUES ('watch-fix-p1', 'worker', 'watch', 'active', 300)"); err != nil {
		t.Fatal(err)
	}
	if due(false) {
		t.Error("due again after the start that got the answer")
	}
}

// A run out of time does not record that its watches ran, so the next
// run takes them up again.
func TestARunOutOfTimeDoesNotRecordItsWatchsRun(t *testing.T) {
	w, conn := workersAt(t)
	e := config.Entry{Name: "fix", Do: "worker", Every: time.Hour}
	budget := runBudget
	t.Cleanup(func() { runBudget = budget })
	w.start = time.Now()
	if err := w.ran(e); err != nil {
		t.Fatal(err)
	}
	runBudget = 0
	w.now += 7200
	if !w.outOfTime() {
		t.Fatal("not out of time")
	}
	if err := w.ran(e); err != nil {
		t.Fatal(err)
	}
	var at int64
	if err := conn.QueryRow("SELECT done_at FROM steps WHERE key = 'watch:fix' AND step = 'ran'").Scan(&at); err != nil || at != 1700000000 {
		t.Errorf("ran at %d, %v; want the first run's 1700000000", at, err)
	}
}

// With the unblock watch off, a screen no rule knows gets no worker; the
// folder-trust rule still answers its dialog, and the people are still
// asked about a hands-off screen and after the 3rd worker (here the ask
// fails, since the agent has no home thread, which shows it was made).
func TestWithTheUnblockWatchOffOnlyTheRulesAct(t *testing.T) {
	w, conn := workersAt(t)
	e := w.builtinWatch(config.BlockedScreens)
	e.Off = true
	pressed := 0
	settleStopped = func(_ *herdr.Herdr, name, cwd string) error {
		pressed++
		return nil
	}
	t.Cleanup(func() { settleStopped = settle })
	row := stopRow{ID: 7, Name: "s-perm", Role: "worker", Job: "s", Parent: "s-lead", Cwd: "/w/x", State: "active"}
	if err := w.handle(e, false, row, bashPrompt); err != nil {
		t.Fatal(err)
	}
	if err := w.handle(e, false, row, trust); err != nil || pressed != 1 {
		t.Fatalf("pressed %d, %v", pressed, err)
	}
	var rows int
	if err := conn.QueryRow("SELECT (SELECT count(*) FROM agents) + (SELECT count(*) FROM questions) + " +
		"(SELECT count(*) FROM steps)").Scan(&rows); err != nil || rows != 0 {
		t.Errorf("%d rows, %v", rows, err)
	}
	if err := w.handle(e, false, row, limitMenu); err == nil || !strings.Contains(err.Error(), "no home thread to ask the people in about s-perm") {
		t.Errorf("hands-off screen: %v", err)
	}
	for range maxHelpers {
		if _, err := conn.Exec("INSERT INTO agents (name, role, parent, state, started_at) VALUES ('watch-unblock-7', 'worker', 'watch', 'ended', 0)"); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.handle(e, false, row, bashPrompt); err == nil || !strings.Contains(err.Error(), "no home thread to ask the people in about s-perm") {
		t.Errorf("after the 3rd worker: %v", err)
	}
}

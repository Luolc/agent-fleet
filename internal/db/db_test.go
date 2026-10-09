package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestLedgerOpensInWALModeAndReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet", "example-dataset", "fleet.db")
	conn, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := conn.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	if _, err := conn.Exec(
		"INSERT INTO agents (name, role, state, started_at) VALUES ('a-lead', 'lead', 'active', 1)"); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err = OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var count, version int64
	if err := conn.QueryRow("SELECT count(*) FROM agents").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
	if err := conn.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Errorf("user_version = %d, want %d", version, schemaVersion)
	}
}

func TestLedgerPathUsesTheTargetUnderTheStateDir(t *testing.T) {
	path, err := PathUnder("/state", "example-dataset")
	if err != nil {
		t.Fatal(err)
	}
	if want := "/state/fleet/example-dataset/fleet.db"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if _, err := PathUnder("/state", ""); err == nil {
		t.Error("an empty target was accepted")
	}
}

func TestLedgerPathFallsBackToLocalStateWithoutXDGStateHome(t *testing.T) {
	t.Setenv("HOME", "/home/example")
	for _, xdg := range []string{"/xdg/state", ""} {
		t.Setenv("XDG_STATE_HOME", xdg)
		path, err := Path("example-dataset")
		if err != nil {
			t.Fatal(err)
		}
		want := "/xdg/state/fleet/example-dataset/fleet.db"
		if xdg == "" {
			want = "/home/example/.local/state/fleet/example-dataset/fleet.db"
		}
		if path != want {
			t.Errorf("XDG_STATE_HOME=%q: path = %q, want %q", xdg, path, want)
		}
	}
}

func TestAVersion2LedgerGainsTheWorktreesTableAndKeepsItsRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.db")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{schema, schemaV2,
		"INSERT INTO agents (name, role, state, started_at) VALUES ('a-lead', 'lead', 'active', 1)",
		"PRAGMA user_version = 2"} {
		if _, err := conn.Exec(step); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err = OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var agents, worktrees, version int64
	if err := conn.QueryRow("SELECT count(*) FROM agents").Scan(&agents); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow("SELECT count(*) FROM worktrees").Scan(&worktrees); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if agents != 1 || worktrees != 0 || version != schemaVersion {
		t.Errorf("agents %d, worktrees %d, version %d; want 1, 0, %d", agents, worktrees, version, schemaVersion)
	}
}

func TestAVersion3LedgerGainsTheIssueColumnsAndKeepsItsRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.db")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{schema, schemaV2, schemaV3,
		"INSERT INTO agents (name, role, state, started_at) VALUES ('a-lead', 'lead', 'active', 1)",
		"PRAGMA user_version = 3"} {
		if _, err := conn.Exec(step); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err = OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var name, issue, parentIssue string
	if err := conn.QueryRow("SELECT name, issue, parent_issue FROM agents").Scan(&name, &issue, &parentIssue); err != nil {
		t.Fatal(err)
	}
	if name != "a-lead" || issue != "" || parentIssue != "" {
		t.Errorf("row = %q %q %q", name, issue, parentIssue)
	}
}

func TestAVersion4LedgerGainsTheJobsTableAndRenamesWorktreeToCwd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.db")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{schema, schemaV2, schemaV3, schemaV4,
		"INSERT INTO agents (name, role, state, started_at, worktree) VALUES ('a-lead', 'lead', 'active', 1, '/w/a')",
		"PRAGMA user_version = 4"} {
		if _, err := conn.Exec(step); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err = OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var cwd string
	if err := conn.QueryRow("SELECT cwd FROM agents WHERE name = 'a-lead'").Scan(&cwd); err != nil {
		t.Fatal(err)
	}
	if cwd != "/w/a" {
		t.Errorf("cwd = %q", cwd)
	}
	var jobs, version int64
	if err := conn.QueryRow("SELECT count(*) FROM jobs").Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 || version != schemaVersion {
		t.Errorf("jobs %d, version %d", jobs, version)
	}
}

func TestAParentIssueHasAtMostOneLiveLead(t *testing.T) {
	conn, err := OpenAt(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	insert := func(name, role, parent, state string) error {
		_, err := conn.Exec("INSERT INTO agents (name, role, parent_issue, state, started_at) VALUES (?1, ?2, ?3, ?4, 1)",
			name, role, parent, state)
		return err
	}
	if err := insert("a-lead", "lead", "EX-1", "active"); err != nil {
		t.Fatal(err)
	}
	if err := insert("b-lead", "lead", "EX-1", "starting"); err == nil {
		t.Error("a second live lead on EX-1 was accepted")
	}
	for _, ok := range []func() error{
		func() error { return insert("c-lead", "lead", "EX-2", "active") },
		func() error { return insert("a-w1", "worker", "EX-1", "active") },
		func() error { return insert("d-lead", "lead", "EX-1", "ended") },
		func() error { return insert("e-lead", "lead", "", "active") },
		func() error { return insert("f-lead", "lead", "", "active") },
	} {
		if err := ok(); err != nil {
			t.Error(err)
		}
	}
}

func TestAnOpenJobsNameAndKeyAreUnique(t *testing.T) {
	conn, err := OpenAt(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	insert := func(job, key, state string) error {
		_, err := conn.Exec("INSERT INTO jobs (job, key, lead_cwd, state, started_at) VALUES (?1, ?2, '/c', ?3, 1)",
			job, key, state)
		return err
	}
	if err := insert("a", "k1", "open"); err != nil {
		t.Fatal(err)
	}
	if err := insert("a", "", "open"); err == nil {
		t.Error("a second open job named a was accepted")
	}
	if err := insert("b", "k1", "open"); err == nil {
		t.Error("a second open job with key k1 was accepted")
	}
	for _, ok := range []func() error{
		func() error { return insert("b", "", "open") },
		func() error { return insert("c", "", "open") },
		func() error { return insert("a", "k1", "ended") },
	} {
		if err := ok(); err != nil {
			t.Error(err)
		}
	}
}

func TestAVersion6LedgerGainsTheStepsAndQuestionsTablesAndKeepsItsRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.db")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{schema, schemaV2, schemaV3, schemaV4, schemaV5, schemaV6,
		"INSERT INTO jobs (job, lead_cwd, state, started_at) VALUES ('a', '/c', 'open', 1)",
		"PRAGMA user_version = 6"} {
		if _, err := conn.Exec(step); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err = OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var jobs, threads, inbox, steps, questions, version int64
	for query, into := range map[string]*int64{"SELECT count(*) FROM jobs": &jobs,
		"SELECT count(*) FROM threads": &threads, "SELECT count(*) FROM inbox": &inbox,
		"SELECT count(*) FROM steps": &steps, "SELECT count(*) FROM questions": &questions, "PRAGMA user_version": &version} {
		if err := conn.QueryRow(query).Scan(into); err != nil {
			t.Fatal(err)
		}
	}
	if jobs != 1 || threads != 0 || inbox != 0 || steps != 0 || questions != 0 || version != schemaVersion {
		t.Errorf("jobs %d, threads %d, inbox %d, steps %d, questions %d, version %d", jobs, threads, inbox, steps, questions, version)
	}
}

func TestAThreadHasAtMostOneLiveThreadAgentAndAMessageOneRow(t *testing.T) {
	conn, err := OpenAt(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	insert := func(name, thread, state string) error {
		_, err := conn.Exec("INSERT INTO agents (name, role, thread, state, started_at) VALUES (?1, 'thread', ?2, ?3, 1)",
			name, thread, state)
		return err
	}
	if err := insert("thread-a", "C1/1.1", "active"); err != nil {
		t.Fatal(err)
	}
	if err := insert("thread-a2", "C1/1.1", "starting"); err == nil {
		t.Error("a second live thread agent on C1/1.1 was accepted")
	}
	for _, ok := range []func() error{
		func() error { return insert("thread-b", "C1/2.2", "active") },
		func() error { return insert("thread-c", "C1/1.1", "ended") },
	} {
		if err := ok(); err != nil {
			t.Error(err)
		}
	}
	if _, err := conn.Exec("INSERT INTO inbox (msg_id, state, received_at) VALUES ('m1', 'reserved', 1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec("INSERT INTO inbox (msg_id, state, received_at) VALUES ('m1', 'reserved', 2)"); err == nil {
		t.Error("a second row for msg_id m1 was accepted")
	}
}

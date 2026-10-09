package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestLedgerOpensInWALModeAndReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scratch", "example-dataset", "fleet.db")
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

func TestLedgerPathUsesTheRepoNameUnderScratch(t *testing.T) {
	home := "/home/example"
	path, err := PathUnder(home, "acme/example-dataset")
	if err != nil {
		t.Fatal(err)
	}
	if want := "/home/example/scratch/example-dataset/fleet.db"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	bare, err := PathUnder(home, "example-dataset")
	if err != nil {
		t.Fatal(err)
	}
	if bare != path {
		t.Errorf("bare name gives %q, want %q", bare, path)
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

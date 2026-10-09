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

func TestLedgerPathUsesTheRepoNameUnderTheStateDir(t *testing.T) {
	path, err := PathUnder("/state", "acme/example-dataset")
	if err != nil {
		t.Fatal(err)
	}
	if want := "/state/fleet/example-dataset/fleet.db"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	bare, err := PathUnder("/state", "example-dataset")
	if err != nil {
		t.Fatal(err)
	}
	if bare != path {
		t.Errorf("bare name gives %q, want %q", bare, path)
	}
}

func TestLedgerPathFallsBackToLocalStateWithoutXDGStateHome(t *testing.T) {
	t.Setenv("HOME", "/home/example")
	for _, xdg := range []string{"/xdg/state", ""} {
		t.Setenv("XDG_STATE_HOME", xdg)
		path, err := Path("acme/example-dataset")
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
	if agents != 1 || worktrees != 0 || version != 3 {
		t.Errorf("agents %d, worktrees %d, version %d; want 1, 0, 3", agents, worktrees, version)
	}
}

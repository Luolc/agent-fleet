// Package db is the dispatch ledger:
// `$XDG_STATE_HOME/fleet/<repo>/fleet.db` (docs/design.md). The binary is its only reader and writer.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // the "sqlite" driver

	"github.com/Luolc/agent-fleet/internal/exit"
)

const schemaVersion = 2

// One table, `agents`. Ended rows are kept as history, so `name` is unique
// only among rows that have not ended.
const schema = `
CREATE TABLE agents (
    id               INTEGER PRIMARY KEY,
    name             TEXT    NOT NULL,
    role             TEXT    NOT NULL,
    job              TEXT    NOT NULL DEFAULT '',
    worktree         TEXT    NOT NULL DEFAULT '',
    parent           TEXT    NOT NULL DEFAULT '',
    report_to        TEXT    NOT NULL DEFAULT '',
    task             TEXT    NOT NULL DEFAULT '',
    pane_id          TEXT    NOT NULL DEFAULT '',
    state            TEXT    NOT NULL CHECK (state IN ('starting', 'active', 'ended')),
    started_at       INTEGER NOT NULL,
    ended_at         INTEGER,
    last_status      TEXT,
    last_seq         INTEGER,
    last_screen_hash TEXT,
    last_change_at   INTEGER,
    suspect          INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX agents_live_name ON agents (name) WHERE state != 'ended';
`

// Version 2: which repo and herdr session this ledger belongs to. At most
// one row. Nothing here reads or writes it; it is kept so the schema stays
// the one the earlier implementation wrote.
const schemaV2 = `
CREATE TABLE dataset (
    id      INTEGER PRIMARY KEY CHECK (id = 1),
    repo    TEXT    NOT NULL,
    session TEXT    NOT NULL
);
`

// Path is where the ledger of a repo lives:
// `$XDG_STATE_HOME/fleet/<name>/fleet.db`, with `~/.local/state` when
// XDG_STATE_HOME is unset or empty. `repo` may be `owner/name` or just
// `name`; only the name part is used.
func Path(repo string) (string, error) {
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home := os.Getenv("HOME")
		if home == "" {
			return "", exit.Environmentf("neither XDG_STATE_HOME nor HOME is set, cannot locate the ledger")
		}
		state = filepath.Join(home, ".local", "state")
	}
	return PathUnder(state, repo)
}

// PathUnder is Path with the state directory given, so no environment is
// read.
func PathUnder(state, repo string) (string, error) {
	name := repo
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		name = repo[i+1:]
	}
	if name == "" {
		return "", exit.Refusedf("the repo name is empty")
	}
	return filepath.Join(state, "fleet", name, "fleet.db"), nil
}

// Open opens (creating if needed) the database for `repo`: WAL mode, a 5 s
// busy timeout, and the schema at the current version.
func Open(repo string) (*sql.DB, error) {
	path, err := Path(repo)
	if err != nil {
		return nil, err
	}
	return OpenAt(path)
}

// OpenAt is Open at an explicit path.
func OpenAt(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return nil, exit.IO(err)
	}
	dsn := url.URL{
		Scheme:   "file",
		Path:     path,
		RawQuery: "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate",
	}
	conn, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, exit.Database(err)
	}
	conn.SetMaxOpenConns(1)
	if err := migrate(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// migrate reads the version, creates or upgrades the schema and sets the
// version marker in one immediate transaction, so an interrupted or
// concurrent open can never leave a schema in place with the marker still
// behind it.
func migrate(conn *sql.DB) error {
	tx, err := conn.BeginTx(context.Background(), nil)
	if err != nil {
		return exit.Database(err)
	}
	defer func() { _ = tx.Rollback() }()
	var version int64
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return exit.Database(err)
	}
	switch version {
	case 0:
		if _, err := tx.Exec(schema); err != nil {
			return exit.Database(err)
		}
		if _, err := tx.Exec(schemaV2); err != nil {
			return exit.Database(err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			return exit.Database(err)
		}
	case 1:
		if _, err := tx.Exec(schemaV2); err != nil {
			return exit.Database(err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			return exit.Database(err)
		}
	case schemaVersion:
	default:
		return exit.Environmentf(
			"database schema version %d is newer than this binary supports (%d)",
			version, schemaVersion)
	}
	if err := tx.Commit(); err != nil {
		return exit.Database(err)
	}
	return nil
}

// Now is the seconds since the Unix epoch, the timestamp format of every
// `*_at` column.
func Now() int64 {
	return time.Now().Unix()
}

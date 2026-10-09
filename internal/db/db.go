// Package db is the dispatch ledger of one target:
// `$XDG_STATE_HOME/fleet/<target>/fleet.db` (docs/design.md). The binary is
// its only reader and writer.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // the "sqlite" driver

	"github.com/Luolc/agent-fleet/internal/exit"
)

const schemaVersion = 5

// Version 1: the `agents` table. Ended rows are kept as history, so `name`
// is unique only among rows that have not ended.
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

// Version 2: a table nothing reads or writes any more; it is kept so an
// existing ledger upgrades through the same steps.
const schemaV2 = `
CREATE TABLE dataset (
    id      INTEGER PRIMARY KEY CHECK (id = 1),
    repo    TEXT    NOT NULL,
    session TEXT    NOT NULL
);
`

// Version 3: the worktrees `fleet worktree` made, each owned by a job, so
// ending the job can remove them. A row is live until removed_at is set; a
// path has at most one live row.
const schemaV3 = `
CREATE TABLE worktrees (
    id         INTEGER PRIMARY KEY,
    path       TEXT    NOT NULL,
    repo       TEXT    NOT NULL,
    branch     TEXT    NOT NULL,
    job        TEXT    NOT NULL,
    created_by TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    removed_at INTEGER
);
CREATE UNIQUE INDEX worktrees_live_path ON worktrees (path) WHERE removed_at IS NULL;
`

// Version 4: the agent's work order and its job's parent issue, both
// Linear identifiers; empty when the job does not use Linear.
const schemaV4 = `
ALTER TABLE agents ADD COLUMN issue TEXT NOT NULL DEFAULT '';
ALTER TABLE agents ADD COLUMN parent_issue TEXT NOT NULL DEFAULT '';
`

// Version 5: the `jobs` table, one row per job started in this target, and
// the agent's working directory (`cwd`, formerly `worktree`: fleet no
// longer makes a worktree for an agent). A job is open until ended_at is
// set; a name, and a non-empty key, are unique among open jobs, and a
// parent issue has at most one live lead. `repo` is empty for a cross-repo
// job; `home_thread` is reserved for the thread the job reports to;
// `outcome` is set when the job ends.
const schemaV5 = `
ALTER TABLE agents RENAME COLUMN worktree TO cwd;
CREATE UNIQUE INDEX agents_live_lead_parent ON agents (parent_issue)
    WHERE role = 'lead' AND state != 'ended' AND parent_issue != '';
CREATE TABLE jobs (
    id           INTEGER PRIMARY KEY,
    job          TEXT    NOT NULL,
    parent_issue TEXT    NOT NULL DEFAULT '',
    key          TEXT    NOT NULL DEFAULT '',
    repo         TEXT    NOT NULL DEFAULT '',
    lead_cwd     TEXT    NOT NULL,
    home_thread  TEXT    NOT NULL DEFAULT '',
    state        TEXT    NOT NULL CHECK (state IN ('open', 'ended')),
    outcome      TEXT    NOT NULL DEFAULT '' CHECK (outcome IN ('', 'done', 'abandoned')),
    started_at   INTEGER NOT NULL,
    ended_at     INTEGER
);
CREATE UNIQUE INDEX jobs_open_job ON jobs (job) WHERE state = 'open';
CREATE UNIQUE INDEX jobs_open_key ON jobs (key) WHERE state = 'open' AND key != '';
`

// migrations[v] upgrades a ledger at version v to v+1.
var migrations = [schemaVersion]string{schema, schemaV2, schemaV3, schemaV4, schemaV5}

// Path is where the ledger of `target` lives:
// `$XDG_STATE_HOME/fleet/<target>/fleet.db`, with `~/.local/state` when
// XDG_STATE_HOME is unset or empty.
func Path(target string) (string, error) {
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home := os.Getenv("HOME")
		if home == "" {
			return "", exit.Environmentf("neither XDG_STATE_HOME nor HOME is set, cannot locate the ledger")
		}
		state = filepath.Join(home, ".local", "state")
	}
	return PathUnder(state, target)
}

// PathUnder is Path with the state directory given, so no environment is
// read.
func PathUnder(state, target string) (string, error) {
	if target == "" {
		return "", exit.Refusedf("the target name is empty")
	}
	return filepath.Join(state, "fleet", target, "fleet.db"), nil
}

// Open opens (creating if needed) the database of `target`: WAL mode, a
// 5 s busy timeout, and the schema at the current version.
func Open(target string) (*sql.DB, error) {
	path, err := Path(target)
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
	if version > schemaVersion {
		return exit.Environmentf(
			"database schema version %d is newer than this binary supports (%d)",
			version, schemaVersion)
	}
	if version == schemaVersion {
		return nil
	}
	for _, step := range migrations[version:] {
		if _, err := tx.Exec(step); err != nil {
			return exit.Database(err)
		}
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return exit.Database(err)
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

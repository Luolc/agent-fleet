// Package db is the dispatch ledger of one scope:
// `$XDG_STATE_HOME/fleet/<scope>.db` (docs/design.md). The binary is
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

const schemaVersion = 11

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

// Version 5: the `jobs` table, one row per job started in this scope, and
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

// Version 6 adds `steps`: the completed steps of a multi-step ending
// (`job end`), keyed by the ending, so a retry skips what is done.
const schemaV6 = `
CREATE TABLE steps (
    id      INTEGER PRIMARY KEY,
    key     TEXT    NOT NULL,
    step    TEXT    NOT NULL,
    done_at INTEGER NOT NULL,
    UNIQUE (key, step)
);
`

// Version 7: thread agents. `agents.thread` is the thread key of a thread
// agent's row (empty for the other roles), and a thread has at most one
// live thread agent. `threads` is one row per thread this scope has
// seen: its slug (the agent is `thread-<slug>`), the channel's context
// as it came with the first message, the thread ticket (empty with
// Linear off) and how many sessions were started on it. `inbox` is one
// row per fednet message by its `msg_id`: `reserved` once `fleet inbox`
// took it, `delivered` once the thread agent has it, `dropped` when it
// was given up (Linear unavailable); a rerun of the same message does
// nothing lasting.
const schemaV7 = `
ALTER TABLE agents ADD COLUMN thread TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX agents_live_thread ON agents (thread)
    WHERE role = 'thread' AND state != 'ended' AND thread != '';
CREATE TABLE threads (
    id         INTEGER PRIMARY KEY,
    thread     TEXT    NOT NULL UNIQUE,
    slug       TEXT    NOT NULL,
    channel    TEXT    NOT NULL DEFAULT '',
    context    TEXT    NOT NULL DEFAULT '',
    ticket     TEXT    NOT NULL DEFAULT '',
    ticket_url TEXT    NOT NULL DEFAULT '',
    sessions   INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);
CREATE TABLE inbox (
    id          INTEGER PRIMARY KEY,
    msg_id      TEXT    NOT NULL UNIQUE,
    thread      TEXT    NOT NULL DEFAULT '',
    state       TEXT    NOT NULL CHECK (state IN ('reserved', 'delivered', 'dropped')),
    received_at INTEGER NOT NULL
);
`

// Version 8: `questions`, one row per `fleet ask-human`: the job it came
// from (empty when a thread agent asked), the thread it went to, who
// asked, the text, whether an approval card was asked for, and `pending`
// until a person's message arrives in that thread.
const schemaV8 = `
CREATE TABLE questions (
    id          INTEGER PRIMARY KEY,
    job         TEXT    NOT NULL DEFAULT '',
    thread      TEXT    NOT NULL,
    asked_by    TEXT    NOT NULL,
    text        TEXT    NOT NULL,
    approval    INTEGER NOT NULL DEFAULT 0,
    state       TEXT    NOT NULL CHECK (state IN ('pending', 'answered')),
    asked_at    INTEGER NOT NULL,
    answered_at INTEGER
);
`

// Version 9: where a thread belongs, fixed at its first delivery so a
// renamed channel does not move it: `mapping` (the channel's name, with
// the scope's repo or initiative prefix; a direct message has the general
// initiative's) and `cwd`, the directory its agents run in.
const schemaV9 = `
ALTER TABLE threads ADD COLUMN mapping TEXT NOT NULL DEFAULT '';
ALTER TABLE threads ADD COLUMN cwd TEXT NOT NULL DEFAULT '';
`

// Version 10: the latest message a person posted in the thread, as
// `inbox` delivered it (text, Slack user, timestamp), so `job start` can
// hand the lead the person's own words.
const schemaV10 = `
ALTER TABLE threads ADD COLUMN last_text TEXT NOT NULL DEFAULT '';
ALTER TABLE threads ADD COLUMN last_user TEXT NOT NULL DEFAULT '';
ALTER TABLE threads ADD COLUMN last_ts TEXT NOT NULL DEFAULT '';
`

// Version 11: what `fleet watch` keeps. A question may be `closed`
// (its job ended, or watch gave up on it) and counts the reminders posted
// for it, with the msg_id of the latest; `agents.reclaim_at` is when watch
// closes a thread agent it asked to end its session, `jobs.reclaim_at`
// when it ends a job whose lead it told that its question expired; and
// `threads.quiet_asked` is the timestamp of the thread's last message when
// watch last asked the thread agent about the quiet, so it asks once per
// quiet spell. SQLite cannot change a CHECK, so `questions` is rebuilt.
const schemaV11 = `
CREATE TABLE questions_v11 (
    id           INTEGER PRIMARY KEY,
    job          TEXT    NOT NULL DEFAULT '',
    thread       TEXT    NOT NULL,
    asked_by     TEXT    NOT NULL,
    text         TEXT    NOT NULL,
    approval     INTEGER NOT NULL DEFAULT 0,
    state        TEXT    NOT NULL CHECK (state IN ('pending', 'answered', 'closed')),
    asked_at     INTEGER NOT NULL,
    answered_at  INTEGER,
    reminders    INTEGER NOT NULL DEFAULT 0,
    reminder_msg TEXT    NOT NULL DEFAULT ''
);
INSERT INTO questions_v11 (id, job, thread, asked_by, text, approval, state, asked_at, answered_at)
    SELECT id, job, thread, asked_by, text, approval, state, asked_at, answered_at FROM questions;
DROP TABLE questions;
ALTER TABLE questions_v11 RENAME TO questions;
ALTER TABLE agents ADD COLUMN reclaim_at INTEGER;
ALTER TABLE jobs ADD COLUMN reclaim_at INTEGER;
ALTER TABLE threads ADD COLUMN quiet_asked TEXT NOT NULL DEFAULT '';
`

// migrations[v] upgrades a ledger at version v to v+1.
var migrations = [schemaVersion]string{schema, schemaV2, schemaV3, schemaV4, schemaV5, schemaV6, schemaV7, schemaV8, schemaV9, schemaV10, schemaV11}

// Path is where the ledger of `scope` lives:
// `$XDG_STATE_HOME/fleet/<scope>.db`, with `~/.local/state` when
// XDG_STATE_HOME is unset or empty.
func Path(scope string) (string, error) {
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home := os.Getenv("HOME")
		if home == "" {
			return "", exit.Environmentf("neither XDG_STATE_HOME nor HOME is set, cannot locate the ledger")
		}
		state = filepath.Join(home, ".local", "state")
	}
	return PathUnder(state, scope)
}

// PathUnder is Path with the state directory given, so no environment is
// read.
func PathUnder(state, scope string) (string, error) {
	if scope == "" {
		return "", exit.Refusedf("the scope name is empty")
	}
	return filepath.Join(state, "fleet", scope+".db"), nil
}

// Open opens (creating if needed) the database of `scope`: WAL mode, a
// 5 s busy timeout, and the schema at the current version.
func Open(scope string) (*sql.DB, error) {
	path, err := Path(scope)
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

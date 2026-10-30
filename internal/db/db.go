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

const schemaVersion = 22

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

// Version 11: what `fleet watch` keeps. A question may be `closed` (watch
// gave up on it) and counts the reminders posted for it, with the msg_id
// of the latest; `agents.reclaim_at` is when watch
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

// Version 12: every reminder `fleet watch` posted, by fednet msg_id, so
// a newer one in the thread replaces it: `posted` while it is the
// thread's latest, `stale` once a newer one is posted and until it is
// deleted, then `deleted` (or gone already), or `kept` when fednet
// refused to delete it. The reminders the questions name are taken over
// as posted.
const schemaV12 = `
CREATE TABLE reminders (
    id     INTEGER PRIMARY KEY,
    msg_id TEXT    NOT NULL UNIQUE,
    thread TEXT    NOT NULL,
    state  TEXT    NOT NULL CHECK (state IN ('posted', 'stale', 'deleted', 'kept'))
);
INSERT INTO reminders (msg_id, thread, state)
    SELECT reminder_msg, min(thread), 'posted' FROM questions WHERE reminder_msg != '' GROUP BY reminder_msg ORDER BY min(id);
`

// Version 13: the subjects of the scope's command watches, one row per
// subject its source gave in its last run: the digest of the text when it
// was last acted on, when it was first seen and last acted on, the acts
// so far, and when the people were asked about it once its acts reached
// the watch's `max`.
const schemaV13 = `
CREATE TABLE watch_subjects (
    id         INTEGER PRIMARY KEY,
    watch      TEXT    NOT NULL,
    key        TEXT    NOT NULL,
    digest     TEXT    NOT NULL DEFAULT '',
    first_seen INTEGER NOT NULL,
    last_acted INTEGER,
    acts       INTEGER NOT NULL DEFAULT 0,
    asked_at   INTEGER,
    UNIQUE (watch, key)
);
`

// Version 14: whether `job start --new-parent` created the job's parent
// issue, so `job end` can close a parent nothing else asked for.
const schemaV14 = `
ALTER TABLE jobs ADD COLUMN parent_created INTEGER NOT NULL DEFAULT 0;
`

// Version 15: whether the agent was started with `--light`, past the load
// check on its word that it only reads.
const schemaV15 = `
ALTER TABLE agents ADD COLUMN light INTEGER NOT NULL DEFAULT 0;
`

// Version 16: question cards. `card` marks a question asked as a card
// (its text the card's JSON): a thread agent's, posted through `fednet
// client ask`, or a lead's, handed to the thread agent; `card_id` and
// `permalink` are where fednet posted it; `for_question` is the lead's
// question a thread agent's card asks for; `outcome` is the last press
// on the card (`answered`, `snoozed`, `closed`, `resurfaced`), or
// `withdrawn` when the agent took it back; `snooze_until` is when a
// snoozed card comes back.
const schemaV16 = `
ALTER TABLE questions ADD COLUMN card INTEGER NOT NULL DEFAULT 0;
ALTER TABLE questions ADD COLUMN card_id TEXT NOT NULL DEFAULT '';
ALTER TABLE questions ADD COLUMN permalink TEXT NOT NULL DEFAULT '';
ALTER TABLE questions ADD COLUMN for_question INTEGER;
ALTER TABLE questions ADD COLUMN outcome TEXT NOT NULL DEFAULT '';
ALTER TABLE questions ADD COLUMN snooze_until INTEGER;
`

// Version 17: `answer`, the last press on a card as the agents get it
// (fednet's JSON, its question_id the question's id), so the next worker
// of a watch reads what the people chose and wrote.
const schemaV17 = `
ALTER TABLE questions ADD COLUMN answer TEXT NOT NULL DEFAULT '';
`

// Version 18: `handoffs`, one row per `fleet thread handoff`: the thread
// it came from, the thread agent that made it, the target's mapping, the
// task as given, the thread fleet opened for it (empty until it is
// opened) and when the last step was done (null until then).
const schemaV18 = `
CREATE TABLE handoffs (
    id          INTEGER PRIMARY KEY,
    from_thread TEXT    NOT NULL,
    by          TEXT    NOT NULL,
    mapping     TEXT    NOT NULL,
    task        TEXT    NOT NULL,
    to_thread   TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    done_at     INTEGER
);
`

// Version 19: why statute did not generate a worktree's private output
// when `fleet worktree` last ran it there; empty when it did, or when the
// worktree has no statute.toml.
const schemaV19 = `
ALTER TABLE worktrees ADD COLUMN statute_failure TEXT NOT NULL DEFAULT '';
`

// Version 20: `orphan_tabs`, the tabs of the `threads` workspace that
// `fleet watch` found it may close, one row per pane: its tab, the pid of
// its shell and the run that first found it so. Its tab is closed only
// when a run 10 minutes later or more finds it so again, in the same tab
// with the same shell; a run that finds it otherwise deletes its row.
const schemaV20 = `
CREATE TABLE orphan_tabs (
    pane_id       TEXT    PRIMARY KEY,
    tab_id        TEXT    NOT NULL,
    shell_pid     INTEGER NOT NULL,
    first_seen_at INTEGER NOT NULL
);
`

// Version 21: what the watch `model-changes` of `fleet watch` read of
// each agent's Claude Code transcript (`transcripts`, one row per agent
// row and session: the file, how far it was read, and the model of the
// session's first and latest assistant message) and the model events it
// found there (`model_events`, one per agent row and event key, with no
// text of the conversation), each reported once to the agent's parent
// (`told_at`) and to its thread (`posted_at`), null until reported or
// found to have nowhere to go.
const schemaV21 = `
CREATE TABLE transcripts (
    agent       INTEGER NOT NULL,
    session     TEXT    NOT NULL,
    path        TEXT    NOT NULL,
    read_to     INTEGER NOT NULL DEFAULT 0,
    start_model TEXT    NOT NULL DEFAULT '',
    last_model  TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (agent, session)
);
CREATE TABLE model_events (
    id          INTEGER PRIMARY KEY,
    agent       INTEGER NOT NULL,
    key         TEXT    NOT NULL,
    kind        TEXT    NOT NULL,
    at          TEXT    NOT NULL,
    from_model  TEXT    NOT NULL,
    to_model    TEXT    NOT NULL,
    start_model TEXT    NOT NULL,
    uuid        TEXT    NOT NULL,
    found_at    INTEGER NOT NULL,
    told_at     INTEGER,
    posted_at   INTEGER,
    UNIQUE (agent, key)
);
`

// Version 22: what a start passed the agent runtime (`kind`, `claude` or
// `pi`, empty in rows from before; `model` and `effort`, null when none
// was passed) and the work order's URL, so the watch `model-changes` can
// start the same agent again; `takeovers`, how many times that watch
// replaced the row's session, told it to go on or tried to, and on a
// thread how many times it replaced the thread's session; the model a
// session's latest fallback after a refusal moved it to
// (`transcripts.fell_to`, empty when none did), whether its latest
// assistant message is stopped (`stopped`) and when the watch decided
// about its present stop or move (`decided_at`, null while it is
// neither; 0 for the sessions read before, so what was reported before
// is not acted on); and `model_actions`, what the watch did about a stop
// or move, with its detail for the agents (`detail`) and for the people
// in a thread (`detail_zh`), each reported once to the agent's parent
// (`told_at`) and its thread (`posted_at`).
const schemaV22 = `
ALTER TABLE agents ADD COLUMN kind TEXT NOT NULL DEFAULT '';
ALTER TABLE agents ADD COLUMN model TEXT;
ALTER TABLE agents ADD COLUMN effort TEXT;
ALTER TABLE agents ADD COLUMN issue_url TEXT NOT NULL DEFAULT '';
ALTER TABLE agents ADD COLUMN takeovers INTEGER NOT NULL DEFAULT 0;
ALTER TABLE threads ADD COLUMN takeovers INTEGER NOT NULL DEFAULT 0;
ALTER TABLE transcripts ADD COLUMN fell_to TEXT NOT NULL DEFAULT '';
ALTER TABLE transcripts ADD COLUMN stopped INTEGER NOT NULL DEFAULT 0;
ALTER TABLE transcripts ADD COLUMN decided_at INTEGER;
UPDATE transcripts SET decided_at = 0;
CREATE TABLE model_actions (
    id         INTEGER PRIMARY KEY,
    agent      INTEGER NOT NULL,
    session    TEXT    NOT NULL,
    action     TEXT    NOT NULL,
    why        TEXT    NOT NULL,
    from_model TEXT    NOT NULL,
    to_model   TEXT    NOT NULL,
    detail     TEXT    NOT NULL DEFAULT '',
    detail_zh  TEXT    NOT NULL DEFAULT '',
    at         INTEGER NOT NULL,
    told_at    INTEGER,
    posted_at  INTEGER
);
`

// migrations[v] upgrades a ledger at version v to v+1.
var migrations = [schemaVersion]string{schema, schemaV2, schemaV3, schemaV4, schemaV5, schemaV6, schemaV7, schemaV8, schemaV9, schemaV10, schemaV11, schemaV12, schemaV13, schemaV14, schemaV15, schemaV16, schemaV17, schemaV18, schemaV19, schemaV20, schemaV21, schemaV22}

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

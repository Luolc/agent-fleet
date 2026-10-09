// Recorded steps of an ending, so a retry after a partial failure skips
// what was done and continues. Written here until the shared helper of
// `job end` lands; the signature is the agreed one.

package cmd

import (
	"database/sql"
	"errors"

	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
)

// runStep runs `do` for the step `step` of the ending `key`, unless the
// ledger records it as done: then it returns nil at once. A `do` that
// returns nil is recorded before runStep returns. Nothing else counts as
// done: an atb exit 4 (no holder) on a retry is a failure like any other.
func runStep(conn querier, key, step string, do func() error) error {
	var one int
	err := conn.QueryRow("SELECT 1 FROM steps WHERE key = ?1 AND step = ?2", key, step).Scan(&one)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return exit.Database(err)
	}
	if err := do(); err != nil {
		return err
	}
	if _, err := conn.Exec("INSERT INTO steps (key, step, done_at) VALUES (?1, ?2, ?3)", key, step, db.Now()); err != nil {
		return exit.Database(err)
	}
	return nil
}

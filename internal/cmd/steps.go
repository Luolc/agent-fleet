// Resumable multi-step operations: each completed step is recorded in the
// ledger, so a retry after a partial failure skips it.

package cmd

import (
	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
)

// runStep runs `do` unless the ledger records that step `step` of the
// operation `key` is done, and records it once `do` returns nil. Only the
// record says a step was ours and done: a step that fails because it was
// already done elsewhere (atb refusing to release an issue nobody holds)
// is still a failure here.
func runStep(conn querier, key, step string, do func() error) error {
	var done int64
	err := conn.QueryRow("SELECT count(*) FROM steps WHERE key = ?1 AND step = ?2", key, step).Scan(&done)
	if err != nil {
		return exit.Database(err)
	}
	if done > 0 {
		return nil
	}
	if err := do(); err != nil {
		return err
	}
	if _, err := conn.Exec("INSERT INTO steps (key, step, done_at) VALUES (?1, ?2, ?3)", key, step, db.Now()); err != nil {
		return exit.Database(err)
	}
	return nil
}

// The emoji fleet keeps on a thread's root message, so people can find
// threads in Slack (`has::question:`, `has::eyes:`): its state, and
// whether it is still to be looked at.

package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/fednet"
)

// The state marks: a card waits for the people; the session runs or a
// job reports to the thread; neither. markLook says the people have yet
// to look at the thread; archived is the reaction by which they say they
// are done with it.
const (
	markAsking = "question"
	markBusy   = "hourglass_flowing_sand"
	markDone   = "white_check_mark"
	markLook   = "eyes"
	archived   = "package"
)

var stateMarks = []string{markAsking, markBusy, markDone}

// marksPerRun is how many threads one `watch` run marks, newest first, so
// a ledger full of unmarked threads (an upgrade) is caught up over runs.
const marksPerRun = 10

// markWant is, in SQL, the state mark the thread `t.thread` should have.
// A card waiting for the people comes first: it is what needs them.
const markWant = `CASE
  WHEN EXISTS (SELECT 1 FROM questions q WHERE q.thread = t.thread AND q.state = 'pending' AND q.card_id != '')
    THEN '` + markAsking + `'
  WHEN EXISTS (SELECT 1 FROM agents a WHERE a.thread = t.thread AND a.role = 'thread' AND a.state != 'ended')
    OR EXISTS (SELECT 1 FROM jobs j WHERE j.home_thread = t.thread AND j.state = 'open')
    THEN '` + markBusy + `'
  ELSE '` + markDone + `' END`

// recorded is, in SQL, the one step recorded under `<prefix><t.thread>`,
// ” when none: `mark:` the state mark fleet put, `look:` whether the
// eyes mark should be on (`on`, `off`), `look-put:` whether fleet put it
// on or took it off.
func recorded(prefix string) string {
	return "COALESCE((SELECT s.step FROM steps s WHERE s.key = '" + prefix + "' || t.thread), '')"
}

// record replaces the step recorded under `key` by `step`.
func record(conn querier, key, step string) error {
	if _, err := conn.Exec("DELETE FROM steps WHERE key = ?1", key); err != nil {
		return exit.Database(err)
	}
	if _, err := conn.Exec("INSERT INTO steps (key, step, done_at) VALUES (?1, ?2, ?3)", key, step, db.Now()); err != nil {
		return exit.Database(err)
	}
	return nil
}

// wantLook records whether the thread's eyes mark should be on; the next
// syncMarks puts it.
func wantLook(conn querier, thread string, on bool) error {
	step := "off"
	if on {
		step = "on"
	}
	return record(conn, "look:"+thread, step)
}

// staleMarks are up to n threads with a mark that is not the one the
// ledger records fleet put, newest first.
func staleMarks(conn *sql.DB, n int) ([]string, error) {
	rows, err := conn.Query("SELECT thread FROM (SELECT t.id, t.thread, "+markWant+" AS want, "+recorded("mark:")+" AS put, "+
		recorded("look:")+" AS look, "+recorded("look-put:")+" AS look_put FROM threads t) "+
		"WHERE want != put OR look != '' AND look != look_put ORDER BY id DESC LIMIT ?1", n)
	if err != nil {
		return nil, exit.Database(err)
	}
	defer rows.Close()
	var threads []string
	for rows.Next() {
		var thread string
		if err := rows.Scan(&thread); err != nil {
			return nil, exit.Database(err)
		}
		threads = append(threads, thread)
	}
	if err := rows.Err(); err != nil {
		return nil, exit.Database(err)
	}
	return threads, nil
}

// syncMarks puts on the thread's root the marks that differ from what the
// ledger records fleet put, and returns what it changed. The state mark
// is added and the other two taken off (fednet counts one not there as
// taken off), then recorded; the eyes mark is added or taken off, then
// recorded. A failure leaves the record as it was, so the next try does
// that part again.
func syncMarks(conn querier, socket, thread string) (changed []string, err error) {
	var want, put, look, lookPut string
	err = conn.QueryRow("SELECT "+markWant+", "+recorded("mark:")+", "+recorded("look:")+", "+recorded("look-put:")+
		" FROM threads t WHERE t.thread = ?1", thread).Scan(&want, &put, &look, &lookPut)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, exit.Database(err)
	}
	if want != put {
		if err := putState(socket, thread, want); err != nil {
			return changed, err
		}
		if err := record(conn, "mark:"+thread, want); err != nil {
			return changed, err
		}
		changed = append(changed, want)
	}
	if look != "" && look != lookPut {
		if err := fednet.React(socket, thread, markLook, look == "off"); err != nil {
			return changed, err
		}
		if err := record(conn, "look-put:"+thread, look); err != nil {
			return changed, err
		}
		changed = append(changed, markLook+" "+look)
	}
	return changed, nil
}

// putState adds the state mark `want` to the thread's root and takes the
// other two off.
func putState(socket, thread, want string) error {
	if err := fednet.React(socket, thread, want, false); err != nil {
		return err
	}
	for _, other := range stateMarks {
		if other == want {
			continue
		}
		if err := fednet.React(socket, thread, other, true); err != nil {
			return err
		}
	}
	return nil
}

// markThread brings the thread's marks up to date after a command changed
// what it waits on. A failure is a warning on stderr and never fails the
// command: `watch` tries again on its next run.
func markThread(conn querier, socket, thread string) {
	if socket == "" || thread == "" {
		return
	}
	if _, err := syncMarks(conn, socket, thread); err != nil {
		fmt.Fprintf(os.Stderr, "warning: the emoji on thread %s were not updated: %v\n", thread, err)
	}
}

// look records whether the thread's eyes mark should be on, then brings
// its marks up to date, as markThread does; a failure is a warning.
func look(conn querier, socket, thread string, on bool) {
	if err := wantLook(conn, thread, on); err != nil {
		fmt.Fprintf(os.Stderr, "warning: the emoji on thread %s were not updated: %v\n", thread, err)
		return
	}
	markThread(conn, socket, thread)
}

// marks is the watch rule that catches up the marks the commands left
// stale: a failed update, or a change no command marks (a session `watch`
// itself ended, a question it closed, a home thread it opened). A failure
// is a warning and leaves the run's exit code as it is.
func (r *watchRun) marks() error {
	threads, err := staleMarks(r.conn, marksPerRun)
	if err != nil {
		return err
	}
	for _, thread := range threads {
		if r.outOfTime() {
			return nil
		}
		changed, err := syncMarks(r.conn, r.cfg.FednetSocket, thread)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: the emoji on thread %s were not updated: %v\n", thread, err)
			continue
		}
		for _, c := range changed {
			fmt.Fprintf(os.Stdout, "marked thread %s %s\n", thread, c)
		}
	}
	return nil
}

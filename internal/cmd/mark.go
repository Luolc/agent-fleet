// The emoji fleet keeps on a thread's root message, so people can find
// threads in Slack (`has::question:`, `has::eyes:`): its state, and
// whether it is still to be looked at.

package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

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

// markCheck is how long after fleet last put a thread's state mark
// `watch` puts it again, the other two taken off as well, whatever the
// record says: every markCheck while the thread is not done, and once
// after it became done. Two writers marking a thread at the same moment
// (a command and `watch`, from what each read) can leave two state marks
// on it; this takes the extra one off.
const markCheck = 10 * 60

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
// empty when none: `mark:` the state mark fleet put, `look-put:` whether
// fleet put the eyes mark on or took it off.
func recorded(prefix string) string {
	return "COALESCE((SELECT s.step FROM steps s WHERE s.key = '" + prefix + "' || t.thread), '')"
}

// lookWant is, in SQL, whether the eyes mark should be on: what `look:`
// records (`on`, `off`), and `on` for a thread with no record, which the
// people have not archived (a thread from before the marks, say).
const lookWant = "COALESCE((SELECT s.step FROM steps s WHERE s.key = 'look:' || t.thread), 'on')"

// putAt is, in SQL, when the record under `<prefix><t.thread>` was
// written, 0 when none: `mark:` when the state mark last changed,
// `mark-put:` when fleet last put it.
func putAt(prefix string) string {
	return "COALESCE((SELECT s.done_at FROM steps s WHERE s.key = '" + prefix + "' || t.thread), 0)"
}

// record replaces the step recorded under `key` by `step`, written at
// `at`.
func record(conn querier, key, step string, at int64) error {
	if _, err := conn.Exec("DELETE FROM steps WHERE key = ?1", key); err != nil {
		return exit.Database(err)
	}
	if _, err := conn.Exec("INSERT INTO steps (key, step, done_at) VALUES (?1, ?2, ?3)", key, step, at); err != nil {
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
	return record(conn, "look:"+thread, step, db.Now())
}

// staleMarks are up to n threads, newest first, with a mark that is not
// the one the ledger records fleet put, or whose state mark is due to be
// put again (markCheck) at `now`; check says which are due.
func staleMarks(conn *sql.DB, n int, now int64) (threads []string, check []bool, err error) {
	rows, err := conn.Query("SELECT thread, put != '' AND put_at <= ?2 - ?3 AND (want != '"+markDone+"' OR put_at < changed + ?3) "+
		"FROM (SELECT t.id, t.thread, "+markWant+" AS want, "+recorded("mark:")+" AS put, "+putAt("mark:")+" AS changed, "+
		putAt("mark-put:")+" AS put_at, "+lookWant+" AS look, "+recorded("look-put:")+" AS look_put FROM threads t) "+
		"WHERE want != put OR look != look_put "+
		"OR put != '' AND put_at <= ?2 - ?3 AND (want != '"+markDone+"' OR put_at < changed + ?3) "+
		"ORDER BY id DESC LIMIT ?1", n, now, markCheck)
	if err != nil {
		return nil, nil, exit.Database(err)
	}
	defer rows.Close()
	for rows.Next() {
		var thread string
		var due bool
		if err := rows.Scan(&thread, &due); err != nil {
			return nil, nil, exit.Database(err)
		}
		threads, check = append(threads, thread), append(check, due)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, exit.Database(err)
	}
	return threads, check, nil
}

// syncMarks puts on the thread's root the marks that differ from what the
// ledger records fleet put, the state mark also when `check` says so, and
// returns what changed. The state mark is added and the other two taken
// off (fednet counts one not there as taken off), then recorded with the
// time `now`; the eyes mark is added or taken off, then recorded. A
// failure leaves the record as it was, so the next try does that part
// again.
func syncMarks(conn querier, r *reactor, thread string, now int64, check bool) (changed []string, err error) {
	var want, put, look, lookPut string
	err = conn.QueryRow("SELECT "+markWant+", "+recorded("mark:")+", "+lookWant+", "+recorded("look-put:")+
		" FROM threads t WHERE t.thread = ?1", thread).Scan(&want, &put, &look, &lookPut)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, exit.Database(err)
	}
	if want != put || check {
		if err := r.putState(thread, want); err != nil {
			return changed, err
		}
		if want != put {
			if err := record(conn, "mark:"+thread, want, now); err != nil {
				return changed, err
			}
			changed = append(changed, want)
		}
		if err := record(conn, "mark-put:"+thread, "put", now); err != nil {
			return changed, err
		}
	}
	if look != lookPut {
		if err := r.react(thread, markLook, look == "off"); err != nil {
			return changed, err
		}
		if err := record(conn, "look-put:"+thread, look, now); err != nil {
			return changed, err
		}
		changed = append(changed, markLook+" "+look)
	}
	return changed, nil
}

// reactor puts marks through one fednet socket, waiting `pause` before
// each call but its first, so a `watch` run catching up many threads
// stays under Slack's rate limits.
type reactor struct {
	socket string
	pause  time.Duration
	calls  int
}

// markPause is how long `watch` waits between two of its reactions.
const markPause = 100 * time.Millisecond

func (r *reactor) react(thread, name string, remove bool) error {
	if r.calls > 0 {
		time.Sleep(r.pause)
	}
	r.calls++
	return fednet.React(r.socket, thread, name, remove)
}

// putState adds the state mark `want` to the thread's root and takes the
// other two off.
func (r *reactor) putState(thread, want string) error {
	if err := r.react(thread, want, false); err != nil {
		return err
	}
	for _, other := range stateMarks {
		if other == want {
			continue
		}
		if err := r.react(thread, other, true); err != nil {
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
	if _, err := syncMarks(conn, &reactor{socket: socket}, thread, db.Now(), false); err != nil {
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
// stale (a failed update, or a change no command marks: a session `watch`
// itself ended, a question it closed, a home thread it opened) and puts
// again the state marks due (markCheck). A failure is a warning and
// leaves the run's exit code as it is.
func (r *watchRun) marks() error {
	threads, check, err := staleMarks(r.conn, marksPerRun, r.now)
	if err != nil {
		return err
	}
	react := &reactor{socket: r.cfg.FednetSocket, pause: markPause}
	for i, thread := range threads {
		if r.outOfTime() {
			return nil
		}
		changed, err := syncMarks(r.conn, react, thread, r.now, check[i])
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

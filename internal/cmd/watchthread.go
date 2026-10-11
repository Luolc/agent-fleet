// The thread rules of `fleet watch`: sessions that broke off or went idle,
// quiet threads, and questions nobody answers (docs/design.md).

package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/Luolc/agent-fleet/internal/atb"
	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/fednet"
)

// reclaimGrace is how long a thread agent asked to end its session has
// before watch ends it; leadGrace how long a lead told that its question
// expired has before watch ends its job.
const (
	reclaimGrace int64 = 10 * 60
	leadGrace    int64 = 30 * 60
)

// The closing lines of a session watch ends: reclaimed, and broken off
// (its agent gone from herdr).
const (
	reclaimedLine = "会话长时间没有动静，已被回收，再说话会重新开始"
	brokenLine    = "会话意外中断了，再说话会重新开始"
)

// question is a pending question as watch reads it. moved is a question
// of a job that is no longer open: it stays pending as the thread's,
// unless dropped: closed as on watch's own forced end, because the job
// ended after watch told its lead that a question expired, or because it
// is a screen question, whose agent went with the job.
type question struct {
	id                 int64
	job, askedBy, text string
	askedAt            int64
	reminders          int
	moved, dropped     bool
}

// screen is whether the question is about an agent stopped at a screen,
// asked under its helper's name: it is reminded of, but never times out,
// since a screen waiting for a person is not a job or session gone stale.
func (q question) screen() bool {
	return strings.HasPrefix(q.askedBy, helperPrefix)
}

// session is a thread's live thread agent.
type session struct {
	name      string
	reclaimAt sql.NullInt64
	inHerdr   bool
}

// watchedJob is an open job reporting to a thread.
type watchedJob struct {
	job, parent string
	reclaimAt   sql.NullInt64
	limits      config.Watch
}

// watchedThread is what one thread looks like this run.
type watchedThread struct {
	key string
	// lastTS is the newest message's Slack timestamp, lastAt the same in
	// seconds; quietAsked the lastTS watch last asked about; lastUser the
	// Slack user of the latest message a person posted, as `inbox`
	// delivered it.
	lastTS, quietAsked, lastUser string
	lastAt                       int64
	// agent is the live session, nil when none.
	agent *session
	// questions are pending, of the thread agent or an open job, oldest
	// first.
	questions []question
	jobs      []*watchedJob
	// unread is why fednet could not give this thread (it answered for
	// others): its rules are skipped this run.
	unread error
}

// readThreads reads every thread watch may act on: one with a live
// thread agent, an open job reporting to it or a pending question. on is
// false without a fednet socket: the thread rules are off. fednet that
// cannot be reached or is busy fails the read; one thread fednet answers
// it cannot give is kept with `unread` set.
func readThreads(conn *sql.DB, cfg *config.Scope, inHerdr map[string]InHerdr, limits map[string]config.Watch) (
	threads []*watchedThread, on bool, err error) {
	if cfg.FednetSocket == "" {
		return nil, false, nil
	}
	byKey := map[string]*watchedThread{}
	get := func(key string) *watchedThread {
		if byKey[key] == nil {
			byKey[key] = &watchedThread{key: key}
		}
		return byKey[key]
	}
	err = eachRow(conn, "SELECT name, thread, reclaim_at FROM agents WHERE role = 'thread' AND state = 'active' AND thread != ''",
		func(rows *sql.Rows) error {
			var s session
			var key string
			if err := rows.Scan(&s.name, &key, &s.reclaimAt); err != nil {
				return err
			}
			_, s.inHerdr = inHerdr[s.name]
			get(key).agent = &s
			return nil
		})
	if err != nil {
		return nil, false, err
	}
	err = eachRow(conn, "SELECT job, parent_issue, home_thread, reclaim_at FROM jobs WHERE state = 'open' AND home_thread != '' ORDER BY id",
		func(rows *sql.Rows) error {
			j := &watchedJob{}
			var key string
			if err := rows.Scan(&j.job, &j.parent, &key, &j.reclaimAt); err != nil {
				return err
			}
			j.limits = limits[j.job]
			t := get(key)
			t.jobs = append(t.jobs, j)
			return nil
		})
	if err != nil {
		return nil, false, err
	}
	if err := readQuestions(conn, get); err != nil {
		return nil, false, err
	}
	for _, t := range byKey {
		threads = append(threads, t)
	}
	slices.SortFunc(threads, func(a, b *watchedThread) int { return strings.Compare(a.key, b.key) })
	for _, t := range threads {
		if err := conn.QueryRow("SELECT quiet_asked, last_user FROM threads WHERE thread = ?1", t.key).Scan(&t.quietAsked, &t.lastUser); err != nil &&
			!errors.Is(err, sql.ErrNoRows) {
			return nil, false, exit.Database(err)
		}
		t.lastTS, t.lastAt, t.unread = fednet.Latest(cfg.FednetSocket, t.key)
		if t.unread != nil && !errors.Is(t.unread, fednet.ErrThread) {
			return nil, false, t.unread
		}
	}
	return threads, true, nil
}

// readQuestions adds each pending question to its thread, oldest first.
// A job's name is unique only among open jobs, so a question belongs to
// the open job of its name only when asked after that job started.
func readQuestions(conn *sql.DB, get func(string) *watchedThread) error {
	return eachRow(conn, "SELECT id, job, thread, asked_by, text, asked_at, reminders, job != '' AND NOT EXISTS "+
		"(SELECT 1 FROM jobs j WHERE j.job = q.job AND j.state = 'open' AND j.started_at <= q.asked_at), "+
		"EXISTS (SELECT 1 FROM jobs j WHERE j.job = q.job AND j.state = 'ended' AND j.reclaim_at IS NOT NULL "+
		"AND j.started_at <= q.asked_at AND j.ended_at >= q.asked_at) "+
		"FROM questions q WHERE state = 'pending' ORDER BY asked_at, id",
		func(rows *sql.Rows) error {
			var q question
			var key string
			if err := rows.Scan(&q.id, &q.job, &key, &q.askedBy, &q.text, &q.askedAt, &q.reminders, &q.moved, &q.dropped); err != nil {
				return err
			}
			q.dropped = q.dropped || q.moved && q.screen()
			t := get(key)
			t.questions = append(t.questions, q)
			return nil
		})
}

// eachRow runs `query` and hands each row to `scan`.
func eachRow(conn *sql.DB, query string, scan func(*sql.Rows) error) error {
	rows, err := conn.Query(query)
	if err != nil {
		return exit.Database(err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return exit.Database(err)
		}
	}
	if err := rows.Err(); err != nil {
		return exit.Database(err)
	}
	return nil
}

// threads runs the thread rules: first the dropped questions are closed,
// then each thread in key order.
func (r *watchRun) threads(threads []*watchedThread, all []watched) error {
	var dropped []question
	for _, t := range threads {
		t.questions = slices.DeleteFunc(t.questions, func(q question) bool {
			if q.dropped {
				dropped = append(dropped, q)
			}
			return q.dropped
		})
	}
	if len(dropped) > 0 {
		if err := r.closeQuestions(dropped); err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "closed %d pending question(s) of ended jobs: a screen's, or the job ended after its lead "+
			"was told a question expired\n", len(dropped))
	}
	for _, t := range threads {
		if r.outOfTime() {
			break
		}
		if t.unread != nil {
			r.failed(fmt.Errorf("thread %s skipped: %w", t.key, t.unread))
			continue
		}
		if err := r.thread(t, all); err != nil {
			r.failed(fmt.Errorf("thread %s: %w", t.key, err))
		}
	}
	return nil
}

// thread runs the rules on one thread, in order: the session that broke
// off or is due to be closed, the questions, the idle session, the quiet
// thread. The first failure stops the thread's rules for this run.
func (r *watchRun) thread(t *watchedThread, all []watched) error {
	w := r.cfg.Watch
	if a := t.agent; a != nil && !a.inHerdr {
		if err := r.endBroken(t); err != nil {
			return err
		}
		t.agent = nil
	}
	if a := t.agent; a != nil && a.reclaimAt.Valid && r.now >= a.reclaimAt.Int64 {
		if err := r.endReclaimed(t); err != nil {
			return err
		}
		t.agent = nil
	}
	if err := r.questions(t, all); err != nil {
		return err
	}
	silent := r.now - t.lastAt
	if a := t.agent; a != nil && !a.reclaimAt.Valid && silent >= secs(w.ThreadIdle) {
		why := fmt.Sprintf("thread %s has had no message for %s (the last at %s)", t.key, Duration(silent), at(t.lastAt))
		if err := r.askToEnd(t, "thread idle", why); err != nil {
			return err
		}
	}
	if len(t.questions) > 0 || t.lastTS == t.quietAsked || silent < secs(w.ThreadQuiet) ||
		t.agent != nil && t.agent.reclaimAt.Valid {
		return nil
	}
	if len(t.jobs) > 0 {
		return r.askProgress(t, all)
	}
	if t.agent != nil {
		return r.askWhyLive(t)
	}
	return nil
}

// questions runs the question rules: a thread agent's question or a
// question moved from an ended job is closed past its limit; until then a
// moved one is told to the live thread agent once; a lead's past its
// limit gets the lead told, then its job ended; what is left is reminded
// of.
func (r *watchRun) questions(t *watchedThread, all []watched) error {
	if err := r.expire(t); err != nil {
		return err
	}
	if err := r.tellMoved(t); err != nil {
		return err
	}
	for _, j := range slices.Clone(t.jobs) {
		if err := r.leadQuestion(t, j, all); err != nil {
			return err
		}
	}
	return r.remind(t)
}

// expire closes the thread's questions pending for `thread_question`: the
// thread agent's own, with its session reclaimed, and those moved from an
// ended job, with the session kept, since it may be doing other work.
func (r *watchRun) expire(t *watchedThread) error {
	var expired, givenUp []question
	for _, q := range t.questions {
		if q.screen() || r.now-q.askedAt < secs(r.cfg.Watch.ThreadQuestion) {
			continue
		}
		if q.job == "" {
			expired = append(expired, q)
		} else if q.moved {
			givenUp = append(givenUp, q)
		}
	}
	if len(expired) > 0 {
		// Asked first: a failed ask leaves the question pending, so the
		// next run asks again.
		q := expired[0]
		if a := t.agent; a != nil && !a.reclaimAt.Valid {
			why := fmt.Sprintf("your question in thread %s has had no answer for %s (asked at %s), so fleet closed it: %s",
				t.key, Duration(r.now-q.askedAt), at(q.askedAt), WorkOrderTitle(q.text))
			if err := r.askToEnd(t, "thread question timeout", why); err != nil {
				return err
			}
		}
	}
	gone := slices.Concat(expired, givenUp)
	if len(gone) == 0 {
		return nil
	}
	if err := r.closeQuestions(gone); err != nil {
		return err
	}
	t.questions = slices.DeleteFunc(t.questions, func(q question) bool { return slices.Contains(gone, q) })
	if len(givenUp) > 0 {
		fmt.Fprintf(os.Stdout, "closed %d question(s) of ended jobs in thread %s: no answer for %s\n", len(givenUp), t.key,
			Duration(secs(r.cfg.Watch.ThreadQuestion)))
	}
	return nil
}

// leadQuestion runs the lead-question rule on one job.
func (r *watchRun) leadQuestion(t *watchedThread, j *watchedJob, all []watched) error {
	i := slices.IndexFunc(t.questions, func(q question) bool { return q.job == j.job && !q.moved && !q.screen() })
	expired := i >= 0 && r.now-t.questions[i].askedAt >= secs(j.limits.LeadQuestion)
	switch {
	case !expired && j.reclaimAt.Valid:
		// The question was answered after the lead was told.
		_, err := r.conn.Exec("UPDATE jobs SET reclaim_at = NULL WHERE job = ?1 AND state = 'open'", j.job)
		return dbErr(err)
	case !expired:
		return nil
	case !j.reclaimAt.Valid:
		q := t.questions[i]
		deadline := r.now + leadGrace
		text := fmt.Sprintf("fleet watch, rule `lead question timeout`: your question in thread %s has had no answer "+
			"for %s (asked at %s): %s\n\nJob %s is ended by force at %s, %s from now, unless you end it first: write "+
			"a report (what is done, what is abandoned, how to pick it up later, which questions need a person), "+
			"clean up your temporary files and worktrees, and end the job with `fleet job end --report-file <file> "+
			"--abandon`.\n", t.key, Duration(r.now-q.askedAt), at(q.askedAt), WorkOrderTitle(q.text), j.job,
			at(deadline), Duration(leadGrace))
		if lead := leadOf(all, j.job); lead != nil {
			if err := r.send(lead.live.Name, text); err != nil {
				return err
			}
		} else {
			fmt.Fprintf(os.Stdout, "job %s has no live lead to tell that its question expired\n", j.job)
		}
		_, err := r.conn.Exec("UPDATE jobs SET reclaim_at = ?1 WHERE job = ?2 AND state = 'open'", deadline, j.job)
		return dbErr(err)
	case r.now < j.reclaimAt.Int64:
		return nil
	}
	askedAt := t.questions[i].askedAt
	ended, pending, err := reclaimJob(r.h, r.conn, r.cfg, j.job)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "reclaimed job %s: its lead's question had no answer; %d rows ended\n", j.job, ended)
	var gone []question
	t.questions = slices.DeleteFunc(t.questions, func(q question) bool {
		if q.job == j.job && !q.moved {
			gone = append(gone, q)
		}
		return q.job == j.job && !q.moved
	})
	t.jobs = slices.DeleteFunc(t.jobs, func(o *watchedJob) bool { return o == j })
	if err := r.closeQuestions(gone); err != nil {
		return err
	}
	text := fmt.Sprintf("Job %s was ended by force: its lead's question had no answer for %s, and the job was not "+
		"ended within %s of the lead being told.", j.job, Duration(r.now-askedAt), Duration(leadGrace))
	if len(pending) > 0 {
		text += " Linear steps not done:\n- " + strings.Join(pending, "\n- ")
	}
	return fednet.Post(r.cfg.FednetSocket, t.key, text)
}

// tellMoved tells the live thread agent, unless it is being reclaimed,
// about the questions moved to its thread that no agent was told of yet.
// Without one nobody is started: the reminders still reach the people,
// and the agent their answer starts reads the thread. A delivery that
// surely did not arrive (blocked, not found) is tried again next run.
func (r *watchRun) tellMoved(t *watchedThread) error {
	if t.agent == nil || t.agent.reclaimAt.Valid {
		return nil
	}
	var moved []question
	for _, q := range t.questions {
		if q.moved && !stepDone(r.conn, movedKey(q), "tell") {
			moved = append(moved, q)
		}
	}
	if len(moved) == 0 {
		return nil
	}
	var text strings.Builder
	fmt.Fprintf(&text, "fleet watch, rule `questions of an ended job`: %d question(s) a lead asked in thread %s are still "+
		"pending, but the job is no longer open. They are this thread's questions now, and yours to follow: the "+
		"people are reminded of them as before, and a person's next message in the thread answers them; with the "+
		"job gone, acting on the answer is yours (a follow-up job, say). `fleet thread end` waits for them as for "+
		"your own; one still pending %s after it was asked is closed, and your session is not reclaimed for it.\n",
		len(moved), t.key, Duration(secs(r.cfg.Watch.ThreadQuestion)))
	for _, q := range moved {
		fmt.Fprintf(&text, "\n- job %s, from %s, asked at %s:\n", q.job, q.askedBy, at(q.askedAt))
		for _, line := range firstLines(q.text, 3) {
			fmt.Fprintf(&text, "    %s\n", line)
		}
	}
	body, err := WithHeader(watchSender, text.String())
	if err != nil {
		return err
	}
	code, err := Deliver(r.h, t.agent.name, body)
	if err != nil {
		return err
	}
	r.got(code)
	if code != exit.Ok && code != exit.Unknown {
		return nil
	}
	for _, q := range moved {
		if _, err := r.conn.Exec("INSERT OR IGNORE INTO steps (key, step, done_at) VALUES (?1, 'tell', ?2)",
			movedKey(q), db.Now()); err != nil {
			return exit.Database(err)
		}
	}
	fmt.Fprintf(os.Stdout, "told %s of %d question(s) of ended jobs in thread %s\n", t.agent.name, len(moved), t.key)
	return nil
}

// movedKey is the steps key of a question moved to its thread.
func movedKey(q question) string {
	return fmt.Sprintf("watch-moved:%d", q.id)
}

// firstLines are the first `n` non-blank lines of `text`.
func firstLines(text string, n int) []string {
	var kept []string
	for _, line := range lines(text) {
		if strings.TrimSpace(line) != "" && len(kept) < n {
			kept = append(kept, line)
		}
	}
	return kept
}

// remind posts one reminder of the thread's pending questions when the
// oldest has passed more of `reminders` than it was reminded of,
// mentioning the person who last wrote in the thread, then deletes the
// reminders it replaces.
func (r *watchRun) remind(t *watchedThread) error {
	if err := r.postReminder(t); err != nil {
		return err
	}
	return r.deleteStale(t)
}

func (r *watchRun) postReminder(t *watchedThread) error {
	if len(t.questions) == 0 {
		return nil
	}
	oldest := t.questions[0]
	due := 0
	for _, d := range r.cfg.Watch.Reminders {
		if r.now-oldest.askedAt >= secs(d) {
			due++
		}
	}
	if due <= oldest.reminders {
		return nil
	}
	var text strings.Builder
	fmt.Fprintf(&text, "Reminder %d of %d: %d question(s) here still wait for an answer, the first asked %s ago:\n",
		due, len(r.cfg.Watch.Reminders), len(t.questions), Duration(r.now-oldest.askedAt))
	ids := make([]any, 0, len(t.questions))
	for _, q := range t.questions {
		fmt.Fprintf(&text, "- from %s: %s\n", q.askedBy, WorkOrderTitle(q.text))
		ids = append(ids, q.id)
	}
	body := strings.TrimRight(text.String(), "\n")
	var mentions []string
	if t.lastUser != "" {
		mentions = []string{t.lastUser}
	}
	msg, err := fednet.PostID(r.cfg.FednetSocket, t.key, body, mentions...)
	if errors.Is(err, fednet.ErrMention) {
		// The reminder matters more than the mention.
		fmt.Fprintf(os.Stderr, "fleet watch: the reminder in thread %s is posted without mentioning anyone: %v\n", t.key, err)
		msg, err = fednet.PostID(r.cfg.FednetSocket, t.key, body)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "reminded thread %s of %d question(s) (%d of %d)\n", t.key, len(t.questions), due,
		len(r.cfg.Watch.Reminders))
	tx, err := r.conn.Begin()
	if err != nil {
		return exit.Database(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []struct {
		query string
		args  []any
	}{
		{"UPDATE reminders SET state = 'stale' WHERE thread = ?1 AND state = 'posted'", []any{t.key}},
		{"INSERT INTO reminders (msg_id, thread, state) VALUES (?1, ?2, 'posted')", []any{msg, t.key}},
		{"UPDATE questions SET reminders = ?1, reminder_msg = ?2 WHERE id IN (?" + strings.Repeat(", ?", len(ids)-1) + ")",
			append([]any{due, msg}, ids...)},
	} {
		if _, err := tx.Exec(stmt.query, stmt.args...); err != nil {
			return exit.Database(err)
		}
	}
	return dbErr(tx.Commit())
}

// deleteStale deletes from the thread the reminders a newer one replaced.
// A failure is said on stderr and stops no other rule: a reminder fednet
// no longer has (exit 1) counts as deleted; one not in Slack yet (5), or
// with the hub unreachable (4) or fednet not run, is tried again on the
// next run; any other refusal (a hub older than its client, say) leaves
// it in the thread for good.
func (r *watchRun) deleteStale(t *watchedThread) error {
	stale, err := staleReminders(r.conn, t.key)
	if err != nil {
		return err
	}
	for _, msg := range stale {
		code, reason, err := fednet.Delete(r.cfg.FednetSocket, msg)
		state := ""
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "fleet watch: the reminder %s in thread %s is not deleted, the next run tries again: %v\n",
				msg, t.key, err)
		case code == 0:
			state = "deleted"
			fmt.Fprintf(os.Stdout, "deleted the reminder %s in thread %s: a newer one replaces it\n", msg, t.key)
		case code == 1:
			state = "deleted"
			fmt.Fprintf(os.Stdout, "the reminder %s in thread %s was gone already\n", msg, t.key)
		case code == 4 || code == 5:
			fmt.Fprintf(os.Stderr, "fleet watch: the reminder %s in thread %s is not deleted, the next run tries again: "+
				"fednet client delete exit status %d: %s\n", msg, t.key, code, reason)
		default:
			state = "kept"
			fmt.Fprintf(os.Stderr, "fleet watch: the reminder %s in thread %s stays in the thread: fednet client delete "+
				"exit status %d: %s\n", msg, t.key, code, reason)
		}
		if state == "" {
			continue
		}
		if _, err := r.conn.Exec("UPDATE reminders SET state = ?1 WHERE msg_id = ?2", state, msg); err != nil {
			return exit.Database(err)
		}
	}
	return nil
}

// staleReminders are the msg_ids of the thread's reminders to delete,
// oldest first.
func staleReminders(conn *sql.DB, thread string) ([]string, error) {
	rows, err := conn.Query("SELECT msg_id FROM reminders WHERE thread = ?1 AND state = 'stale' ORDER BY id", thread)
	if err != nil {
		return nil, exit.Database(err)
	}
	defer rows.Close()
	var msgs []string
	for rows.Next() {
		var msg string
		if err := rows.Scan(&msg); err != nil {
			return nil, exit.Database(err)
		}
		msgs = append(msgs, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, exit.Database(err)
	}
	return msgs, nil
}

// closeQuestions marks the questions closed: given up on, not answered.
func (r *watchRun) closeQuestions(qs []question) error {
	for _, q := range qs {
		if _, err := r.conn.Exec("UPDATE questions SET state = 'closed' WHERE id = ?1 AND state = 'pending'", q.id); err != nil {
			return exit.Database(err)
		}
	}
	return nil
}

// askToEnd asks the live thread agent to end its session, and records
// when watch ends it otherwise.
func (r *watchRun) askToEnd(t *watchedThread, rule, why string) error {
	a := t.agent
	text := fmt.Sprintf("fleet watch, rule `%s`: %s. This session is reclaimed: write your summary and end it with "+
		"`fleet thread end --summary-file <file> --asked-to-end` (being reclaimed counts as being asked to end) within "+
		"%s. After that fleet ends it without a summary, closes this tab and posts the closing line; the next "+
		"message in the thread starts a new session.\n", rule, why, Duration(reclaimGrace))
	if err := r.send(a.name, text); err != nil {
		return err
	}
	a.reclaimAt = sql.NullInt64{Int64: r.now + reclaimGrace, Valid: true}
	_, err := r.conn.Exec("UPDATE agents SET reclaim_at = ?1 WHERE name = ?2 AND state = 'active'", a.reclaimAt.Int64, a.name)
	return dbErr(err)
}

// askProgress asks the thread agent, started for it when none is live,
// whether the thread's open jobs have progress to report.
func (r *watchRun) askProgress(t *watchedThread, all []watched) error {
	var text strings.Builder
	fmt.Fprintf(&text, "fleet watch, rule `thread quiet, jobs open`: thread %s has had no new message for %s (the last "+
		"at %s). Look at the jobs below and decide whether there is progress to report: post it to the thread; ask "+
		"the people when a person is in the way; tell another agent when it is; when a lead is stuck or gone, send "+
		"it a message, ask in the thread, or reclaim the job with `fleet job end <job> --force`. watch asks once "+
		"per quiet spell; a new message in the thread starts the count again.\n\nJobs reporting to this thread:\n",
		t.key, Duration(r.now-t.lastAt), at(t.lastAt))
	for _, j := range t.jobs {
		text.WriteString(jobState(j, all, r.now))
	}
	code, err := toThread(r.h, r.conn, r.scope, r.cfg, inboundMessage{Thread: t.key, Text: text.String(), Watch: true})
	if err != nil {
		return err
	}
	return r.asked(t, code)
}

// askWhyLive asks a live thread agent with nothing open why it has not
// ended its session.
func (r *watchRun) askWhyLive(t *watchedThread) error {
	text := fmt.Sprintf("fleet watch, rule `thread quiet, nothing open`: thread %s has had no new message for %s "+
		"(the last at %s), no question in it waits for a person and no job reports to it, yet this session is "+
		"live. Why has it not ended, and what is left to do? When nothing is, end it with `fleet thread end "+
		"--summary-file <file>`. watch does not ask again in this quiet spell, and reclaims a session whose "+
		"thread has had no message for %s.\n", t.key, Duration(r.now-t.lastAt), at(t.lastAt),
		Duration(secs(r.cfg.Watch.ThreadIdle)))
	text, err := WithHeader(watchSender, text)
	if err != nil {
		return err
	}
	code, err := Deliver(r.h, t.agent.name, text)
	if err != nil {
		return err
	}
	return r.asked(t, code)
}

// asked records that watch asked about this quiet spell, unless the
// question surely did not arrive (blocked, not found): then the next run
// asks again.
func (r *watchRun) asked(t *watchedThread, code exit.Code) error {
	r.got(code)
	if code != exit.Ok && code != exit.Unknown {
		return nil
	}
	_, err := r.conn.Exec("UPDATE threads SET quiet_asked = ?1 WHERE thread = ?2", t.lastTS, t.key)
	return dbErr(err)
}

// send delivers `body` from watch to `to`, keeping the exit code.
func (r *watchRun) send(to, body string) error {
	text, err := WithHeader(watchSender, body)
	if err != nil {
		return err
	}
	code, err := Deliver(r.h, to, text)
	if err != nil {
		return err
	}
	r.got(code)
	return nil
}

// endBroken ends the session of a thread agent gone from herdr: the
// ticket released as it was before the claim (not done), the closing line
// saying the session broke off, the row ended and the tab closed, as
// `thread end`'s recorded steps.
func (r *watchRun) endBroken(t *watchedThread) error {
	name := t.agent.name
	e, err := newThreadEnding(r.conn, name, false)
	if err != nil {
		return err
	}
	if e.ticket != "" {
		if err := runStep(r.conn, e.key, "release", func() error {
			return atb.ReleaseClaim(e.ticket, name, "the session ended abnormally: its agent is gone from herdr")
		}); err != nil {
			return err
		}
	}
	if err := r.closeSession(e, name, closingLine(brokenLine, e.ticket, e.ticketURL)); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "ended the session of %s on thread %s: its agent is gone from herdr\n", name, t.key)
	return nil
}

// endReclaimed ends the session of a thread agent asked to end it that
// did not: `thread end`'s steps with a summary saying none was left.
func (r *watchRun) endReclaimed(t *watchedThread) error {
	name := t.agent.name
	e, err := newThreadEnding(r.conn, name, false)
	if err != nil {
		return err
	}
	if err := e.linearSteps(name, e.ticket, "The session was reclaimed by fleet watch; the agent left no summary."); err != nil {
		return err
	}
	if err := r.closeSession(e, name, closingLine(reclaimedLine, e.ticket, e.ticketURL)); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "reclaimed the session of %s on thread %s\n", name, t.key)
	return nil
}

// closeSession is the end of an ending watch runs: the closing line, the
// row, the tab.
func (r *watchRun) closeSession(e *threadEnding, name, line string) error {
	if err := runStep(r.conn, e.key, "footer", func() error { return fednet.Footer(r.cfg.FednetSocket, e.thread, line) }); err != nil {
		return err
	}
	if err := runStep(r.conn, e.key, "end-row", func() error { return endRow(r.conn, name) }); err != nil {
		return err
	}
	return closeOwnTab(r.h, r.conn, name)
}

// jobState is one job's line in the quiet-thread notice: its lead's state
// and its suspect workers.
func jobState(j *watchedJob, all []watched, now int64) string {
	line := "- job " + j.job
	if j.parent != "" {
		line += " (parent " + j.parent + ")"
	}
	switch lead := leadOf(all, j.job); {
	case lead == nil:
		line += ": no live lead"
	case lead.reading == nil && !lead.suspect:
		line += fmt.Sprintf(": lead %s starting", lead.live.Name)
	case lead.reading == nil:
		line += fmt.Sprintf(": lead %s gone from herdr", lead.live.Name)
	default:
		line += fmt.Sprintf(": lead %s %s, unchanged for %s", lead.live.Name, lead.reading.status,
			Duration(now-*lead.lastChangeAt))
		if lead.suspect {
			line += " (a suspect)"
		}
	}
	var suspects []string
	for _, w := range all {
		if w.live.Role == "worker" && w.live.Job == j.job && w.suspect {
			suspects = append(suspects, w.live.Name)
		}
	}
	if len(suspects) == 0 {
		return line + "; no suspect worker\n"
	}
	return line + "; suspect workers: " + strings.Join(suspects, ", ") + "\n"
}

// leadOf is the job's live lead as watched this run, or nil.
func leadOf(all []watched, job string) *watched {
	for i := range all {
		if all[i].live.Role == "lead" && all[i].live.Job == job {
			return &all[i]
		}
	}
	return nil
}

func secs(d time.Duration) int64 {
	return int64(d / time.Second)
}

// at is a time as the notices show it.
func at(secs int64) string {
	return time.Unix(secs, 0).UTC().Format("2006-01-02 15:04 UTC")
}

func dbErr(err error) error {
	if err != nil {
		return exit.Database(err)
	}
	return nil
}

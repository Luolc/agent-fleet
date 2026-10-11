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

// question is a pending question as watch reads it.
type question struct {
	id                 int64
	job, askedBy, text string
	askedAt            int64
	reminders          int
}

// byHelper is whether an agent watch started asked the question: a
// screen helper about an agent stopped at a screen, or a revisit agent
// about a parent issue. It is reminded of, but never times out, since
// what waits for a person there is not a job or session gone stale.
func (q question) byHelper() bool {
	return helperNamed(q.askedBy)
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
	// seconds; quietAsked the lastTS watch last asked about.
	lastTS, quietAsked string
	lastAt             int64
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
	err = eachRow(conn, "SELECT id, job, thread, asked_by, text, asked_at, reminders FROM questions WHERE state = 'pending' "+
		"AND (job = '' OR job IN (SELECT job FROM jobs WHERE state = 'open')) ORDER BY asked_at, id",
		func(rows *sql.Rows) error {
			var q question
			var key string
			if err := rows.Scan(&q.id, &q.job, &key, &q.askedBy, &q.text, &q.askedAt, &q.reminders); err != nil {
				return err
			}
			t := get(key)
			t.questions = append(t.questions, q)
			return nil
		})
	if err != nil {
		return nil, false, err
	}
	for _, t := range byKey {
		threads = append(threads, t)
	}
	slices.SortFunc(threads, func(a, b *watchedThread) int { return strings.Compare(a.key, b.key) })
	for _, t := range threads {
		if err := conn.QueryRow("SELECT quiet_asked FROM threads WHERE thread = ?1", t.key).Scan(&t.quietAsked); err != nil &&
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

// eachRow runs `query` and hands each row to `scan`.
func eachRow(conn *sql.DB, query string, scan func(*sql.Rows) error, args ...any) error {
	rows, err := conn.Query(query, args...)
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

// threads runs the thread rules: first the pending questions of jobs that
// are no longer open are closed, then each thread in key order.
func (r *watchRun) threads(threads []*watchedThread, all []watched) error {
	res, err := r.conn.Exec("UPDATE questions SET state = 'closed' WHERE state = 'pending' AND job != '' " +
		"AND job NOT IN (SELECT job FROM jobs WHERE state = 'open')")
	if err != nil {
		return exit.Database(err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		fmt.Fprintf(os.Stdout, "closed %d pending question(s) of jobs that are not open\n", n)
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

// questions runs the question rules: a thread agent's question past its
// limit is closed and its session reclaimed; a lead's past its limit gets
// the lead told, then its job ended; what is left is reminded of.
func (r *watchRun) questions(t *watchedThread, all []watched) error {
	var expired, left []question
	for _, q := range t.questions {
		if q.job == "" && !q.byHelper() && r.now-q.askedAt >= secs(r.cfg.Watch.ThreadQuestion) {
			expired = append(expired, q)
		} else {
			left = append(left, q)
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
		if err := r.closeQuestions(expired); err != nil {
			return err
		}
		t.questions = left
	}
	for _, j := range slices.Clone(t.jobs) {
		if err := r.leadQuestion(t, j, all); err != nil {
			return err
		}
	}
	return r.remind(t)
}

// leadQuestion runs the lead-question rule on one job.
func (r *watchRun) leadQuestion(t *watchedThread, j *watchedJob, all []watched) error {
	i := slices.IndexFunc(t.questions, func(q question) bool { return q.job == j.job && !q.byHelper() })
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
		if q.job == j.job {
			gone = append(gone, q)
		}
		return q.job == j.job
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

// remind posts one reminder of the thread's pending questions when the
// oldest has passed more of `reminders` than it was reminded of.
func (r *watchRun) remind(t *watchedThread) error {
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
	msg, err := fednet.PostID(r.cfg.FednetSocket, t.key, strings.TrimRight(text.String(), "\n"))
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "reminded thread %s of %d question(s) (%d of %d)\n", t.key, len(t.questions), due,
		len(r.cfg.Watch.Reminders))
	_, err = r.conn.Exec("UPDATE questions SET reminders = ?1, reminder_msg = ?2 WHERE id IN (?"+
		strings.Repeat(", ?", len(ids)-1)+")", append([]any{due, msg}, ids...)...)
	return dbErr(err)
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

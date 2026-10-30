// What the watch `model-changes` does about a session that, read to its
// last complete line, is now stopped by a safety classifier or on another
// model than it started on (docs/design.md): a lead or worker gets a new
// session of the same name, on the model and effort its start recorded; a
// thread agent stopped on its model is told once to go on; a thread agent
// moved to another model gets the thread's next session, as `inbox` starts
// one. Each is done once (per row, or per thread for a thread's new
// session); after that the people decide. fleet reads none of the
// conversation for this, never switches a model and types into a session
// only its own messages.

package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/Luolc/agent-fleet/internal/atb"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/fednet"
	"github.com/Luolc/agent-fleet/internal/identity"
)

// listedFiles bounds the agent's files a takeover note names; fileShape
// is a file name it may name, since any agent can name its files.
var (
	listedFiles = 20
	fileShape   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)
)

// actOnSession decides what to do about the agent row `id` from its session as
// this run read it to its last complete line: stopped (its latest
// assistant message refused, with no reply after it) or moved (on
// another model than it started on, a variant such as `[1m]` counting as
// the same model); a session that is neither has nothing done, and the
// events of its past are only reported. A stop or move is decided once:
// the session's `decided_at` is set, and cleared once the session is
// neither again, as it is when this run read a line after which it was
// neither. Nothing is decided while the agent is busy (herdr's
// status not idle or done, the row not active): the next run looks
// again. `status` is what herdr says of the agent.
func (r *watchRun) actOnSession(id int64, read *sessionRead, status InHerdr) error {
	moved := read.st.moved()
	if !read.st.stopped && !moved {
		if !read.decided {
			return nil
		}
		_, err := r.conn.Exec("UPDATE transcripts SET decided_at = NULL WHERE agent = ?1 AND session = ?2", id,
			read.session)
		return dbErr(err)
	}
	// A stop or move after the session was neither, in what this run
	// read, is a new one.
	if read.decided && !read.st.recovered {
		return nil
	}
	a, err := loadAgent(r.conn, id)
	if err != nil {
		return err
	}
	x := modelAction{why: "stopped", from: reportable(read.st.startModel), to: reportable(read.st.lastModel)}
	if moved {
		x.why = "moved"
	}
	// A thread's session is replaced once per thread, so each new session
	// is not replaced again; a thread agent stopped on its model is told
	// to go on once per session, its row.
	handled := a.takeovers
	if a.role == "thread" && moved {
		if handled, err = threadTakeovers(r.conn, a.thread); err != nil {
			return err
		}
	}
	switch {
	case a.role != "thread" && a.job == "":
		_, err := r.decide(a, read, x, actionReported, "", false)
		return err
	case handled > 0:
		_, err := r.decide(a, read, x, actionHeld, "", false)
		return err
	case a.state != "active" || status.Status != "idle" && status.Status != "done":
		return nil
	case a.role == "thread" && moved:
		return r.replaceThread(a, read, x)
	case a.role == "thread":
		return r.nudge(a, read, x)
	}
	return r.takeOver(a, read, x)
}

// threadTakeovers is how many times the watch replaced the session of
// `thread`.
func threadTakeovers(conn *sql.DB, thread string) (int, error) {
	var n int
	err := conn.QueryRow("SELECT takeovers FROM threads WHERE thread = ?1", thread).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, dbErr(err)
}

// decide records the action `x` with `action` and `detail` about the
// session of `a` read in `read`, marks its stop or move decided and, with
// `count`, counts one more takeover (on the thread for a thread agent
// moved to another model, else on the row), in one transaction; it
// returns the action's id.
func (r *watchRun) decide(a *eventAgent, read *sessionRead, x modelAction, action, detail string, count bool) (int64, error) {
	tx, err := r.conn.Begin()
	if err != nil {
		return 0, exit.Database(err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec("INSERT INTO model_actions (agent, session, action, why, from_model, to_model, detail, at) "+
		"VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)", a.id, read.session, action, x.why, x.from, x.to, detail, r.now)
	if err != nil {
		return 0, exit.Database(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, exit.Database(err)
	}
	if _, err := tx.Exec("UPDATE transcripts SET decided_at = ?1 WHERE agent = ?2 AND session = ?3", r.now, a.id,
		read.session); err != nil {
		return 0, exit.Database(err)
	}
	if count && x.why == "moved" && a.role == "thread" {
		if _, err := tx.Exec("UPDATE threads SET takeovers = takeovers + 1 WHERE thread = ?1", a.thread); err != nil {
			return 0, exit.Database(err)
		}
	} else if count {
		if _, err := tx.Exec("UPDATE agents SET takeovers = takeovers + 1 WHERE id = ?1", a.id); err != nil {
			return 0, exit.Database(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, exit.Database(err)
	}
	return id, nil
}

// recentEvents are the latest events recorded for the agent row `id`, as
// the agents read them, oldest first.
func (r *watchRun) recentEvents(id int64) ([]string, error) {
	var lines []string
	err := eachRow(r.conn, "SELECT kind, at, from_model, to_model, start_model, uuid FROM (SELECT * FROM model_events "+
		"WHERE agent = ?1 ORDER BY id DESC LIMIT ?2) ORDER BY id",
		func(rows *sql.Rows) error {
			var ev modelEvent
			if err := rows.Scan(&ev.Kind, &ev.At, &ev.From, &ev.To, &ev.StartModel, &ev.UUID); err != nil {
				return err
			}
			lines = append(lines, ev.line())
			return nil
		}, id, listedEvents)
	return lines, err
}

// nudge tells the thread agent `a`, stopped on its model, to go on, as
// `send` would as `watch`: a message, never a command. Counted once it
// landed (exit 0, or 2: it may have); a blocked or missing agent is told
// next run.
func (r *watchRun) nudge(a *eventAgent, read *sessionRead, x modelAction) error {
	jobs, err := openJobsOf(r, a.thread)
	if err != nil {
		return err
	}
	events, err := r.recentEvents(a.id)
	if err != nil {
		return err
	}
	text, err := WithHeader(watchSender, nudgeMessage(x, events, jobs))
	if err != nil {
		return err
	}
	got, err := Deliver(r.h, a.name, text)
	if err != nil {
		r.failed(err)
		return nil
	}
	r.got(got)
	if got != exit.Ok && got != exit.Unknown {
		return nil
	}
	_, err = r.decide(a, read, x, actionNudged, "", true)
	return err
}

// openJobsOf are the open jobs whose home thread is `thread`, oldest
// first.
func openJobsOf(r *watchRun, thread string) ([]string, error) {
	var jobs []string
	err := eachRow(r.conn, "SELECT job FROM jobs WHERE home_thread = ?1 AND state = 'open' ORDER BY id",
		func(rows *sql.Rows) error {
			var job string
			if err := rows.Scan(&job); err != nil {
				return err
			}
			jobs = append(jobs, job)
			return nil
		}, thread)
	return jobs, err
}

// nudgeMessage is what a thread agent stopped on its model is told.
func nudgeMessage(x modelAction, events []string, jobs []string) string {
	var text strings.Builder
	fmt.Fprintf(&text, "fleet watch, watch `model-changes`: %s. The latest model events fleet recorded in your "+
		"transcript:\n", x.whyLine(false))
	for _, line := range events {
		fmt.Fprintf(&text, "- %s\n", line)
	}
	text.WriteString("\nGo on with the thread's work from where it stands")
	if len(jobs) > 0 {
		fmt.Fprintf(&text, " (the open jobs reporting to this thread: %s)", strings.Join(jobs, ", "))
	}
	text.WriteString(". Do not redo the step that was stopped, neither as it was nor in other words: take another " +
		"way, or ask the people in the thread about that step with `fleet ask-human --card`. fleet read none of your " +
		"conversation, only its records' metadata; it changed no session and switched no model. It sends this once " +
		"per session: if your session is stopped again, the people decide.\n")
	return text.String()
}

// takeover is a new session for a lead or worker, ready to start.
type takeover struct {
	id        *identity.Identity
	workspace string
	label     string
	model     *string
	effort    *string
	body      string
}

// takeOver replaces the session of the lead or worker `a`: the same row
// and name, so its claims, its parent and the messages to it stay as they
// are, with a new session started as its start did. The takeover is
// counted and recorded as failed before anything changes, so a run that
// stops mid-way does not try it again, and the next run reports it.
func (r *watchRun) takeOver(a *eventAgent, read *sessionRead, x modelAction) error {
	t, reason := r.takeoverPlan(a, x)
	if reason != "" {
		_, err := r.decide(a, read, x, actionFailed, reason, true)
		return err
	}
	id, err := r.decide(a, read, x, actionFailed, "the takeover did not finish: the watch run stopped before it did",
		true)
	if err != nil {
		return err
	}
	action, detail, err := r.replace(a, t)
	if err != nil {
		return err
	}
	_, err = r.conn.Exec("UPDATE model_actions SET action = ?1, detail = ?2 WHERE id = ?3", action, detail, id)
	return dbErr(err)
}

// replace opens the new session's tab in the job's workspace, closes the
// old session's tab, then starts the agent in the new tab and delivers its
// first message, headed `[FROM: watch]`, as `spawn` does; the tab is
// opened first so a lead's workspace is never left without a tab. A step
// that fails is the action's detail: before the old tab is closed, the old
// session goes on; after it, the row stays `starting` with the new pane,
// as a failed `spawn` leaves it, for the rules on gone agents and stopped
// screens.
func (r *watchRun) replace(a *eventAgent, t *takeover) (action, detail string, err error) {
	place, err := CreateTab(r.h, t.workspace, t.label, a.cwd, t.id)
	if err != nil {
		return actionFailed, "no tab could be opened for a new session, so the old one goes on: " + firstLine(err.Error()), nil
	}
	if err := closePaneTab(r.h, a.pane, a.name); err != nil {
		detail = fmt.Sprintf("the old session's tab (pane %s) could not be closed, so it goes on: %s", a.pane,
			firstLine(err.Error()))
		if err := closeTab(r.h, place.TabID, a.name); err != nil {
			detail += fmt.Sprintf("; the new tab %s is left open too: %s", place.TabID, firstLine(err.Error()))
		}
		return actionFailed, detail, nil
	}
	if _, err := r.conn.Exec("UPDATE agents SET state = 'starting' WHERE id = ?1", a.id); err != nil {
		return "", "", exit.Database(err)
	}
	var created []string
	code, err := startAndDeliver(r.h, r.conn, t.id, place, a.cwd, KindClaude, t.model, t.effort, watchSender, t.body,
		&created)
	switch {
	case err != nil:
		return actionFailed, fmt.Sprintf("the old session (pane %s) was closed, but the new one in pane %s did not "+
			"start: %s; its row is left `starting`", a.pane, place.PaneID, firstLine(err.Error())), nil
	case code != exit.Ok:
		return actionFailed, fmt.Sprintf("the old session (pane %s) was closed and the new one started in pane %s, "+
			"but its first message did not land (exit %d); its row is left `starting`", a.pane, place.PaneID, code), nil
	}
	return actionReplaced, fmt.Sprintf("pane %s closed, the new session in pane %s", a.pane, place.PaneID), nil
}

// takeoverPlan is the new session of `a`, its first message rendered as
// its start rendered it, from the ledger, the scope's config and the
// task file, with a note on the takeover; or why there is none. It
// changes nothing.
func (r *watchRun) takeoverPlan(a *eventAgent, x modelAction) (*takeover, string) {
	if a.kind != KindClaude {
		return nil, "the ledger has no record of the model and effort it was started with (it started before fleet " +
			"recorded them)"
	}
	role, _ := identity.ParseRole(a.role)
	job, err := openJob(r.conn, a.job)
	if err != nil || job == nil {
		return nil, "its job " + a.job + " is not open in the ledger"
	}
	task, _, err := taskFile(a.task, watchSender)
	if err != nil {
		return nil, "its task file cannot be read again: " + firstLine(err.Error())
	}
	if info, err := os.Stat(a.cwd); err != nil || !info.IsDir() {
		return nil, "its directory " + a.cwd + " is gone"
	}
	prompt, err := loadPrompt(r.cfg, a.role)
	if err != nil {
		return nil, "the " + a.role + " prompt cannot be used: " + firstLine(err.Error())
	}
	cfg, err := jobConfig(job.Repo, job.LeadCwd)
	if err != nil {
		return nil, "its job's config cannot be read: " + firstLine(err.Error())
	}
	workspaces, err := WorkspacesLabelled(r.h, a.job)
	if err != nil || len(workspaces) != 1 {
		return nil, fmt.Sprintf("herdr does not have one workspace labelled %s", a.job)
	}
	t := &takeover{workspace: workspaces[0], label: "lead", model: nullable(a.model), effort: nullable(a.effort),
		id: &identity.Identity{Agent: a.name, Role: role, Parent: a.parent, Scope: r.scope, Job: a.job, Issue: a.issue,
			Scratch: jobScratch(r.cfg, job.Repo, job.LeadCwd)}}
	events, err := r.recentEvents(a.id)
	if err != nil {
		return nil, "the ledger's events cannot be read: " + firstLine(err.Error())
	}
	notes := []string{takeoverNote(a, x, events, agentFiles(t.id.Scratch, a.name, r.cfg.Tilde))}
	var section string
	if role == identity.Worker {
		t.label = strings.TrimPrefix(a.name, a.job+"-")
		statute, err := worktreeStatuteNote(r.conn, a.cwd)
		if err != nil {
			return nil, "the ledger's worktrees cannot be read: " + firstLine(err.Error())
		}
		notes = append(notes, statute)
	} else if job.HomeThread == "" && a.parent == plainStarter {
		section = noHomeThreadSection(jobMapping(r.cfg, job.Repo, job.LeadCwd), generalMapping(r.cfg.Channels))
	} else {
		latest, err := latestMessage(r.conn, job.HomeThread)
		if err == nil {
			section, err = latestMessageSection(latest)
		}
		if err != nil {
			return nil, "the home thread's latest message cannot be read: " + firstLine(err.Error())
		}
	}
	issue := atb.Issue{Identifier: a.issue, URL: a.issueURL}
	t.body = prompt.fill(roleSlots(r.cfg, t.id, a.cwd, job.Repo, job.LeadCwd, cfg.MaxAgentsPerJob, issue,
		job.ParentIssue)) + noteSection(append(notes, lightNote(a.light))...) + section + taskSection(issue.URL, task)
	return t, ""
}

// nullable is a recorded --model or --effort: nil when none was passed.
func nullable(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

// agentFiles are the files directly in `dir` whose names hold `name`, as
// an agent names its reports, by name, shown through `show`; their
// contents are not read.
func agentFiles(dir, name string, show func(string) string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && fileShape.MatchString(entry.Name()) && strings.Contains(entry.Name(), name) {
			files = append(files, show(dir+"/"+entry.Name()))
		}
	}
	slices.Sort(files)
	return files[:min(len(files), listedFiles)]
}

// takeoverNote is the part of the new session's `## Note from fleet` on
// the takeover: what happened, from the records' metadata alone, what
// stays the same, where to find how far the work got, and that the step
// that was stopped is not to be redone.
func takeoverNote(a *eventAgent, x modelAction, events, files []string) string {
	var text strings.Builder
	fmt.Fprintf(&text, "This session takes over from an earlier session of %s, which fleet closed: in it, %s. The "+
		"latest model events the watch `model-changes` recorded for %s:\n", a.name, x.whyLine(false), a.name)
	for _, line := range events {
		fmt.Fprintf(&text, "- %s\n", line)
	}
	issue := "none"
	if a.issue != "" {
		issue = a.issue
	}
	fmt.Fprintf(&text, "\nYou are the same agent on the same attempt: the same name, task, work order (%s), "+
		"directory and parent (%s); fleet claimed and released nothing for this, and this session runs on the model "+
		"and effort recorded when %s started. fleet read none of the earlier conversation, only its records' "+
		"metadata, and passes none of it on.\n\n", issue, a.parent, a.name)
	fmt.Fprintf(&text, "Find where the work stands from what is on disk: the task below; your directory %s (read its "+
		"branch, HEAD and `git status` first)", a.cwd)
	if len(files) == 0 {
		text.WriteString("; fleet found no file of yours in FLEET_SCRATCH.\n\n")
	} else {
		fmt.Fprintf(&text, "; your files in FLEET_SCRATCH: %s.\n\n", strings.Join(files, ", "))
	}
	who := "your lead " + a.parent + " with `fleet send " + a.parent + " --file <file>`"
	if a.role == "lead" {
		who = "the people in the home thread with `fleet ask-thread --card <file>`"
	}
	fmt.Fprintf(&text, "Do not redo the step that was stopped, neither as it was nor in other words: take another way "+
		"to the goal, or hand that step to %s. fleet replaces a session once per agent: if this one is stopped or "+
		"moved to another model too, the people decide.", who)
	return text.String()
}

// replacedLine is the closing line of a thread's session the watch
// replaced.
const replacedLine = "fleet 结束了这次会话并另起了一个新会话"

// replaceThread replaces the session of the thread agent `a`, moved to
// another model: the session is ended as `watch` ends one it reclaims
// (`thread end`'s recorded steps: a summary of fleet's own from the
// records' metadata, the ticket released, the closing line, the row, the
// tab), then a new one is started for the thread as `inbox` starts one
// (the prompt, the earlier sessions' summaries, the ticket claimed
// again), its message the takeover note, headed `[FROM: watch]`. Thread
// starts pass the runtime no model or effort, so a row that recorded one
// is not replaced. Counted on the thread and recorded as failed before
// anything changes, as takeOver does.
func (r *watchRun) replaceThread(a *eventAgent, read *sessionRead, x modelAction) error {
	events, err := r.recentEvents(a.id)
	if err != nil {
		return err
	}
	reason := ""
	switch {
	case a.kind != KindClaude:
		reason = "the ledger has no record of how its session was started (it started before fleet recorded it)"
	case a.model.Valid || a.effort.Valid:
		reason = "its session was started with a model or effort a thread's new session would not get"
	case r.cfg.FednetSocket == "":
		reason = "the scope has no fednet socket, so no session can be ended or started for its thread"
	}
	if reason != "" {
		_, err := r.decide(a, read, x, actionFailed, reason, true)
		return err
	}
	id, err := r.decide(a, read, x, actionFailed, "the takeover did not finish: the watch run stopped before it did", true)
	if err != nil {
		return err
	}
	action, detail, err := r.renewThread(a, x, events)
	if err != nil {
		return err
	}
	_, err = r.conn.Exec("UPDATE model_actions SET action = ?1, detail = ?2 WHERE id = ?3", action, detail, id)
	return dbErr(err)
}

// renewThread ends the session of `a` and starts the thread's next one; a
// step that fails is the action's detail, the steps done recorded as
// `thread end` records them.
func (r *watchRun) renewThread(a *eventAgent, x modelAction, events []string) (action, detail string, err error) {
	e, err := newThreadEnding(r.conn, a.name, false)
	if err != nil {
		return "", "", err
	}
	me := &identity.Identity{Agent: a.name, Scope: r.scope, Issue: e.issue}
	if err := e.linearSteps(me, "", threadSummary(x, events)); err != nil {
		return actionFailed, "the old session's ending stopped at its Linear step, so it goes on: " +
			firstLine(err.Error()), nil
	}
	if err := runStep(r.conn, e.key, "footer", func() error {
		return fednet.Footer(r.cfg.FednetSocket, e.thread, closingLine(replacedLine, e.ticket, e.ticketURL))
	}); err != nil {
		return actionFailed, "the old session's closing line could not be posted, so it goes on: " +
			firstLine(err.Error()), nil
	}
	if err := runStep(r.conn, e.key, "end-row", func() error { return endRow(r.conn, a.name) }); err != nil {
		return "", "", err
	}
	detail = "the old session ended"
	if err := closeOwnTab(r.h, r.conn, a.name); err != nil {
		detail += fmt.Sprintf(" (its tab is left for watch to close: %s)", firstLine(err.Error()))
	}
	jobs, err := openJobsOf(r, a.thread)
	if err != nil {
		return "", "", err
	}
	code, err := toThread(r.h, r.conn, r.scope, r.cfg,
		inboundMessage{Thread: a.thread, Text: threadTakeoverNote(a, x, events, jobs), Watch: true})
	switch {
	case err != nil:
		return actionFailed, detail + ", but the new one did not start: " + firstLine(err.Error()) +
			"; the thread's next message starts one", nil
	case code != exit.Ok:
		return actionFailed, fmt.Sprintf("%s, but the new one did not get its first message (exit %d); the thread's "+
			"next message reaches it or starts one", detail, code), nil
	}
	return actionReplaced, detail + ", a new session started for the thread", nil
}

// threadSummary is the summary of a thread's session the watch ended:
// fleet's own, from the records' metadata, since the agent left none.
func threadSummary(x modelAction, events []string) string {
	var text strings.Builder
	fmt.Fprintf(&text, "fleet watch ended this session and started a new one for the thread, because %s. The agent "+
		"left no summary; fleet read none of the conversation. The latest model events recorded:\n", x.whyLine(false))
	for _, line := range events {
		fmt.Fprintf(&text, "- %s\n", line)
	}
	return text.String()
}

// threadTakeoverNote is the message a thread's new session starts with
// when the watch replaced the one before.
func threadTakeoverNote(a *eventAgent, x modelAction, events, jobs []string) string {
	var text strings.Builder
	fmt.Fprintf(&text, "fleet watch, watch `model-changes`: this session takes over from the session before it on "+
		"this thread, which fleet ended: in it, %s. The latest model events recorded:\n", x.whyLine(false))
	for _, line := range events {
		fmt.Fprintf(&text, "- %s\n", line)
	}
	text.WriteString("\nThe thread, its ticket, its pending questions and its jobs stay as they were")
	if len(jobs) > 0 {
		fmt.Fprintf(&text, " (the open jobs reporting to it: %s)", strings.Join(jobs, ", "))
	}
	text.WriteString(". fleet read none of the earlier conversation and passes none of it on; a summary of " +
		"that session above is fleet's own, from the records' metadata. Find where things stand from the " +
		"thread itself and the summaries of the sessions before. Do not redo the step that was stopped, neither as it " +
		"was nor in other words: take another way, or ask the people in the thread about that step with `fleet " +
		"ask-human --card`. fleet replaces a thread's session once: if this one is stopped or moved to another model " +
		"too, the people decide.\n")
	return text.String()
}

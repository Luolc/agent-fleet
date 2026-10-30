// What the watch `model-changes` does about a session that, read to its
// last complete line, is now stopped by a safety classifier or on another
// model than it started on (docs/design.md): a lead or worker gets a new
// session of the same name, on the model and effort its start recorded; a
// thread agent stopped at its prompt is told once to go on; a thread agent
// moved to another model, or stopped on the dialog Claude Code pauses on
// after a refusal, gets the thread's next session, as `inbox` starts one.
// Each is done once (per row, or per thread for a thread's new
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
	"github.com/Luolc/agent-fleet/internal/herdr"
	"github.com/Luolc/agent-fleet/internal/identity"
)

// listedFiles bounds the agent's files a takeover note names; fileShape
// is a file name it may name, since any agent can name its files.
var (
	listedFiles = 20
	fileShape   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)
)

// actOnSession decides what to do about the agent row `id` from its session as
// this run read it to its last complete line: stopped (a message refused,
// with no reply after it that ended its turn; at its prompt only after a
// stop with no fallback, else on a dialog) or moved (on
// another model than it started on, a variant such as `[1m]` counting as
// the same model); a session that is neither has nothing done, and the
// events of its past are only reported. A stop or move is decided once:
// the session's `decided_at` is set, and cleared once the session is
// neither again, as it is when this run read a line after which it was
// neither. Nothing is decided while the agent is busy (herdr's
// status not idle or done, the row not active), or when it is no longer
// as this run read it just before fleet acts (stillAsRead): the next run
// looks again. `status` is what herdr said of the agent at the run's
// start.
func (r *watchRun) actOnSession(id int64, read *sessionRead, status InHerdr) error {
	moved := read.st.moved()
	// A stop or move after the session was neither, in what this run
	// read, is a new one.
	if read.decided && (read.st.recovered || !read.st.stopped && !moved) {
		if _, err := r.conn.Exec("UPDATE transcripts SET decided_at = NULL WHERE agent = ?1 AND session = ?2", id,
			read.session); err != nil {
			return exit.Database(err)
		}
		read.decided = false
	}
	if read.decided || !read.st.stopped && !moved {
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
	// A message typed into a session on a dialog would answer the dialog,
	// so a thread agent stopped anywhere but at its prompt is not told to
	// go on: its session is replaced, as a moved one's is. A thread's
	// session is replaced once per thread, so each new session is not
	// replaced again; a thread agent stopped at its prompt is told to go on
	// once per session, its row.
	renew := a.role == "thread" && (moved || !read.st.atPrompt)
	handled := a.takeovers
	if renew {
		if handled, err = threadTakeovers(r.conn, a.thread); err != nil {
			return err
		}
	}
	switch {
	case a.role != "thread" && a.job == "":
		_, err := r.decide(a, read, x, actionReported, actionDetail{}, false)
		return err
	case handled > 0:
		_, err := r.decide(a, read, x, actionHeld, actionDetail{}, false)
		return err
	case !r.ready(a, read, status):
		return nil
	case renew:
		return r.replaceThread(a, read, x)
	case a.role == "thread":
		return r.nudge(a, read, x)
	}
	return r.takeOver(a, read, x)
}

// ready is whether the watch may act on `a` now: its row is active, and
// herdr found it idle or done at the run's start (`status`) and finds it
// as this run read it just before fleet acts (stillAsRead).
func (r *watchRun) ready(a *eventAgent, read *sessionRead, status InHerdr) bool {
	return a.state == "active" && (status.Status == "idle" || status.Status == "done") && r.stillAsRead(a.name, read)
}

// stillAsRead is whether the agent `name` is, just before the watch acts
// on its session, as this run found it: herdr says it is idle or done
// now, and its transcript has not grown since this run read it, so no
// message reached it in the meantime, to be lost with its session.
func (r *watchRun) stillAsRead(name string, read *sessionRead) bool {
	agent, err := r.h.CallOK("agent", "get", name)
	if err != nil {
		return false
	}
	inner, _ := herdr.Lookup(agent, "agent").(map[string]any)
	if status, _ := inner["agent_status"].(string); status != "idle" && status != "done" {
		return false
	}
	info, err := os.Stat(read.path)
	return err == nil && info.Size() == read.size
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

// actionDetail is what the watch adds to an action, such as the panes or
// why it failed, for the agents (`en`) and for the people in a thread
// (`zh`); an error quoted in it stays as the tool wrote it.
type actionDetail struct {
	en, zh string
}

// notFinished is a takeover's detail until it finishes.
var notFinished = actionDetail{"the takeover did not finish: the watch run stopped before it did",
	"接手没有做完：watch 这一轮在做完之前停了"}

// decide records the action `x` with `action` and `detail` about the
// session of `a` read in `read`, marks its stop or move decided and, with
// `count`, counts one more takeover (on the row for a lead or worker and
// for a thread agent told to go on, else on the thread), in one
// transaction; it returns the action's id.
func (r *watchRun) decide(a *eventAgent, read *sessionRead, x modelAction, action string, detail actionDetail,
	count bool) (int64, error) {
	tx, err := r.conn.Begin()
	if err != nil {
		return 0, exit.Database(err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec("INSERT INTO model_actions (agent, session, action, why, from_model, to_model, detail, "+
		"detail_zh, at) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9)", a.id, read.session, action, x.why, x.from, x.to,
		detail.en, detail.zh, r.now)
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
	if count && a.role == "thread" && action != actionNudged {
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

// nudge tells the thread agent `a`, stopped at its prompt, to go on, as
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
	_, err = r.decide(a, read, x, actionNudged, actionDetail{}, true)
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

// nudgeMessage is what a thread agent stopped at its prompt is told.
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
	if reason.en != "" {
		_, err := r.decide(a, read, x, actionFailed, reason, true)
		return err
	}
	id, err := r.decide(a, read, x, actionFailed, notFinished, true)
	if err != nil {
		return err
	}
	action, detail, err := r.replace(a, t)
	if err != nil {
		return err
	}
	return r.finish(id, action, detail)
}

// finish records how the action `id` ended.
func (r *watchRun) finish(id int64, action string, detail actionDetail) error {
	_, err := r.conn.Exec("UPDATE model_actions SET action = ?1, detail = ?2, detail_zh = ?3 WHERE id = ?4", action,
		detail.en, detail.zh, id)
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
func (r *watchRun) replace(a *eventAgent, t *takeover) (string, actionDetail, error) {
	place, err := CreateTab(r.h, t.workspace, t.label, a.cwd, t.id)
	if err != nil {
		why := firstLine(err.Error())
		return actionFailed, actionDetail{"no tab could be opened for a new session, so the old one goes on: " + why,
			"开不出新会话的 tab，原会话照常运行：" + why}, nil
	}
	if err := closePaneTab(r.h, a.pane, a.name); err != nil {
		why := firstLine(err.Error())
		d := actionDetail{fmt.Sprintf("the old session's tab (pane %s) could not be closed, so it goes on: %s", a.pane,
			why), fmt.Sprintf("关不掉原会话的 tab (pane %s)，原会话照常运行：%s", a.pane, why)}
		if err := closeTab(r.h, place.TabID, a.name); err != nil {
			why := firstLine(err.Error())
			d.en += fmt.Sprintf("; the new tab %s is left open too: %s", place.TabID, why)
			d.zh += fmt.Sprintf("；新开的 tab %s 也没关掉：%s", place.TabID, why)
		}
		return actionFailed, d, nil
	}
	if _, err := r.conn.Exec("UPDATE agents SET state = 'starting' WHERE id = ?1", a.id); err != nil {
		return "", actionDetail{}, exit.Database(err)
	}
	var created []string
	code, err := startAndDeliver(r.h, r.conn, t.id, place, a.cwd, KindClaude, t.model, t.effort, watchSender, t.body,
		&created)
	switch {
	case err != nil:
		why := firstLine(err.Error())
		return actionFailed, actionDetail{fmt.Sprintf("the old session (pane %s) was closed, but the new one in pane %s "+
			"did not start: %s; its row is left `starting`", a.pane, place.PaneID, why),
			fmt.Sprintf("原会话 (pane %s) 已经关掉，但 pane %s 里的新会话没起来：%s；它的 row 停在 `starting`", a.pane,
				place.PaneID, why)}, nil
	case code != exit.Ok:
		return actionFailed, actionDetail{fmt.Sprintf("the old session (pane %s) was closed and the new one started in "+
			"pane %s, but its first message did not land (exit %d); its row is left `starting`", a.pane, place.PaneID,
			code), fmt.Sprintf("原会话 (pane %s) 已经关掉，新会话在 pane %s 起来了，但第一条消息没送到 (exit %d)；它的 row 停在 "+
			"`starting`", a.pane, place.PaneID, code)}, nil
	}
	return actionReplaced, actionDetail{fmt.Sprintf("pane %s closed, the new session in pane %s", a.pane, place.PaneID),
		fmt.Sprintf("关掉了 pane %s，新会话在 pane %s", a.pane, place.PaneID)}, nil
}

// takeoverPlan is the new session of `a`, its first message rendered as
// its start rendered it, from the ledger, the scope's config and the
// task file, with a note on the takeover; or why there is none. It
// changes nothing.
func (r *watchRun) takeoverPlan(a *eventAgent, x modelAction) (*takeover, actionDetail) {
	if a.kind != KindClaude {
		return nil, actionDetail{"the ledger has no record of the model and effort it was started with (it started " +
			"before fleet recorded them)", "ledger 里没有记它启动时用的模型和 effort (它在 fleet 开始记这两项之前就起了)"}
	}
	role, _ := identity.ParseRole(a.role)
	job, err := openJob(r.conn, a.job)
	if err != nil || job == nil {
		return nil, actionDetail{"its job " + a.job + " is not open in the ledger",
			"ledger 里它的 job " + a.job + " 不是 open"}
	}
	task, _, err := taskFile(a.task, watchSender)
	if err != nil {
		why := firstLine(err.Error())
		return nil, actionDetail{"its task file cannot be read again: " + why, "读不了它的 task file：" + why}
	}
	if info, err := os.Stat(a.cwd); err != nil || !info.IsDir() {
		return nil, actionDetail{"its directory " + a.cwd + " is gone", "它的目录 " + a.cwd + " 不在了"}
	}
	prompt, err := loadPrompt(r.cfg, a.role)
	if err != nil {
		why := firstLine(err.Error())
		return nil, actionDetail{"the " + a.role + " prompt cannot be used: " + why, a.role + " 的 prompt 用不了：" + why}
	}
	cfg, err := jobConfig(job.Repo, job.LeadCwd)
	if err != nil {
		why := firstLine(err.Error())
		return nil, actionDetail{"its job's config cannot be read: " + why, "读不了它的 job 的配置：" + why}
	}
	workspaces, err := WorkspacesLabelled(r.h, a.job)
	if err != nil || len(workspaces) != 1 {
		return nil, actionDetail{"herdr does not have one workspace labelled " + a.job,
			"herdr 里 label 为 " + a.job + " 的 workspace 不是正好一个"}
	}
	t := &takeover{workspace: workspaces[0], label: "lead", model: nullable(a.model), effort: nullable(a.effort),
		id: &identity.Identity{Agent: a.name, Role: role, Parent: a.parent, Scope: r.scope, Job: a.job, Issue: a.issue,
			Scratch: jobScratch(r.cfg, job.Repo, job.LeadCwd)}}
	events, err := r.recentEvents(a.id)
	if err != nil {
		why := firstLine(err.Error())
		return nil, actionDetail{"the ledger's events cannot be read: " + why, "读不了 ledger 里的事件：" + why}
	}
	notes := []string{takeoverNote(a, x, events, agentFiles(t.id.Scratch, a.name, r.cfg.Tilde))}
	var section string
	if role == identity.Worker {
		t.label = strings.TrimPrefix(a.name, a.job+"-")
		statute, err := worktreeStatuteNote(r.conn, a.cwd)
		if err != nil {
			why := firstLine(err.Error())
			return nil, actionDetail{"the ledger's worktrees cannot be read: " + why, "读不了 ledger 里的 worktree：" + why}
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
			why := firstLine(err.Error())
			return nil, actionDetail{"the home thread's latest message cannot be read: " + why,
				"读不了 home thread 最新的消息：" + why}
		}
	}
	issue := atb.Issue{Identifier: a.issue, URL: a.issueURL}
	t.body = prompt.fill(roleSlots(r.cfg, t.id, a.cwd, job.Repo, job.LeadCwd, cfg.MaxAgentsPerJob, issue,
		job.ParentIssue)) + noteSection(append(notes, lightNote(a.light))...) + section + taskSection(issue.URL, task)
	return t, actionDetail{}
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
// another model or stopped on a dialog: the session is ended as `watch` ends one it reclaims
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
	var reason actionDetail
	switch {
	case a.kind != KindClaude:
		reason = actionDetail{"the ledger has no record of how its session was started (it started before fleet " +
			"recorded it)", "ledger 里没有记它的会话是怎么起的 (它在 fleet 开始记之前就起了)"}
	case a.model.Valid || a.effort.Valid:
		reason = actionDetail{"its session was started with a model or effort a thread's new session would not get",
			"它的会话启动时指定了模型或 effort，线程的新会话不会用同样的"}
	case r.cfg.FednetSocket == "":
		reason = actionDetail{"the scope has no fednet socket, so no session can be ended or started for its thread",
			"这个 scope 没有 fednet socket，没法给它的线程结束或另起会话"}
	}
	if reason.en != "" {
		_, err := r.decide(a, read, x, actionFailed, reason, true)
		return err
	}
	id, err := r.decide(a, read, x, actionFailed, notFinished, true)
	if err != nil {
		return err
	}
	action, detail, err := r.renewThread(a, x, events)
	if err != nil {
		return err
	}
	return r.finish(id, action, detail)
}

// renewThread ends the session of `a` and starts the thread's next one; a
// step that fails is the action's detail, the steps done recorded as
// `thread end` records them.
func (r *watchRun) renewThread(a *eventAgent, x modelAction, events []string) (string, actionDetail, error) {
	e, err := newThreadEnding(r.conn, a.name, false)
	if err != nil {
		return "", actionDetail{}, err
	}
	me := &identity.Identity{Agent: a.name, Scope: r.scope, Issue: e.issue}
	if err := e.linearSteps(me, "", threadSummary(x, events)); err != nil {
		why := firstLine(err.Error())
		return actionFailed, actionDetail{"the old session's ending stopped at its Linear step, so it goes on: " + why,
			"结束原会话时停在了 Linear 那一步，原会话照常运行：" + why}, nil
	}
	if err := runStep(r.conn, e.key, "footer", func() error {
		return fednet.Footer(r.cfg.FednetSocket, e.thread, closingLine(replacedLine, e.ticket, e.ticketURL))
	}); err != nil {
		why := firstLine(err.Error())
		return actionFailed, actionDetail{"the old session's closing line could not be posted, so it goes on: " + why,
			"原会话的结束语发不出去，原会话照常运行：" + why}, nil
	}
	if err := runStep(r.conn, e.key, "end-row", func() error { return endRow(r.conn, a.name) }); err != nil {
		return "", actionDetail{}, err
	}
	d := actionDetail{"the old session ended", "原会话已经结束"}
	if err := closeOwnTab(r.h, r.conn, a.name); err != nil {
		why := firstLine(err.Error())
		d.en += fmt.Sprintf(" (its tab is left for watch to close: %s)", why)
		d.zh += fmt.Sprintf(" (它的 tab 留给 watch 去关：%s)", why)
	}
	jobs, err := openJobsOf(r, a.thread)
	if err != nil {
		return "", actionDetail{}, err
	}
	code, err := toThread(r.h, r.conn, r.scope, r.cfg,
		inboundMessage{Thread: a.thread, Text: threadTakeoverNote(a, x, events, jobs), Watch: true})
	switch {
	case err != nil:
		why := firstLine(err.Error())
		return actionFailed, actionDetail{d.en + ", but the new one did not start: " + why +
			"; the thread's next message starts one", d.zh + "，但新会话没起来：" + why + "；线程里的下一条消息会起一个"}, nil
	case code != exit.Ok:
		return actionFailed, actionDetail{fmt.Sprintf("%s, but the new one did not get its first message (exit %d); "+
			"the thread's next message reaches it or starts one", d.en, code),
			fmt.Sprintf("%s，但新会话没收到第一条消息 (exit %d)；线程里的下一条消息会送到它那里，或者另起一个", d.zh, code)}, nil
	}
	return actionReplaced, actionDetail{d.en + ", a new session started for the thread", d.zh + "，给线程另起了一个新会话"},
		nil
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

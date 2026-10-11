// What `watch` does for an agent stopped at a screen (docs/design.md): a
// rule it knows, a person for the screens fleet never answers, and for any
// other screen a helper agent that answers it as the guidance says or asks
// the people.

package cmd

import (
	"database/sql"
	_ "embed" // the helper's prompt and the guidance
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Luolc/agent-fleet/internal/atb"
	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/fednet"
	"github.com/Luolc/agent-fleet/internal/identity"
)

//go:embed unblock_prompt.md
var unblockPrompt string

// screenGuidance is what a helper may press on which screen. Embedded, so
// a helper reads the guidance of the binary that started it.
//
//go:embed screen_guidance.md
var screenGuidance string

const (
	// maxHelpers bounds the helpers started for one stopped agent; after
	// them a person is asked.
	maxHelpers = 3
	// startGraceSecs is how long a `starting` row is its starter's before
	// watch reads its screen: past every bounded step of a start.
	startGraceSecs int64 = 10 * 60
	// helperSecs is how long a helper may work before it is closed as
	// stuck, which counts it against maxHelpers.
	helperSecs int64 = 30 * 60
	// screenLabel labels the tickets of the screens helpers handle.
	screenLabel = "blocked-screen"
)

// screenKind is what the rules make of a screen.
type screenKind int

const (
	// unknownScreen is a screen no rule knows: a helper's.
	unknownScreen screenKind = iota
	// ownTrustScreen is Claude Code's folder-trust dialog for the agent's
	// own directory, answered as a start answers it.
	ownTrustScreen
	// handsOffScreen is a usage limit, a model switch, a usage reset or a
	// purchase: nothing is pressed, no helper started, a person asked.
	handsOffScreen
)

// handsOff are the phrases, lower-cased, of the screens fleet never
// presses keys on. A phrase too many only sends a screen to a person.
var handsOff = []string{
	"usage limit", "rate limit", "rate-limit", "limit reached", "hit your limit", "reached your limit",
	"limit will reset", "resets at", "reset your", "extra usage", "more usage", "upgrade", "purchase",
	"buy ", "billing", "credits", "switch model", "switch to ", "change model", "keep current model",
}

// classifyScreen is what the rules make of `screen`, the screen of an
// agent whose directory is `cwd`. Only the part below the last horizontal
// rule, where Claude Code draws its dialogs, is matched against handsOff,
// so the transcript above cannot decide.
func classifyScreen(screen, cwd string) screenKind {
	if p, ok := trustDialog(screen); ok && sameDir(p.Dir, cwd) {
		return ownTrustScreen
	}
	dialog := strings.ToLower(dialogPart(screen))
	for _, phrase := range handsOff {
		if strings.Contains(dialog, phrase) {
			return handsOffScreen
		}
	}
	return unknownScreen
}

// dialogPart is the screen below its last horizontal rule, or the whole
// screen when nothing but blank lines follows one.
func dialogPart(screen string) string {
	all := lines(screen)
	for i := len(all) - 1; i >= 0; i-- {
		if isRule(all[i]) {
			if below := strings.Join(all[i+1:], "\n"); strings.TrimSpace(below) != "" {
				return below
			}
			break
		}
	}
	return screen
}

// helperPrefix starts the name of every screen helper.
const helperPrefix = "unblock-"

// helperNamed is whether `name` is that of an agent watch starts, a
// screen helper or a revisit agent: no job name may start the same way.
func helperNamed(name string) bool {
	return strings.HasPrefix(name, helperPrefix) || strings.HasPrefix(name, revisitPrefix)
}

// helperName is the helper of the stopped agent whose row has `id`: the
// same name for every helper of that row, so the ledger counts them.
func helperName(id int64) string {
	return fmt.Sprintf("%s%d", helperPrefix, id)
}

// stopRow is a live row as the unblocking reads it.
type stopRow struct {
	ID                                                             int64
	Name, Role, Job, Parent, Cwd, Pane, State, Thread, ParentIssue string
	StartedAt                                                      int64
}

func stopRows(conn *sql.DB) ([]stopRow, error) {
	rows, err := conn.Query("SELECT id, name, role, job, parent, cwd, pane_id, state, thread, parent_issue, started_at " +
		"FROM agents WHERE state != 'ended' ORDER BY id")
	if err != nil {
		return nil, exit.Database(err)
	}
	defer rows.Close()
	var all []stopRow
	for rows.Next() {
		var r stopRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Role, &r.Job, &r.Parent, &r.Cwd, &r.Pane, &r.State, &r.Thread,
			&r.ParentIssue, &r.StartedAt); err != nil {
			return nil, exit.Database(err)
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, exit.Database(err)
	}
	return all, nil
}

// who names the row's place for a person or a lead.
func (r stopRow) who() string {
	var text string
	switch r.Role {
	case "thread":
		text = "the thread agent of thread " + r.Thread
	case "lead":
		text = "the lead of job " + r.Job
	default:
		text = fmt.Sprintf("a worker of job %s, lead %s", r.Job, r.Parent)
	}
	if r.State == "starting" {
		text += ", still starting"
	}
	return text
}

// unblocking is one watch run's look at the stopped agents.
type unblocking struct {
	*watchRun
	inHerdr map[string]InHerdr
	// dir is where helpers run and write their questions.
	dir string
}

// helpers is the run's look at the agents watch starts: in the scope's
// own directory next to the ledger, `<scope>-unblock`.
func (r *watchRun) helpers(inHerdr map[string]InHerdr) (*unblocking, error) {
	ledger, err := db.Path(r.scope)
	if err != nil {
		return nil, err
	}
	return &unblocking{watchRun: r, inHerdr: inHerdr, dir: strings.TrimSuffix(ledger, ".db") + "-unblock"}, nil
}

// unblock looks at every live agent stopped at a screen: an active one
// herdr reports blocked, or one whose start was left at a screen for
// startGraceSecs. It first closes the helpers that are done. A failure
// for one agent is recorded on the run and the others are still looked
// at, while the run has time. Revisit agents are the parent rule's.
func (r *watchRun) unblock(inHerdr map[string]InHerdr) error {
	u, err := r.helpers(inHerdr)
	if err != nil {
		return err
	}
	rows, err := stopRows(r.conn)
	if err != nil {
		return err
	}
	live := map[string]stopRow{}
	for _, row := range rows {
		live[row.Name] = row
	}
	helped := map[string]bool{}
	for _, row := range rows {
		if row.Role != identity.Unblock.String() {
			continue
		}
		if u.outOfTime() {
			return nil
		}
		busy, err := u.helper(row, live)
		u.failedFor(row.Name, err)
		helped[row.Parent] = busy || err != nil
	}
	for _, row := range rows {
		if watchStarted(row.Role) {
			continue
		}
		if u.outOfTime() {
			return nil
		}
		screen, stopped, err := u.stopped(row)
		if err == nil && stopped {
			if helped[row.Name] {
				fmt.Fprintf(os.Stdout, "unblock: %s is stopped at a screen; %s is on it\n", row.Name, helperName(row.ID))
				continue
			}
			err = u.handle(row, screen)
		}
		u.failedFor(row.Name, err)
	}
	return nil
}

// watchStarted is whether `role` is that of an agent watch starts.
func watchStarted(role string) bool {
	return role == identity.Unblock.String() || role == identity.Revisit.String()
}

// failedFor records a failure for the stopped agent or helper `name`.
func (u *unblocking) failedFor(name string, err error) {
	if err != nil {
		u.failed(fmt.Errorf("unblock %s: %w", name, err))
	}
}

// helper looks at a live helper; busy is whether it is still working. A
// helper done with its turn (herdr idle or done), stopped at a screen
// itself, working for helperSecs, gone from herdr, or left `starting` by
// a start that failed is closed and its row ended, after its question,
// when it wrote one, is asked.
func (u *unblocking) helper(row stopRow, live map[string]stopRow) (busy bool, err error) {
	return u.closeDone(row, func(question string) error {
		stopped, ok := live[row.Parent]
		if !ok {
			return nil
		}
		text := fmt.Sprintf("%s (%s) is stopped at a screen that fleet's guidance does not cover, so its helper "+
			"%s asks:\n\n%s", stopped.Name, stopped.who(), row.Name, question)
		if issue, _ := u.helperIssue(row.Name); issue != "" {
			text += "\n\nTicket: " + issue
		}
		if err := u.ask(stopped, text); err != nil {
			return err
		}
		u.tell(stopped, "its helper asked the people in the home thread what to press")
		return nil
	})
}

// closeDone looks at a live agent watch started (a screen helper or a
// revisit agent); busy is whether it is still working. One done with its
// turn (herdr idle or done), stopped at a screen itself, working for
// helperSecs, gone from herdr, or left `starting` by a start that failed
// is closed and its row ended, after `ask` is given the question it
// wrote, if any.
func (u *unblocking) closeDone(row stopRow, ask func(question string) error) (busy bool, err error) {
	agent, present := u.inHerdr[row.Name]
	if present && row.State == "active" && agent.Status != "idle" && agent.Status != "done" &&
		agent.Status != "blocked" && u.now-row.StartedAt < helperSecs {
		return true, nil
	}
	file := u.questionFile(row.Name)
	if question, err := os.ReadFile(file); err == nil && strings.TrimSpace(string(question)) != "" {
		if err := ask(strings.TrimSpace(string(question))); err != nil {
			return false, err
		}
	}
	if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, exit.IO(err)
	}
	if row.Pane != "" {
		if err := closeOwnTab(u.h, u.conn, row.Name); err != nil {
			return false, err
		}
	}
	if err := endRow(u.conn, row.Name); err != nil {
		return false, err
	}
	fmt.Fprintf(os.Stdout, "%s: closed %s (herdr status %q)\n", row.Role, row.Name, agent.Status)
	return false, nil
}

// stopped is the screen of `row` when its agent is stopped at one.
func (u *unblocking) stopped(row stopRow) (screen string, ok bool, err error) {
	agent, present := u.inHerdr[row.Name]
	switch {
	case !present:
		return "", false, nil
	case row.State == "active" && agent.Status == "blocked":
	case row.State == "starting" && u.now-row.StartedAt >= startGraceSecs:
	default:
		return "", false, nil
	}
	if screen, err = u.h.Screen(row.Name); err != nil {
		return "", false, err
	}
	if row.State == "starting" && IsInputBox(screen) {
		return "", false, nil
	}
	return screen, true, nil
}

// handle deals with one stopped agent that has no live helper: nothing
// while a question about it waits for the people; the folder-trust
// dialog for its own directory answered; a screen fleet never answers
// sent to a person once; any other screen to a new helper, at most
// maxHelpers of them, then to a person once.
func (u *unblocking) handle(row stopRow, screen string) error {
	helper := helperName(row.ID)
	var pending int
	if err := u.conn.QueryRow("SELECT count(*) FROM questions WHERE asked_by = ?1 AND state = 'pending'",
		helper).Scan(&pending); err != nil {
		return exit.Database(err)
	}
	if pending > 0 {
		fmt.Fprintf(os.Stdout, "unblock: %s is stopped at a screen; the people were asked and have not answered\n", row.Name)
		return nil
	}
	shown := "\n\nThe screen:\n\n" + fenced(tail(screen, 15))
	switch classifyScreen(screen, row.Cwd) {
	case ownTrustScreen:
		// A failure is the next run's to look at again, on the screen the
		// keys left.
		if err := settleStopped(u.h, row.Name, row.Cwd); err != nil {
			return fmt.Errorf("the folder-trust rule did not get it to its input box: %w", err)
		}
		fmt.Fprintf(os.Stdout, "unblock: %s: answered the folder-trust dialog for its own directory\n", row.Name)
		if row.State == "starting" {
			u.tell(row, "fleet answered the folder-trust dialog for its own directory")
		}
		return nil
	case handsOffScreen:
		return u.askOnce(row, "ask-hands-off", fmt.Sprintf("%s (%s) is stopped at a screen fleet never answers: a usage "+
			"limit, a model switch, a usage reset or a purchase. A person has to deal with it at the machine (agent %s "+
			"in the herdr session %s).", row.Name, row.who(), row.Name, identity.Session(u.scope))+shown)
	}
	var helpers int
	if err := u.conn.QueryRow("SELECT count(*) FROM agents WHERE name = ?1", helper).Scan(&helpers); err != nil {
		return exit.Database(err)
	}
	if helpers >= maxHelpers {
		return u.askOnce(row, "ask-helpers", fmt.Sprintf("%s (%s) is still stopped at a screen after %d helpers. A "+
			"person has to look at it at the machine (agent %s in the herdr session %s).", row.Name, row.who(), helpers,
			row.Name, identity.Session(u.scope))+shown)
	}
	return u.startHelper(row, helper, screen)
}

// settleStopped is how the folder-trust rule answers the dialog; a
// variable so the tests can make it fail.
var settleStopped = settle

// fenced is `text` in a Markdown code block.
func fenced(text string) string {
	return "```\n" + text + "\n```"
}

// askOnce asks the people about `row` once for its `step`: a retry after
// a failure asks again, a step done is never repeated.
func (u *unblocking) askOnce(row stopRow, step, text string) error {
	asked := false
	err := runStep(u.conn, fmt.Sprintf("unblock:%d", row.ID), step, func() error {
		asked = true
		return u.ask(row, text)
	})
	if err != nil || !asked {
		return err
	}
	u.tell(row, "fleet asked the people in the home thread to deal with it")
	return nil
}

// ask posts `text` to the home thread of `row` (its own thread for a
// thread agent, else its job's), records it as a question pending from
// the row's helper, and then, unless the stopped agent is the thread's
// own, delivers it to the thread's agent as `ask-human` does; a delivery
// that fails is printed, since the question is out.
func (u *unblocking) ask(row stopRow, text string) error {
	thread := row.Thread
	if row.Role != "thread" {
		err := u.conn.QueryRow("SELECT home_thread FROM jobs WHERE job = ?1 AND state = 'open'", row.Job).Scan(&thread)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return exit.Database(err)
		}
	}
	if thread == "" {
		return exit.Refusedf("no home thread to ask the people in about %s's screen", row.Name)
	}
	helper := helperName(row.ID)
	if err := u.post(thread, row.Job, helper, text); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "unblock: %s: asked the people in thread %s\n", row.Name, thread)
	if row.Role == "thread" {
		return nil
	}
	msg := inboundMessage{Thread: thread, Text: text, Question: helper, Screen: true}
	if code, err := toThread(u.h, u.conn, u.scope, u.cfg, msg); err != nil || code != exit.Ok {
		fmt.Fprintf(os.Stderr, "fleet: unblock %s: the question is posted, but did not reach the agent of thread %s "+
			"(exit %d): %v\n", row.Name, thread, code, err)
	}
	return nil
}

// post posts `text` to `thread` and records it as a question pending from
// `asker` for `job` (empty for none).
func (u *unblocking) post(thread, job, asker, text string) error {
	if u.cfg.FednetSocket == "" {
		return exit.Refusedf("fednet.socket is not configured for scope %s, so %s cannot ask the people", u.scope, asker)
	}
	_, _, code, err := fednet.Relay(u.cfg.FednetSocket, thread, text, nil)
	if err != nil {
		return err
	}
	if code != 0 {
		return exit.Environmentf("fednet client post failed (exit status: %d)", code)
	}
	if _, err := u.conn.Exec("INSERT INTO questions (job, thread, asked_by, text, approval, state, asked_at) "+
		"VALUES (?1, ?2, ?3, ?4, 0, 'pending', ?5)", job, thread, asker, text, db.Now()); err != nil {
		return exit.Database(err)
	}
	return nil
}

// tell lets whoever has to know about a screen fleet did not answer by
// rule know `what` fleet did: a worker's lead, and the starter of a lead
// or worker whose start stopped at the screen. A thread agent's start is
// finished by `inbox` with the next message, so nobody is told about it.
func (u *unblocking) tell(row stopRow, what string) {
	if row.Role == "thread" || row.Parent == "" || row.Role == "lead" && row.State != "starting" {
		return
	}
	text := fmt.Sprintf("fleet watch: %s (%s) is stopped at a screen; %s.\n", row.Name, row.who(), what)
	if row.State == "starting" {
		text += "\nIts row is still `starting`: its start stopped at a screen or did not see its first message " +
			"delivered, so it may not have its task. Once past the screen it may wait at its input box with " +
			"nothing to do; what to do with it is yours to decide (`fleet status` shows it).\n"
	} else {
		text += "\nNothing to do on your side unless it stays stopped: the screen is not yours to answer.\n"
	}
	message, err := WithHeader(watchSender, text)
	if err == nil {
		var code exit.Code
		code, err = Deliver(u.h, row.Parent, message)
		if err == nil && code != exit.Ok {
			err = exit.New(code, fmt.Sprintf("telling %s gave exit %d", row.Parent, code))
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet: unblock %s: %v\n", row.Name, err)
	}
}

// questionFile is where the helper `name` writes its question.
func (u *unblocking) questionFile(name string) string {
	return filepath.Join(u.dir, name+"-question.md")
}

// helperIssue is the ticket an earlier helper of the same name opened,
// "" when none did.
func (u *unblocking) helperIssue(name string) (string, error) {
	var issue string
	err := u.conn.QueryRow("SELECT issue FROM agents WHERE name = ?1 AND issue != '' ORDER BY id DESC LIMIT 1",
		name).Scan(&issue)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", exit.Database(err)
	}
	return issue, nil
}

// ticket is the helper's ticket: the one an earlier helper of the same
// stopped agent opened, else a new one labelled blocked-screen in the
// scope's Linear team and unblock project; none when either is not
// configured.
func (u *unblocking) ticket(row stopRow, helper, screen string) (string, error) {
	if u.cfg.LinearTeam == "" || u.cfg.UnblockProject == "" {
		return "", nil
	}
	if issue, err := u.helperIssue(helper); err != nil || issue != "" {
		return issue, err
	}
	file := filepath.Join(u.dir, helper+"-ticket.md")
	description := fmt.Sprintf("%s (%s) was stopped at a screen that fleet has no rule for when `fleet watch` read it "+
		"at %s; the helper %s handles it and records here what it did or asked.\n\nThe screen:\n\n%s\n",
		row.Name, row.who(), time.Unix(u.now, 0).UTC().Format(time.RFC3339), helper, fenced(tail(screen, 15)))
	if err := os.WriteFile(file, []byte(description), 0o644); err != nil {
		return "", exit.IO(err)
	}
	issue, err := atb.Create(u.cfg.LinearTeam, u.cfg.UnblockProject, "", screenLabel, row.Name+" stopped at a screen", file)
	if err != nil {
		return "", err
	}
	return issue.Identifier, nil
}

// startHelper starts the helper of a stopped agent in the scope's
// `threads` workspace, in the helpers' directory, with the screen, the
// guidance and what the people said about the screen so far.
func (u *unblocking) startHelper(row stopRow, helper, screen string) error {
	if err := os.MkdirAll(u.dir, 0o755); err != nil {
		return exit.IO(err)
	}
	issue, err := u.ticket(row, helper, screen)
	if err != nil {
		return err
	}
	body, err := u.helperPrompt(row, helper, screen, issue)
	if err != nil {
		return err
	}
	id := &identity.Identity{Agent: helper, Role: identity.Unblock, Parent: row.Name, Scope: u.scope, Issue: issue}
	if err := u.startInThreads(id, "", body); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "unblock: %s: started %s\n", row.Name, helper)
	u.tell(row, fmt.Sprintf("fleet started %s, a helper that answers it as fleet's guidance says or asks the people",
		helper))
	return nil
}

// startInThreads starts the agent `id` in a new tab of the scope's
// `threads` workspace, in the helpers' directory, with `body` as its first
// message headed `[FROM: watch]`; its row records `parentIssue`.
func (u *unblocking) startInThreads(id *identity.Identity, parentIssue, body string) error {
	if err := reserve(u.conn, func(q querier) error { return liveNameTaken(q, id.Agent) },
		func(q querier) error { return insertStarting(q, id, u.dir, "", parentIssue) }); err != nil {
		return err
	}
	created := []string{fmt.Sprintf("ledger row %s (state starting)", id.Agent)}
	// What a failed start leaves, the next run closes as an agent that
	// did not get to work.
	failed := func(err error, code exit.Code) error {
		code, _ = startFailed(id.Agent, err, code, created, "")
		return exit.New(code, fmt.Sprintf("the start of %s did not complete; the next run closes what it left", id.Agent))
	}
	home, err := Home()
	if err != nil {
		return failed(err, 0)
	}
	workspace, err := threadsWorkspaceID(u.h, u.scope, home)
	if err != nil {
		return failed(err, 0)
	}
	place, err := CreateTab(u.h, workspace, id.Agent, u.dir, id)
	if err != nil {
		return failed(err, 0)
	}
	created = append(created, fmt.Sprintf("tab %s (%s)", id.Agent, place.TabID))
	code, err := startAndDeliver(u.h, u.conn, id, place, u.dir, nil, nil, watchSender, body, &created)
	if err != nil || code != exit.Ok {
		return failed(err, code)
	}
	return nil
}

// helperPrompt is the helper's first message: the prompt, the guidance,
// the screen, and the questions earlier helpers asked about it with the
// latest message a person posted in the home thread.
func (u *unblocking) helperPrompt(row stopRow, helper, screen, issue string) (string, error) {
	ticket := "none: tickets are off for this scope, so put what you did or asked in your last message instead."
	if issue != "" {
		ticket = fmt.Sprintf("%s, labelled %s; record on it with `atb linear comment %s --body-file <file>`, a file "+
			"you write first in your directory.", issue, screenLabel, issue)
	}
	text := strings.NewReplacer("{{agent}}", helper, "{{scope}}", u.scope, "{{dir}}", u.cfg.Tilde(u.dir),
		"{{blocked}}", row.Name, "{{who}}", row.who(), "{{cwd}}", row.Cwd,
		"{{question_file}}", u.questionFile(helper), "{{ticket}}", ticket).Replace(unblockPrompt)
	text += "\n" + screenGuidance + "\n## The screen\n\nAs watch read it:\n\n" + fenced(strings.TrimRight(screen, "\n")) + "\n"
	rows, err := u.conn.Query("SELECT text, state, asked_at, thread FROM questions WHERE asked_by = ?1 ORDER BY id",
		helper)
	if err != nil {
		return "", exit.Database(err)
	}
	defer rows.Close()
	var earlier []string
	thread := ""
	for rows.Next() {
		var question, state string
		var at int64
		if err := rows.Scan(&question, &state, &at, &thread); err != nil {
			return "", exit.Database(err)
		}
		earlier = append(earlier, fmt.Sprintf("Asked at %s (%s):\n\n> %s", time.Unix(at, 0).UTC().Format(time.RFC3339),
			state, strings.ReplaceAll(question, "\n", "\n> ")))
	}
	if err := rows.Err(); err != nil {
		return "", exit.Database(err)
	}
	if len(earlier) == 0 {
		return text, nil
	}
	text += "\n## Earlier on this screen\n\n" + strings.Join(earlier, "\n\n") + "\n"
	m, err := latestMessage(u.conn, thread)
	if err != nil {
		return "", err
	}
	section, err := latestMessageSection(m)
	if err != nil {
		return "", err
	}
	return text + section, nil
}

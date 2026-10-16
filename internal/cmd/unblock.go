// What `watch` does for an agent stopped at a screen (docs/design.md): a
// rule it knows, a person for the screens fleet never answers, and for any
// other screen, by the `unblock` watch (source blocked-screens), a worker
// that answers it as the guidance says or asks the people.

package cmd

import (
	"database/sql"
	_ "embed" // the unblock watch's prompt and the guidance
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Luolc/agent-fleet/internal/atb"
	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/fednet"
	"github.com/Luolc/agent-fleet/internal/identity"
)

// unblockPrompt is the built-in prompt of the `unblock` watch: what its
// workers do, then screenGuidance, what they may press on which screen.
// Embedded, so a worker reads the guidance of the binary that started it.
var (
	//go:embed unblock_prompt.md
	unblockPrompt string
	//go:embed screen_guidance.md
	screenGuidance string
)

const (
	// maxHelpers bounds the workers started for one stopped agent; after
	// them a person is asked.
	maxHelpers = 3
	// startGraceSecs is how long a `starting` row is its starter's before
	// watch reads its screen: past every bounded step of a start.
	startGraceSecs int64 = 10 * 60
	// screenLabel labels the tickets of the screens workers handle.
	screenLabel = "blocked-screen"
)

// screenKind is what the rules make of a screen.
type screenKind int

const (
	// unknownScreen is a screen no rule knows: a worker's.
	unknownScreen screenKind = iota
	// ownTrustScreen is Claude Code's folder-trust dialog for the agent's
	// own directory, answered as a start answers it.
	ownTrustScreen
	// handsOffScreen is a usage limit, a model switch, a usage reset or a
	// purchase: nothing is pressed, no worker started, a person asked.
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

// screenWorkers are the names of the workers for the stopped agent whose
// row has `id`: the `unblock` watch's, the same for every worker of that
// row so the ledger counts them, and the name its helpers had before the
// upgrade.
func screenWorkers(id int64) []string {
	key := strconv.FormatInt(id, 10)
	return []string{workerName(screenWatch, key), legacyHelperPrefix + key}
}

// screenWatch is the name of the default watch with the source
// blocked-screens.
const screenWatch = "unblock"

// stopRow is a live row as the screens and the watch workers read it.
type stopRow struct {
	ID                                                                   int64
	Name, Role, Job, Parent, Cwd, Pane, State, Thread, ParentIssue, Task string
	StartedAt                                                            int64
}

func stopRows(conn *sql.DB) ([]stopRow, error) {
	rows, err := conn.Query("SELECT id, name, role, job, parent, cwd, pane_id, state, thread, parent_issue, task, " +
		"started_at FROM agents WHERE state != 'ended' ORDER BY id")
	if err != nil {
		return nil, exit.Database(err)
	}
	defer rows.Close()
	var all []stopRow
	for rows.Next() {
		var r stopRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Role, &r.Job, &r.Parent, &r.Cwd, &r.Pane, &r.State, &r.Thread,
			&r.ParentIssue, &r.Task, &r.StartedAt); err != nil {
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

// unblock looks at every live agent stopped at a screen, but the agents
// watch started: an active one herdr reports blocked, or one whose start
// was left at a screen for startGraceSecs. The rules are built in; the
// workers for the other screens are the `unblock` watch's, started only
// when it is due. A failure for one agent is recorded on the run and the
// others are still looked at, while the run has time.
func (w *workers) unblock() error {
	e := w.builtinWatch(config.BlockedScreens)
	due, err := w.due(e)
	if err != nil {
		return err
	}
	rows, err := stopRows(w.conn)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.startedByWatch() {
			continue
		}
		if w.outOfTime() {
			return nil
		}
		screen, stopped, err := w.stopped(row)
		if err == nil && stopped {
			names := screenWorkers(row.ID)
			if w.busy[names[0]] || w.busy[names[1]] {
				fmt.Fprintf(os.Stdout, "unblock: %s is stopped at a screen; %s is on it\n", row.Name, names[0])
				continue
			}
			err = w.handle(e, due, row, screen)
		}
		if err != nil {
			w.failed(fmt.Errorf("unblock %s: %w", row.Name, err))
		}
	}
	if !due {
		return nil
	}
	return w.ran(e)
}

// stopped is the screen of `row` when its agent is stopped at one.
func (u *workers) stopped(row stopRow) (screen string, ok bool, err error) {
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

// handle deals with one stopped agent that has no live worker: nothing
// while a question about it waits for the people; the folder-trust
// dialog for its own directory answered; a screen fleet never answers
// sent to a person once; any other screen to a person once after
// maxHelpers workers, before that, when the watch `e` is `due`, to a new
// worker.
func (u *workers) handle(e config.Entry, due bool, row stopRow, screen string) error {
	names := screenWorkers(row.ID)
	pending, _, err := pendingFrom(u.conn, names)
	if err != nil {
		return err
	}
	if pending {
		fmt.Fprintf(os.Stdout, "unblock: %s is stopped at a screen; the people were asked and have not answered\n", row.Name)
		return nil
	}
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
		card, err := u.screenCard(row, row.Name+" is stopped at a screen fleet never answers", fmt.Sprintf("%s (%s) is "+
			"stopped at a screen fleet never answers: a usage limit, a model switch, a usage reset or a purchase.",
			row.Name, row.who()), screen)
		if err != nil {
			return err
		}
		return u.askOnce(row, "ask-hands-off", card)
	}
	var helpers int
	if err := u.conn.QueryRow("SELECT count(*) FROM agents WHERE name IN (?1, ?2)", names[0], names[1]).
		Scan(&helpers); err != nil {
		return exit.Database(err)
	}
	if helpers >= maxHelpers {
		card, err := u.screenCard(row, fmt.Sprintf("%s is still stopped at a screen after %d helpers", row.Name, helpers),
			fmt.Sprintf("%s (%s) is still stopped at a screen after %d helpers.", row.Name, row.who(), helpers), screen)
		if err != nil {
			return err
		}
		return u.askOnce(row, "ask-helpers", card)
	}
	if !due {
		why := "is off"
		if !e.Off {
			why = "is not due this run"
		}
		fmt.Fprintf(os.Stdout, "unblock: %s is stopped at a screen; the watch %s %s\n", row.Name, e.Name, why)
		return nil
	}
	return u.startHelper(e, row, screen)
}

// settleStopped is how the folder-trust rule answers the dialog; a
// variable so the tests can make it fail.
var settleStopped = settle

// fenced is `text` in a Markdown code block.
func fenced(text string) string {
	return "```\n" + text + "\n```"
}

// askOnce asks the people about `row` once for its `step`, on `card`: a
// retry after a failure asks again, a step done is never repeated.
func (u *workers) askOnce(row stopRow, step, card string) error {
	asked := false
	err := runStep(u.conn, fmt.Sprintf("unblock:%d", row.ID), step, func() error {
		asked = true
		return u.ask(row, screenWorkers(row.ID)[0], card)
	})
	if err != nil || !asked {
		return err
	}
	u.tell(row, "fleet asked the people in the home thread to deal with it")
	return nil
}

// screenCard is the card the people are asked on about `row`, stopped at
// a screen a person has to deal with: `title`, then `why`, where to find
// the agent and the last 15 lines of its screen, its earliest lines left
// out, and counted, while the context is over fednet's limit. For an
// agent of a job, the people may also have the thread's agent end the
// job.
func (u *workers) screenCard(row stopRow, title, why, screen string) (string, error) {
	shown := lines(tail(screen, 15))
	var context string
	for left := 0; left <= len(shown); left++ {
		omitted := ""
		if left > 0 {
			omitted = fmt.Sprintf(" (its first %d line(s) left out)", left)
		}
		context = fmt.Sprintf("%s A person has to deal with it at the machine (agent %s in the herdr session %s).\n\n"+
			"The screen%s:\n\n%s", why, row.Name, identity.Session(u.scope), omitted,
			fenced(strings.Join(shown[left:], "\n")))
		if utf8.RuneCountInString(context) <= maxContext {
			break
		}
	}
	var options []cardOption
	if row.Job != "" {
		options = []cardOption{
			{Short: "I'll deal with it", Long: "A person deals with the screen at the machine; fleet does not ask " +
				"about it again.", Recommended: true},
			{Short: "End job " + row.Job, Long: fmt.Sprintf("The thread's agent ends job %s by force (`fleet job end "+
				"%s --force`), its work as it stands.", row.Job, row.Job)},
		}
	}
	return encodeCard(title, context, options)
}

// ask asks the people on `card` in the home thread of `row` (its own
// thread for a thread agent, else its job's), as a question pending from
// `helper`, the row's worker, and then, unless the stopped agent is the
// thread's own, delivers it to the thread's agent; a delivery that fails
// is printed, since the question is out. A single-repo job without a
// home thread gets one opened first, as `ask-thread` opens it.
func (u *workers) ask(row stopRow, helper, card string) error {
	thread := row.Thread
	if row.Role != "thread" {
		job, err := openJob(u.conn, row.Job)
		if err != nil {
			return err
		}
		if job != nil {
			thread = job.HomeThread
		}
		if thread == "" && job != nil && job.Repo != "" && u.cfg.FednetSocket != "" {
			if thread, err = openHomeThread(u.conn, u.cfg, job); err != nil {
				return err
			}
		}
	}
	if thread == "" {
		return exit.Refusedf("no home thread to ask the people in about %s's screen", row.Name)
	}
	link, err := u.askCard(thread, row.Job, helper, card)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "unblock: %s: asked the people in thread %s\n", row.Name, thread)
	if row.Role == "thread" {
		return nil
	}
	msg := inboundMessage{Thread: thread, Text: cardText(link, card), Question: helper, Screen: true}
	if code, err := toThread(u.h, u.conn, u.scope, u.cfg, msg); err != nil || code != exit.Ok {
		fmt.Fprintf(os.Stderr, "fleet: unblock %s: the question is posted, but did not reach the agent of thread %s "+
			"(exit %d): %v\n", row.Name, thread, code, err)
	}
	return nil
}

// errCardRefused marks a card fednet refused (its exit 2: one over a
// limit, not JSON, or a hub older than the client), which it refuses
// again however often it is asked.
var errCardRefused = errors.New("fednet refused the card")

// askCard asks the people in `thread` on `card` (its JSON), a question
// pending from `asker` for `job` (empty for none), and returns the card's
// link; a card asked before that still waits is not posted again. A card
// whose post failed stays pending without a card_id, which no rule waits
// on (pendingFrom), and the next try posts it under the same ref, so a
// post fednet made but did not report makes no second card; another
// card of the asker's that never got posted is closed. A card fednet
// refused closes its question: it would be refused again.
func (r *watchRun) askCard(thread, job, asker, card string) (string, error) {
	if r.cfg.FednetSocket == "" {
		return "", exit.Refusedf("fednet.socket is not configured for scope %s, so %s cannot ask the people", r.scope, asker)
	}
	id, err := pendingQuestion(r.conn, job, thread, asker, card, 0, true)
	if err != nil {
		return "", err
	}
	var cardID, link string
	if err := r.conn.QueryRow("SELECT card_id, permalink FROM questions WHERE id = ?1", id).Scan(&cardID, &link); err != nil {
		return "", exit.Database(err)
	}
	if cardID != "" {
		return link, nil
	}
	if _, err := r.conn.Exec("UPDATE questions SET state = 'closed' WHERE thread = ?1 AND asked_by = ?2 AND card = 1 "+
		"AND card_id = '' AND state = 'pending' AND id != ?3", thread, asker, id); err != nil {
		return "", exit.Database(err)
	}
	path, remove, err := tempFile(card + "\n")
	if err != nil {
		return "", err
	}
	defer remove()
	posted, stderr, code, err := fednet.Ask(r.cfg.FednetSocket, thread, questionRef(r.scope, id), path)
	if err == nil && code == 0 {
		_, err = r.conn.Exec("UPDATE questions SET card_id = ?1, permalink = ?2 WHERE id = ?3", posted.ID, posted.Permalink, id)
		return posted.Permalink, dbErr(err)
	}
	switch {
	case err != nil:
		return "", err
	case code == 2:
		if _, err := r.conn.Exec("UPDATE questions SET state = 'closed' WHERE id = ?1", id); err != nil {
			return "", exit.Database(err)
		}
		return "", fmt.Errorf("%w, question #%d is closed: %s", errCardRefused, id, strings.TrimSpace(string(stderr)))
	}
	return "", exit.Environmentf("fednet client ask failed (exit status %d), question #%d is posted on the next try: %s",
		code, id, strings.TrimSpace(string(stderr)))
}

// cardText is a card fleet posted, as the thread's agent is told of it:
// its JSON, then its link.
func cardText(link, card string) string {
	return fmt.Sprintf("%s\n\nCard: %s", card, link)
}

// tell lets whoever has to know about a screen fleet did not answer by
// rule know `what` fleet did: a worker's lead, and the starter of a lead
// or worker whose start stopped at the screen. A thread agent's start is
// finished by `inbox` with the next message, so nobody is told about it;
// nor is the caller without a role that started a lead, which is no agent.
func (u *workers) tell(row stopRow, what string) {
	if row.Role == "thread" || row.Parent == "" || row.Parent == plainStarter || row.Role == "lead" && row.State != "starting" {
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

// homeThread is the thread the people are asked in about `row`: its own
// for a thread agent, else its open job's home thread; "" for none.
func (u *workers) homeThread(row stopRow) (string, error) {
	if row.Role == "thread" {
		return row.Thread, nil
	}
	job, err := openJob(u.conn, row.Job)
	if err != nil || job == nil {
		return "", err
	}
	return job.HomeThread, nil
}

// promptBroken is what watch does instead of `what` (a start, or telling
// an agent) when its prompt cannot be put together: the error is posted
// to `thread`, the one the agent would have asked in, once per error (the
// steps record `prompt-error:<digest>`), so a thread that meets the same
// error later is not told; the error is returned for the run's output,
// which is all there is without a thread or a socket.
func (r *watchRun) promptBroken(err error, thread, what string) error {
	var broken *promptError
	if !errors.As(err, &broken) || thread == "" || r.cfg.FednetSocket == "" {
		return err
	}
	posted := runStep(r.conn, "prompt-error:"+broken.digest, "posted", func() error {
		return fednet.Post(r.cfg.FednetSocket, thread, fmt.Sprintf("fleet watch did not %s: fleet's prompt config is "+
			"broken (%v). This is said once for this error; once it is fixed, a later run tries again.", what, err))
	})
	return errors.Join(err, posted)
}

// helperIssue is the ticket an earlier worker of one of `names` opened,
// "" when none did.
func helperIssue(conn *sql.DB, names []string) (string, error) {
	var issue string
	err := conn.QueryRow("SELECT issue FROM agents WHERE name IN (?"+strings.Repeat(", ?", len(names)-1)+
		") AND issue != '' ORDER BY id DESC LIMIT 1", anys(names)...).Scan(&issue)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", exit.Database(err)
	}
	return issue, nil
}

// ticket is the worker's ticket: the one an earlier worker of the same
// stopped agent opened, else a new one labelled blocked-screen in the
// scope's Linear team and unblock project; none when either is not
// configured.
func (u *workers) ticket(row stopRow, screen string) (string, error) {
	if u.cfg.LinearTeam == "" || u.cfg.UnblockProject == "" {
		return "", nil
	}
	names := screenWorkers(row.ID)
	if issue, err := helperIssue(u.conn, names); err != nil || issue != "" {
		return issue, err
	}
	if err := os.MkdirAll(u.dir, 0o755); err != nil {
		return "", exit.IO(err)
	}
	file := filepath.Join(u.dir, names[0]+"-ticket.md")
	description := fmt.Sprintf("%s (%s) was stopped at a screen that fleet has no rule for when `fleet watch` read it "+
		"at %s; the worker %s handles it and records here what it did or asked.\n\nThe screen:\n\n%s\n",
		row.Name, row.who(), time.Unix(u.now, 0).UTC().Format(time.RFC3339), names[0], fenced(tail(screen, 15)))
	if err := os.WriteFile(file, []byte(description), 0o644); err != nil {
		return "", exit.IO(err)
	}
	issue, err := atb.Create(u.cfg.LinearTeam, u.cfg.UnblockProject, "", screenLabel, row.Name+" stopped at a screen", file)
	if err != nil {
		return "", err
	}
	return issue.Identifier, nil
}

// startHelper starts the `unblock` watch's worker for a stopped agent,
// its subject the agent, the screen and the ticket, with what the people
// said about the screen so far; a broken worker text or watch prompt
// starts nothing and opens no ticket.
func (u *workers) startHelper(e config.Entry, row stopRow, screen string) error {
	role, err := u.workerPrompt()
	var prompt *promptText
	if err == nil {
		prompt, err = watchPrompt(u.cfg, e)
	}
	if err != nil {
		thread, threadErr := u.homeThread(row)
		if threadErr != nil {
			return errors.Join(err, threadErr)
		}
		return u.promptBroken(err, thread, fmt.Sprintf("start a screen helper for %s (%s)", row.Name, row.who()))
	}
	issue, err := u.ticket(row, screen)
	if err != nil {
		return err
	}
	id, s, body, err := u.screenMessage(e, role, prompt, row, screen, issue)
	if err != nil {
		return err
	}
	if err := u.startWorker(e, id, s, body); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "unblock: %s: started %s\n", row.Name, id.Agent)
	u.tell(row, fmt.Sprintf("fleet started %s, a helper that answers it as fleet's guidance says or asks the people",
		id.Agent))
	return nil
}

// screenMessage is the identity, the subject and the first message of the
// `unblock` watch's worker for the stopped agent `row`, with the ticket
// `issue` ("" for none): the worker text `role`, the questions earlier
// workers asked about the agent, and the agent's screen with the watch's
// prompt.
func (u *workers) screenMessage(e config.Entry, role, prompt *promptText, row stopRow, screen, issue string) (
	*identity.Identity, workerSubject, string, error) {
	names := screenWorkers(row.ID)
	id := u.workerID(names[0], issue)
	s := workerSubject{Watch: e.Name, Key: strconv.FormatInt(row.ID, 10), Job: row.Job, Agent: row.Name}
	body, err := u.workerMessage(role, prompt, id, s, screenSubject(row, screen, issue), names)
	return id, s, body, err
}

// screenSubject is the text of a stopped agent as the `unblock` watch's
// worker gets it: the agent, how to read and press on it, the ticket and
// the screen as watch read it.
func screenSubject(row stopRow, screen, issue string) string {
	ticket := "none: tickets are off for this scope, so put what you did or asked in your last message instead."
	if issue != "" {
		ticket = fmt.Sprintf("%s, labelled %s; record on it with `atb linear comment %s --body-file <file>`, a file "+
			"you write first in your directory.", issue, screenLabel, issue)
	}
	return fmt.Sprintf("The agent %s (%s, working directory %s) is stopped at a screen that fleet has no rule for. "+
		"Read its screen with `herdr agent read %s --source visible`; press a key on it with `herdr agent send-keys %s "+
		"<key>`.\n\nTicket: %s\n\nThe screen as watch read it:\n\n%s\n", row.Name, row.who(), row.Cwd, row.Name, row.Name,
		ticket, fenced(strings.TrimRight(screen, "\n")))
}

// earlierSection is the section `heading` of an agent's first message
// with the questions `askers` asked, oldest first, each with the last
// press on its card; for a question fleet posted as text before cards,
// the latest message a person posted in the thread of the last of them.
// "" when they asked none.
func earlierSection(conn *sql.DB, heading string, askers []string) (string, error) {
	var earlier []string
	thread := ""
	text := false
	query := "SELECT text, state, asked_at, thread, card, answer FROM questions WHERE asked_by IN (?" +
		strings.Repeat(", ?", len(askers)-1) + ") ORDER BY id"
	err := eachRow(conn, query, func(rows *sql.Rows) error {
		var question, state, answer string
		var at int64
		var card bool
		if err := rows.Scan(&question, &state, &at, &thread, &card, &answer); err != nil {
			return err
		}
		text = text || !card
		entry := fmt.Sprintf("Asked at %s (%s):\n\n> %s", time.Unix(at, 0).UTC().Format(time.RFC3339), state,
			strings.ReplaceAll(question, "\n", "\n> "))
		if answer != "" {
			entry += "\n\nThe last press on its card:\n\n" + answer
		}
		earlier = append(earlier, entry)
		return nil
	}, anys(askers)...)
	if err != nil || len(earlier) == 0 {
		return "", err
	}
	section := ""
	if text {
		m, err := latestMessage(conn, thread)
		if err != nil {
			return "", err
		}
		if section, err = latestMessageSection(m); err != nil {
			return "", err
		}
	}
	return "\n## " + heading + "\n\n" + strings.Join(earlier, "\n\n") + "\n" + section, nil
}

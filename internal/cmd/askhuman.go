// `fleet ask-human` and `fleet ask-thread`: a thread agent asks the people
// in its thread a question on a card; a lead puts its question to the
// home thread's agent, which settles it or asks the people for it.

package cmd

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/fednet"
	"github.com/Luolc/agent-fleet/internal/herdr"
	"github.com/Luolc/agent-fleet/internal/identity"
)

// AskHumanAbout and AskHumanLongAbout are the help texts of `ask-human`;
// AskThreadAbout and AskThreadLongAbout those of `ask-thread`.
const (
	AskHumanAbout     = "Ask the people in your thread a question on a card (thread agents)"
	AskHumanLongAbout = "Ask the people in your thread a question on a card (thread agents).\n\n" +
		"Only a thread agent asks people, and always on a card: a lead puts its questions to the " +
		"thread agent with `fleet ask-thread`, a worker to its lead. --card is a file holding the " +
		"question as JSON (" + cardShape + "). fleet records it as a pending question of your thread " +
		"(table `questions`) and posts it with `fednet client ask -ref <scope>:<question id> -card " +
		"<file>`; fednet checks the card (its fields and their limits are fednet's, see `fednet " +
		"client ask --help`) and refuses one over a limit, naming the field, the limit and the " +
		"length, never cutting it. fleet prints the question's id and the card's link. With --for " +
		"<id>, the card asks for the lead's question <id> pending in your thread: its answers go to " +
		"that lead too, and until the card is settled your `fleet send` to that lead does not mark " +
		"the lead's question answered.\n\n" +
		"Only a press on the card answers it; a message in the thread does not. Each press comes " +
		"back through `fleet inbox` as `[ANSWER #<id>]` and the answer as JSON: " + answerShape + ". " +
		"A card a person closed or answered is no longer pending; a snoozed one stays pending, " +
		"and is not reminded of until it comes back.\n\n" +
		"--close <id> takes back a card of yours that still waits (`fednet client ask-close`, " +
		"with --note shown on it): the question is closed, and nothing more comes from it.\n\n" +
		"Asking again with the same card while it is pending is the same question: a card fednet " +
		"has is not posted again. When fednet cannot be reached or fails, the question stays " +
		"pending; ask again to post it. When fednet refuses the card (its exit 2: a card over a " +
		"limit, not JSON, or a hub older than the client), nothing was posted and the question is " +
		"closed; fix the card and ask again.\n\n" +
		"Deprecated, for agents started on an older prompt: --file <file> is a plain-text " +
		"question. A thread agent's is posted as a card with no options (only the note box and " +
		"\"other\"): its first line as the question, or a fixed title when that line is longer than " +
		"fednet's limit, and the whole text as the context; fednet refuses a text over its limit. " +
		"A lead's goes on as `fleet ask-thread` does, the text as it is. --approval is refused " +
		"(approval cards are not supported yet).\n\n" +
		"Exit: 0 when the card is posted (or was already), or taken back; 1 for a caller that is " +
		"not a thread agent (a lead with --file: as `fleet ask-thread`), --approval, an unreadable or empty card, a --for that is not " +
		"a lead's question pending in your thread, a --close that is not your pending card, or " +
		"no fednet socket; fednet's exit code, with its stderr, when `fednet client ask` or " +
		"`ask-close` fails; 5 when fednet cannot run or the database fails."
	AskThreadAbout     = "Put a question for the people to your home thread's agent, as a card (leads)"
	AskThreadLongAbout = "Put a question for the people to your home thread's agent, as a card (leads).\n\n" +
		"A lead does not talk to people: only the thread agent does. --card is a file holding the " +
		"question as JSON, the same card `fleet ask-human` posts (" + cardShape + "; fednet checks it " +
		"when the thread agent posts it). It is recorded as pending in the ledger (table " +
		"`questions`) and delivered to the job's home thread (`jobs.home_thread`) as `fleet " +
		"inbox` delivers a message: to the thread's live agent, headed `[FROM: <you>]`, or to a " +
		"thread agent started for the thread (its ticket claimed again, the question as the " +
		"session's first message). The message starts `[QUESTION #<id> from <you>]`, then the " +
		"JSON; fleet posts nothing to the thread. The thread agent answers it itself with " +
		"`fleet send`, which marks your question answered, or posts the card with `fleet " +
		"ask-human --card <file> --for <id>`: then each press on that card reaches you too, as " +
		"`[ANSWER #<id>]` with your question's id and the answer as JSON (" + answerShape + "), and " +
		"an answered or closed card answers your question. A message in the thread answers " +
		"nothing.\n\n" +
		"A single-repo job without a home thread (started by a caller without FLEET_ROLE) gets " +
		"one at its lead's first ask: fleet opens a thread with `fednet client open-thread` in " +
		"the channel of the job's repo (`<repo prefix><repo>`, or the initiative's channel for " +
		"an initiative's repo run in the initiative's checkout), or, when fleet knows no id " +
		"of that channel, in the general initiative's channel (`<initiative prefix><general " +
		"initiative>`). fleet takes " +
		"a channel's id from the newest thread of it this scope has seen, or else from fednet's " +
		"routes (`fednet client routes -json`), the route with the channel's name (direct " +
		"messages left out in both); routes fednet cannot give count as naming none. The thread " +
		"opens with fleet's one line naming the job and its repo, not the question. It is " +
		"recorded as the job's home thread, and the question goes on as above. Until it is " +
		"recorded nothing of the question is.\n\n" +
		"The delivery is recorded in the ledger (table `steps`), so running it again with the " +
		"same card while it is pending continues where it stopped and never delivers it twice.\n\n" +
		"Exit: 0 when the thread agent has the question; 1 when the caller is not a lead, the " +
		"file is unreadable or empty, the job is not open, a cross-repo job has no home thread (it " +
		"was started by a caller without FLEET_THREAD), fleet knows neither channel to open " +
		"a single-repo job's home thread in, or the scope has no fednet socket; " +
		"2/3/4 as `send` for the delivery; 3 when a new thread agent stops at an unknown " +
		"screen; 5 when opening the thread fails, or herdr, atb or the database fails. A " +
		"thread agent that has to be started while Linear is unavailable starts without its " +
		"ticket, as `fleet inbox` starts one. After a failure " +
		"the question stays pending; ask again to finish."
)

// cardShape and answerShape are the card's JSON and the answer's, as the
// help texts give them; the limits are fednet's.
const (
	cardShape = `{"question": "one sentence", "context": "Markdown, optional", "options": [{"short": ` +
		`"the radio button's text", "long": "Markdown, optional", "recommended": true}]}, at most one ` +
		`option recommended; the card adds "other", a note box, submit, snooze and close`
	answerShape = `{"question_id": <id>, "card_id": "<fednet's id>", "outcome": "answered" | "snoozed" | ` +
		`"closed" | "resurfaced", "choice": {"index": <from 0>, "short": "..."} or null for "other", ` +
		`"note": "...", "snooze_until": "<RFC 3339>", "by": "<Slack user>", "at": "<RFC 3339>"}; answered ` +
		`is a submit, snoozed a snooze (the card comes back at snooze_until), closed a person's ` +
		`close (no need to answer: drop it or ask another way), resurfaced a snoozed card back in ` +
		`front of the people (no choice, note or by)`
)

// AskHumanArgs are the arguments of `ask-human`.
type AskHumanArgs struct {
	// Card holds the question as JSON; For is the lead's question it
	// asks for, 0 for none.
	Card string
	For  int64
	// Close is the question whose card is taken back, with Note shown on
	// it.
	Close int64
	Note  string
	// File is a plain-text question, kept for agents started on an older
	// prompt (deprecated): a lead's goes on as `ask-thread`, a thread
	// agent's is posted as a card with no options.
	File string
	// Approval is refused: approval cards are not supported yet.
	Approval bool
}

// AskThreadArgs are the arguments of `ask-thread`.
type AskThreadArgs struct {
	// Card holds the question as JSON.
	Card string
}

// AskHuman runs `ask-human`.
func AskHuman(h *herdr.Herdr, args AskHumanArgs) (exit.Code, error) {
	me, err := identity.FromEnv()
	if err != nil {
		return 0, err
	}
	if args.Approval {
		return 0, exit.Refusedf("approval cards are not supported yet; ask on a card with --card, without --approval")
	}
	if err := notAsker(me, "ask-human", args.File != ""); err != nil {
		return 0, err
	}
	if args.File != "" && me.Role == identity.Lead {
		return askThread(h, me, args.File, false)
	}
	if me.Thread == "" {
		return 0, exit.Refusedf("FLEET_THREAD is not set: a thread agent asks in its own thread")
	}
	cfg, err := config.LoadScope(me.Scope)
	if err != nil {
		return 0, err
	}
	if cfg.FednetSocket == "" {
		return 0, exit.Refusedf("fednet.socket is not configured for scope %s, so no card can reach the thread", me.Scope)
	}
	conn, err := db.Open(me.Scope)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if args.Close != 0 {
		return withdraw(conn, cfg.FednetSocket, me, args.Close, args.Note)
	}
	return askCard(conn, cfg.FednetSocket, me, args)
}

// askCard records a thread agent's card and posts it, unless an earlier
// run did.
func askCard(conn *sql.DB, socket string, me *identity.Identity, args AskHumanArgs) (exit.Code, error) {
	cardFile := args.Card
	if args.File != "" {
		text, err := readCard(args.File)
		if err != nil {
			return 0, err
		}
		card, err := textCard(text, "")
		if err != nil {
			return 0, err
		}
		path, remove, err := tempFile(card)
		if err != nil {
			return 0, err
		}
		defer remove()
		cardFile = path
	}
	text, err := readCard(cardFile)
	if err != nil {
		return 0, err
	}
	if err := leadQuestionPending(conn, args.For, me.Thread); err != nil {
		return 0, err
	}
	id, err := pendingQuestion(conn, "", me.Thread, me.Agent, text, args.For, true)
	if err != nil {
		return 0, err
	}
	var cardID, link string
	if err := conn.QueryRow("SELECT card_id, permalink FROM questions WHERE id = ?1", id).Scan(&cardID, &link); err != nil {
		return 0, exit.Database(err)
	}
	if cardID != "" {
		fmt.Fprintf(os.Stdout, "question #%d was already asked in thread %s; nothing done: %s\n", id, me.Thread, link)
		return exit.Ok, nil
	}
	card, stderr, code, err := fednet.Ask(socket, me.Thread, questionRef(me.Scope, id), cardFile)
	if err != nil {
		return 0, exit.New(failureCode(err), err.Error()+fmt.Sprintf("; question #%d is recorded, run ask-human again to post it", id))
	}
	if code != 0 {
		return notAsked(conn, id, code, stderr)
	}
	if _, err := conn.Exec("UPDATE questions SET card_id = ?1, permalink = ?2 WHERE id = ?3", card.ID, card.Permalink, id); err != nil {
		return 0, exit.Database(err)
	}
	fmt.Fprintf(os.Stdout, "asked question #%d in thread %s: %s\n", id, me.Thread, card.Permalink)
	return exit.Ok, nil
}

// leadQuestionPending refuses a --for (`id`, 0 for none) that is not a
// lead's question pending in the thread.
func leadQuestionPending(conn *sql.DB, id int64, thread string) error {
	if id == 0 {
		return nil
	}
	var n int
	if err := conn.QueryRow("SELECT count(*) FROM questions WHERE id = ?1 AND thread = ?2 AND job != '' AND state = 'pending'",
		id, thread).Scan(&n); err != nil {
		return exit.Database(err)
	}
	if n == 0 {
		return exit.Refusedf("question #%d is not a lead's question pending in thread %s", id, thread)
	}
	return nil
}

// notAsked hands back fednet's failure to post question `id`'s card: its
// exit code and stderr. A card fednet refused (exit 2) posted nothing, so
// the question is closed; after any other failure it stays pending.
func notAsked(conn *sql.DB, id int64, code int, stderr []byte) (exit.Code, error) {
	_, _ = os.Stderr.Write(stderr)
	if code != 2 {
		fmt.Fprintf(os.Stderr, "fleet: question #%d is recorded, run ask-human again to post it\n", id)
		return exit.Code(code), nil
	}
	if _, err := conn.Exec("UPDATE questions SET state = 'closed' WHERE id = ?1", id); err != nil {
		return 0, exit.Database(err)
	}
	fmt.Fprintf(os.Stderr, "fleet: fednet refused the card, so nothing was posted and question #%d is closed; fix the card and ask again\n", id)
	return exit.Code(code), nil
}

// AskThread runs `ask-thread`.
func AskThread(h *herdr.Herdr, args AskThreadArgs) (exit.Code, error) {
	me, err := identity.FromEnv()
	if err != nil {
		return 0, err
	}
	if err := notAsker(me, "ask-thread", false); err != nil {
		return 0, err
	}
	return askThread(h, me, args.Card, true)
}

// askThread puts a lead's question, a card or (`card` false) the plain
// text of an older prompt's `ask-human --file`, to its home thread's
// agent.
func askThread(h *herdr.Herdr, me *identity.Identity, file string, card bool) (exit.Code, error) {
	text, err := readCard(file)
	if err != nil {
		return 0, err
	}
	conn, err := db.Open(me.Scope)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	thread, job, cfg, err := leadThread(conn, me)
	if err != nil {
		return 0, err
	}
	id, err := pendingQuestion(conn, job, thread, me.Agent, text, 0, card)
	if err != nil {
		return 0, err
	}
	msg := inboundMessage{Thread: thread, Text: text, Question: me.Agent, QuestionID: id}
	delivered, err := deliverQuestion(h, conn, fmt.Sprintf("ask-human:%d", id), me.Scope, cfg, msg)
	if err != nil {
		return 0, err
	}
	if !delivered {
		fmt.Fprintf(os.Stdout, "question #%d was already delivered to the agent of thread %s; nothing done\n", id, thread)
		return exit.Ok, nil
	}
	fmt.Fprintf(os.Stdout, "gave question #%d to the agent of thread %s, which settles it or asks the people\n", id, thread)
	return exit.Ok, nil
}

// textTitle is a plain-text question's header on its card when its first
// line does not fit; maxTitle and maxContext are fednet's limits on a
// card's question and context, which fleet heeds only in what it writes
// itself.
const (
	textTitle  = "agent 有一个问题，详见下方"
	maxTitle   = 140
	maxContext = 3000
)

// cardOption is an option of a card fleet writes itself.
type cardOption struct {
	Short       string `json:"short"`
	Long        string `json:"long,omitempty"`
	Recommended bool   `json:"recommended,omitempty"`
}

// encodeCard is the JSON of a card fleet writes itself; fednet checks it.
func encodeCard(question, context string, options []cardOption) (string, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(struct {
		Question string       `json:"question"`
		Context  string       `json:"context,omitempty"`
		Options  []cardOption `json:"options,omitempty"`
	}{question, context, options}); err != nil {
		return "", exit.Environmentf("cannot write the card: %v", err)
	}
	return strings.TrimSpace(b.String()), nil
}

// textCard is the card a plain-text question is asked on: its first line
// as the question, or textTitle when that line is too long, `header` (if
// any) and the whole text as the context, no options. fednet checks the
// rest, and refuses a text over its limit for a context.
func textCard(text, header string) (string, error) {
	title, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	title = strings.TrimSpace(title)
	if utf8.RuneCountInString(title) > maxTitle {
		title = textTitle
	}
	return encodeCard(title, withHeader(header, text), nil)
}

// withHeader is `context` with fleet's `header` before it, or `context`
// alone when the header would take it over fednet's limit: what fleet
// adds never gets a text refused that fits on its own.
func withHeader(header, context string) string {
	if header == "" {
		return context
	}
	if context == "" {
		return header
	}
	joined := header + "\n\n" + context
	if utf8.RuneCountInString(joined) > maxContext && utf8.RuneCountInString(context) <= maxContext {
		return context
	}
	return joined
}

// questionCard is the card a watch worker's question is asked on: the
// card its question file holds, `header` put before its context, or for
// a file that holds no card, textCard of its text.
func questionCard(text, header string) (string, error) {
	var card map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &card) != nil || card == nil {
		return textCard(text, header)
	}
	var context string
	if raw, ok := card["context"]; ok && json.Unmarshal(raw, &context) != nil {
		// Not a string: fednet refuses it as it is.
		return text, nil
	}
	raw, err := json.Marshal(withHeader(header, context))
	if err != nil {
		return "", exit.Environmentf("cannot write the card: %v", err)
	}
	card["context"] = raw
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(card); err != nil {
		return "", exit.Environmentf("cannot write the card: %v", err)
	}
	return strings.TrimSpace(b.String()), nil
}

// notAsker refuses the callers `command` is not for: a worker, and for
// `ask-human` a lead, for `ask-thread` a thread agent.
// A lead's `ask-human --file` (`text`) is an older prompt's ask-thread.
func notAsker(me *identity.Identity, command string, text bool) error {
	switch {
	case me.Role == identity.Worker && me.Parent == watchSender:
		return exit.Refusedf("a worker does not ask people; fleet's watch started you, so write your question " +
			"to the question file your task names and end your turn")
	case me.Role == identity.Worker:
		return exit.Refusedf("a worker does not ask people; tell your lead with `fleet send %s`", me.Parent)
	case me.Role == identity.Lead && command == "ask-human" && !text:
		return exit.Refusedf("a lead does not ask people itself; put the question to your home thread's agent with " +
			"`fleet ask-thread --card <file>`")
	case me.Role == identity.Thread && command == "ask-thread":
		return exit.Refusedf("a thread agent asks the people itself, with `fleet ask-human --card <file>`")
	}
	return nil
}

// readCard is the card file's text, its end trimmed; fednet checks the
// rest.
func readCard(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", exit.Refusedf("cannot read %s: %v", path, err)
	}
	text := strings.TrimRight(string(data), " \t\r\n")
	if strings.TrimSpace(text) == "" {
		return "", exit.Refusedf("the card file is empty")
	}
	return text, nil
}

// questionRef is the ref a card is asked with: one fednet client serves
// every scope of the machine, and its answers carry nothing but the ref.
func questionRef(scope string, id int64) string {
	return fmt.Sprintf("%s:%d", scope, id)
}

// failureCode is the exit code of a failure, 5 for any other error.
func failureCode(err error) exit.Code {
	var failure *exit.Failure
	if errors.As(err, &failure) {
		return failure.Code
	}
	return exit.Environment
}

// withdraw takes back the card of the caller's pending question `id`.
// A question whose card was never posted is only closed.
func withdraw(conn *sql.DB, socket string, me *identity.Identity, id int64, note string) (exit.Code, error) {
	var cardID string
	err := conn.QueryRow("SELECT card_id FROM questions WHERE id = ?1 AND thread = ?2 AND asked_by = ?3 AND card = 1 "+
		"AND state = 'pending'", id, me.Thread, me.Agent).Scan(&cardID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, exit.Refusedf("question #%d is not a card of yours pending in thread %s", id, me.Thread)
	}
	if err != nil {
		return 0, exit.Database(err)
	}
	if cardID != "" {
		code, reason, err := fednet.AskClose(socket, cardID, note)
		if err != nil {
			return 0, err
		}
		if code != 0 {
			fmt.Fprintf(os.Stderr, "fleet: fednet client ask-close: %s\n", reason)
			return exit.Code(code), nil
		}
	}
	if _, err := conn.Exec("UPDATE questions SET state = 'closed', outcome = 'withdrawn' WHERE id = ?1", id); err != nil {
		return 0, exit.Database(err)
	}
	fmt.Fprintf(os.Stdout, "took back question #%d in thread %s\n", id, me.Thread)
	return exit.Ok, nil
}

// closeCards takes back from Slack the cards of the questions `ids`, which
// fleet is closing, and those asked for them that still wait, which it
// closes too, so nobody presses a card no agent waits on. A failure is
// only a warning: the questions close all the same.
func closeCards(conn *sql.DB, socket string, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	in := "(?" + strings.Repeat(", ?", len(ids)-1) + ")"
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	var cards []struct {
		id     int64
		cardID string
	}
	err := eachRow(conn, "SELECT id, card_id FROM questions WHERE card_id != '' AND (id IN "+in+
		" OR (for_question IN "+in+" AND state = 'pending')) ORDER BY id", func(rows *sql.Rows) error {
		var c struct {
			id     int64
			cardID string
		}
		if err := rows.Scan(&c.id, &c.cardID); err != nil {
			return err
		}
		cards = append(cards, c)
		return nil
	}, append(args, args...)...)
	if err != nil {
		return err
	}
	for _, c := range cards {
		if socket == "" {
			fmt.Fprintf(os.Stderr, "fleet: warning: the card of question #%d stays in Slack: no fednet socket\n", c.id)
			continue
		}
		code, reason, err := fednet.AskClose(socket, c.cardID, "fleet closed this question")
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "fleet: warning: the card of question #%d stays in Slack: %v\n", c.id, err)
		case code != 0:
			fmt.Fprintf(os.Stderr, "fleet: warning: the card of question #%d stays in Slack: fednet client ask-close "+
				"exit status %d: %s\n", c.id, code, reason)
		}
	}
	_, err = conn.Exec("UPDATE questions SET state = 'closed' WHERE for_question IN "+in+" AND state = 'pending'", args...)
	return dbErr(err)
}

// deliverQuestion is the `deliver` step of a lead's ask: true when it ran
// now, false when an earlier run recorded it.
func deliverQuestion(h *herdr.Herdr, conn *sql.DB, key, scope string, cfg *config.Scope, msg inboundMessage) (bool, error) {
	ran := false
	err := runStep(conn, key, "deliver", func() error {
		ran = true
		code, err := toThread(h, conn, scope, cfg, msg)
		if err == nil && code != exit.Ok {
			err = exit.New(code, fmt.Sprintf("did not reach the agent of thread %s (exit %d)", msg.Thread, code))
		}
		return err
	})
	var failure *exit.Failure
	if errors.As(err, &failure) {
		return false, exit.New(failure.Code, failure.Message+"; the question is recorded, run ask-thread again to deliver it")
	}
	return ran, err
}

// leadThread is the thread a lead asks in, as askedIn names it, with
// the scope's settings, which must name fednet's socket; a single-repo
// job without a home thread gets one opened first.
func leadThread(conn *sql.DB, me *identity.Identity) (thread, job string, cfg *config.Scope, err error) {
	thread, job, row, err := askedIn(conn, me)
	if err != nil {
		return "", "", nil, err
	}
	if cfg, err = config.LoadScope(me.Scope); err != nil {
		return "", "", nil, err
	}
	if cfg.FednetSocket == "" {
		return "", "", nil, exit.Refusedf("fednet.socket is not configured for scope %s, so the question cannot reach the thread", me.Scope)
	}
	if thread == "" {
		if thread, err = openHomeThread(conn, cfg, row); err != nil {
			return "", "", nil, err
		}
		fmt.Fprintf(os.Stdout, "opened thread %s as the home thread of job %s\n", thread, job)
	}
	return thread, job, cfg, nil
}

// askedIn is the thread a lead asks in, its open job's home thread, with
// the job. The thread is empty for a single-repo job without a home
// thread, which gets one when its lead first asks; `row` is the job.
func askedIn(conn *sql.DB, me *identity.Identity) (thread, job string, row *jobRow, err error) {
	if me.Role != identity.Lead {
		return "", "", nil, exit.Refusedf("FLEET_ROLE is not lead: only a lead puts a question to its home thread's agent")
	}
	if me.Job == "" {
		return "", "", nil, exit.Refusedf("FLEET_JOB is not set")
	}
	row, err = openJob(conn, me.Job)
	if err != nil {
		return "", "", nil, err
	}
	if row == nil {
		return "", "", nil, exit.Refusedf("job %s is not open in scope %s", me.Job, me.Scope)
	}
	if row.HomeThread == "" && row.Repo == "" {
		return "", "", nil, exit.Refusedf("job %s has no home thread: it is a cross-repo job started by a caller "+
			"without FLEET_THREAD, so there is no thread to ask in", me.Job)
	}
	return row.HomeThread, me.Job, row, nil
}

// openHomeThread opens a thread for the questions of a single-repo job
// without a home thread, in its repo's channel, or the general
// initiative's when fleet knows no id of the repo's, and records it as
// the job's home thread and as a thread of that channel, so its agent
// starts in that channel's directory, and the next lookup finds the
// channel in the ledger. A channel's id is the one of the newest thread
// this scope has seen there, or else the one fednet's routes give the
// channel's name; direct messages, which have the general mapping too,
// are left out: their Slack ids start with D, and no thread opens there.
// Routes fednet cannot give count as naming no channel, with a line on
// stderr. The thread opens with a line of fleet's, since fednet opens no
// thread without a message; the question is the thread agent's to ask.
// Until the thread is recorded nothing of the question is.
func openHomeThread(conn *sql.DB, cfg *config.Scope, row *jobRow) (string, error) {
	tried := []string{jobMapping(cfg, row.Repo, row.LeadCwd), generalMapping(cfg.Channels)}
	var routes map[string]string
	var mapping, channel string
	for i, m := range tried {
		mapping = m
		err := conn.QueryRow("SELECT channel FROM threads WHERE mapping = ?1 AND channel != '' AND channel NOT LIKE 'D%' "+
			"ORDER BY id DESC LIMIT 1",
			mapping).Scan(&channel)
		if err == nil {
			break
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", exit.Database(err)
		}
		if i == 0 {
			if routes, err = fednet.Channels(cfg.FednetSocket); err != nil {
				fmt.Fprintf(os.Stderr, "fleet: %v; channel ids are taken from the ledger only\n", err)
			}
		}
		if channel = routes[mapping]; channel != "" {
			break
		}
	}
	if channel == "" {
		return "", exit.Refusedf("job %s has no home thread, and fleet cannot open one: no thread of #%s or #%s has "+
			"reached this scope, and fednet's routes name neither, so their ids are unknown (a message to the bot in "+
			"either, or a route with the channel's name, makes it known); nothing is recorded, run ask-thread again then",
			row.Job, tried[0], tried[1])
	}
	text := fmt.Sprintf("Thread of job %s in %s, which started without one.", row.Job, row.Repo)
	thread, err := fednet.OpenThread(cfg.FednetSocket, channel, text)
	var failure *exit.Failure
	if errors.As(err, &failure) {
		return "", exit.New(failure.Code, failure.Message+"; nothing is recorded, run ask-thread again")
	}
	if err != nil {
		return "", err
	}
	return thread, reserve(conn, func(querier) error { return nil }, func(q querier) error {
		if _, err := q.Exec("INSERT INTO threads (thread, slug, channel, mapping, cwd, created_at) VALUES (?1, ?2, ?3, ?4, ?5, ?6)",
			thread, ThreadSlug(thread), channel, mapping, MappingDir(cfg, mapping), db.Now()); err != nil {
			return exit.Database(err)
		}
		if _, err := q.Exec("UPDATE jobs SET home_thread = ?1 WHERE job = ?2 AND state = 'open'", thread, row.Job); err != nil {
			return exit.Database(err)
		}
		return nil
	})
}

// pendingQuestion is the id of the pending question with this text from
// this asker in this thread, a card or not, for the lead's question `forID` (0 for none),
// recorded now unless an earlier run did.
func pendingQuestion(conn *sql.DB, job, thread, by, text string, forID int64, card bool) (int64, error) {
	var id int64
	forQuestion := sql.NullInt64{Int64: forID, Valid: forID != 0}
	err := conn.QueryRow("SELECT id FROM questions WHERE job = ?1 AND thread = ?2 AND asked_by = ?3 AND text = ?4 "+
		"AND card = ?6 AND for_question IS ?5 AND state = 'pending' ORDER BY id DESC LIMIT 1", job, thread, by, text,
		forQuestion, card).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, exit.Database(err)
	}
	res, err := conn.Exec("INSERT INTO questions (job, thread, asked_by, text, approval, state, asked_at, card, for_question) "+
		"VALUES (?1, ?2, ?3, ?4, 0, 'pending', ?5, ?7, ?6)", job, thread, by, text, db.Now(), forQuestion, card)
	if err != nil {
		return 0, exit.Database(err)
	}
	if id, err = res.LastInsertId(); err != nil {
		return 0, exit.Database(err)
	}
	return id, nil
}

// toThread takes an agent's message to a thread as `route` does, with a
// dropped message an environment failure: unlike fednet, the caller gets
// nothing from a drop.
func toThread(h *herdr.Herdr, conn *sql.DB, scope string, cfg *config.Scope, msg inboundMessage) (exit.Code, error) {
	code, dropped, err := route(h, conn, scope, cfg, msg)
	if err != nil || code != exit.Ok {
		return code, err
	}
	if dropped {
		return 0, exit.Environmentf("no thread agent could be started for %s; try again later", msg.Thread)
	}
	return exit.Ok, nil
}

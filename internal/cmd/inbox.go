// `fleet inbox <event-file>`: the fednet client's hook. One message from a
// person, routed to its thread's agent, which is started when needed.

package cmd

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
	"github.com/Luolc/agent-fleet/internal/identity"
)

// InboxAbout and InboxLongAbout are the help texts of `inbox`.
const (
	InboxAbout     = "Take one message from the fednet client (its hook) to the thread's agent, starting it when needed"
	InboxLongAbout = "Take one message from the fednet client (its hook) to the thread's agent, starting it when needed.\n\n" +
		"The fednet client runs `fleet inbox <event-file>` for every message a person posts in " +
		"a thread this machine owns. The file is one JSON object: `msg_id` and `payload`; a " +
		"payload of type `message` carries `thread` (CHANNEL/TS), `text`, `user`, `ts`, for a " +
		"new thread the channel's `context`, and optional fields: `trigger` (`dm` for a " +
		"direct message to the bot; `handoff` and `relay` below), `channel_name` and `scope`. " +
		"A message of trigger `handoff` (a thread another machine opened for this one with work) " +
		"or `relay` (a post an agent on another machine delivered here with `fleet thread post " +
		"--deliver`) is an agent's words: it reaches the thread's agent headed `[FROM: " +
		"<from_client>/<agent>]` and `[HANDOFF from …]` or `[RELAY from …]` with ` in thread " +
		"<key>`, the agent and thread read from the text's last line `— <agent> · <thread key>` " +
		"(labels the sender gives; without that line the machine alone), is never recorded as " +
		"the latest message a person posted and answers no question; `from_client` is the " +
		"machine as the hub authenticated it. A payload of type `answer` is a " +
		"press on a question card (below). A payload of type `reaction` is an emoji a person put on " +
		"a thread's root message (`thread`, `user`, `reaction`, and `scope` as a message's): " +
		"`package` (the people archived the thread) takes fleet's `eyes` off a thread the scope " +
		"knows, any other is ignored, and no agent hears of it. Any other payload type is ignored, " +
		"exit 0.\n\n" +
		"fleet keeps emoji on a thread's root message (`fednet client react`, with the scope's " +
		"socket): one of `hourglass_flowing_sand` (a session or a job runs), `question` (a card " +
		"waits for the people) and `white_check_mark` (neither), and `eyes` from each delivered " +
		"message until the people archive the thread. One that fednet fails to put is a warning " +
		"on stderr, never the exit code; `fleet watch` puts it later.\n\n" +
		"An answer's `answer` is fednet's JSON, its `question_id` the ref fleet asked with, " +
		"`<scope>:<question id>`, and `card_id` the card's; one whose ref is not fleet's, or whose " +
		"question the scope's ledger has not with that card, is ignored, exit 0. Once its msg_id is " +
		"reserved, the press is recorded on the question: `answered` and `closed` (a person's) " +
		"answer it, and the lead's question it was asked for (`fleet ask-human --for`); `snoozed` " +
		"keeps it pending until `snooze_until`, `resurfaced` counts its reminders again. Then " +
		"`[ANSWER #<id>]` and the answer, its `question_id` the question's id, go to the thread's " +
		"agent as a message does (one is started when none is live), and, for a card asked for a " +
		"lead's question, to that lead under that question's id, when it is live. A press on a " +
		"question fleet has closed (taken back, or given up on by `fleet watch`) is recorded and " +
		"given to no agent. A press on a card fleet posted for a watch worker, a screen or a " +
		"watch's `max` reaches the thread's agent naming who asked, and is kept on the question " +
		"for the next worker (`fleet watch --help`). Messages in a thread answer only the " +
		"questions an older fleet posted there as text and the questions of a lead whose job has " +
		"ended that no pending card asks for: only a press answers a card, and only the thread " +
		"agent an open job's lead's question.\n\n" +
		"Which messages fleet serves, by channel: `<repo prefix><R>` (thread agents run in the " +
		"main checkout <checkouts>/<R>), `<initiative prefix><I>` (in <initiatives>/<I>/, the " +
		"checkout of the initiative's repo) and direct messages (in the general initiative's " +
		"checkout). The prefixes, the general initiative and the directories are settings " +
		"(below); by default `repo-<R>` runs in ~/dev/<R>, `x-repo-<I>` in ~/x-repo/<I>/, and " +
		"direct messages in ~/x-repo/general/. A " +
		"message from any other channel, or with no channel_name and not a direct message, is " +
		"ignored with exit 0 and no reply, unless the ledger already knows its thread. A thread " +
		"keeps the channel and directory recorded at its first delivery, even when the channel " +
		"is renamed. When the directory does not exist, nothing is started, one line saying " +
		"the repo is not checked out on this machine is posted to the thread and the message " +
		"is dropped, exit 0.\n\n" +
		"The scope is the payload's `scope`, `main` when absent ([a-z0-9-]): the herdr session " +
		"fleet-<scope>, the ledger $XDG_STATE_HOME/fleet/<scope>.db (~/.local/state when unset) " +
		"and the settings $XDG_CONFIG_HOME/fleet/<scope>.json (~/.config when unset): `linear` " +
		"({\"team\": ...}; absent: thread tickets are off, threads still run) and `fednet` " +
		"({\"socket\": ...}; absent: fleet cannot post to the thread), `paths` (`checkouts`, " +
		"`initiatives`, `worktrees`, `scratch`: absolute or ~/ paths, default ~/dev, ~/x-repo, " +
		"~/wt, ~/scratch) and `channels` (`repo_prefix`, `initiative_prefix`, " +
		"`general_initiative`: default repo-, x-repo-, general); each key left out keeps its " +
		"default. A recorded thread's kind (repo or initiative) is read with the current " +
		"prefixes, so change `channels` only while no thread is recorded. --scope does not " +
		"apply.\n\n" +
		"First the msg_id is reserved in the scope's ledger (table `inbox`): a message already " +
		"delivered or dropped is exit 0 at once, so a retry does nothing twice. Then the route: " +
		"a thread whose agent is live gets the message as `fleet send` would, headed `[FROM: " +
		"inbox]`. Otherwise a thread agent is started: `thread-<slug>` (the slug is the key, " +
		"lower-cased, [a-z0-9-]) in a tab of the herdr workspace `threads`, in the thread's " +
		"directory, with FLEET_ROLE=thread, FLEET_THREAD, FLEET_SCOPE, FLEET_ISSUE and FLEET_SCRATCH. Its row is reserved first (one live agent " +
		"per thread), so a rerun after a kill cannot start a second one. With a Linear team, a " +
		"new thread gets a thread ticket (label `thread`, no project, description with the " +
		"channel, thread and first message) claimed for the agent; a known thread gets its " +
		"ticket claimed again, a `Session <n> started` comment, and its earlier session " +
		"summaries. A Linear step atb fails is tried twice more, after 3 s and 7 s (not a " +
		"call that timed out, nor a claim another agent holds, atb's exit 3); each failed try " +
		"is logged with atb's exit code and its stderr, credentials redacted, as one JSON " +
		"line in $XDG_STATE_HOME/fleet/<scope>-linear.log. When a step still fails, the " +
		"steps after it are skipped and the agent starts anyway: before the ticket is " +
		"claimed, the session runs without it (FLEET_ISSUE empty), the thread is told in one " +
		"line (`fednet client post`, as far as it can) and `fleet thread end` or the next " +
		"session catches up; after, it keeps the ticket. Either way its first message says " +
		"what is missing and the message is delivered. Before the start, the thread's " +
		"checkout is fetched from origin and fast-forwarded when it is on origin/HEAD's " +
		"branch, clean and only behind; otherwise it is left and the first message says why. " +
		"Then statute refreshes its private output there as `fleet worktree` does; when it " +
		"does not generate it, the first message says why and how to. " +
		"The agent's first " +
		"message is the thread prompt and `common` (as `fleet prompt show thread` shows them), the channel context, the summaries, fleet's " +
		"notes and the message. A run killed after the agent started leaves its row `starting`: the retry " +
		"finds the agent, gets it to its input box and delivers that first message in full. " +
		"A live row whose agent is gone from herdr is ended (the session ended abnormally) " +
		"and a new agent started.\n\n" +
		"Exit: 0 when the message is delivered, ignored or dropped (fednet marks it delivered); " +
		"1 when the event file cannot be read or is not an event, or the scope is not a scope name, or --scope is given; " +
		"2/3/4 as `send` when the delivery to a live agent gives no clear signal, finds it " +
		"blocked, or does not find it; 3 when a new agent stops at an unknown screen; 5 when " +
		"herdr or the database fails. A non-zero exit makes fednet run the hook again later, " +
		"with the reservation kept: a message that was delivered with no clear signal (exit 2) " +
		"may then be delivered twice."
)

// InboxArgs are the arguments of `inbox`.
type InboxArgs struct {
	// File is the event file fednet wrote.
	File string
}

// event is the event file: fednet's `hook.Event` with the fields of a
// `message` payload.
type event struct {
	MsgID   string `json:"msg_id"`
	Payload struct {
		Type    string `json:"type"`
		Thread  string `json:"thread"`
		Text    string `json:"text"`
		User    string `json:"user"`
		TS      string `json:"ts"`
		Context string `json:"context"`
		// Trigger is `dm` for a direct message to the bot, `handoff` or
		// `relay` for what an agent on another machine passed here.
		Trigger string `json:"trigger"`
		// FromClient is the machine a handoff or relay came from, as the
		// hub authenticated it.
		FromClient string `json:"from_client"`
		// ChannelName is the channel's name, absent in a direct message.
		ChannelName string `json:"channel_name"`
		// Scope is the fleet the hub routes the channel to; absent means
		// the default.
		Scope string `json:"scope"`
		// Answer is a press on a question card, in a payload of type
		// `answer`.
		Answer json.RawMessage `json:"answer"`
		// Reaction is the emoji a person put on a thread's root message,
		// in a payload of type `reaction`.
		Reaction string `json:"reaction"`
	} `json:"payload"`
}

// readEvent reads and checks the event file.
func readEvent(path string) (*event, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, exit.Refusedf("cannot read %s: %v", path, err)
	}
	var e event
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, exit.Refusedf("%s is not an event file: %v", path, err)
	}
	if e.MsgID == "" || e.Payload.Type == "" {
		return nil, exit.Refusedf("%s is not an event file: msg_id and payload.type are required", path)
	}
	return &e, nil
}

// reserveMessage writes the message's row as `reserved`, or reads the
// state of the row an earlier run wrote.
func reserveMessage(conn *sql.DB, msgID, thread string) (state string, err error) {
	err = reserve(conn, func(q querier) error {
		err := q.QueryRow("SELECT state FROM inbox WHERE msg_id = ?1", msgID).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return exit.Database(err)
		}
		return nil
	}, func(q querier) error {
		if state != "" {
			return nil
		}
		if _, err := q.Exec("INSERT INTO inbox (msg_id, thread, state, received_at) VALUES (?1, ?2, 'reserved', ?3)",
			msgID, thread, db.Now()); err != nil {
			return exit.Database(err)
		}
		state = "reserved"
		return nil
	})
	return state, err
}

// setMessage records the message's outcome.
func setMessage(conn *sql.DB, msgID, state string) error {
	if _, err := conn.Exec("UPDATE inbox SET state = ?1 WHERE msg_id = ?2", state, msgID); err != nil {
		return exit.Database(err)
	}
	return nil
}

// herdrHas is whether herdr knows an agent named `name`.
func herdrHas(h *herdr.Herdr, name string) (bool, error) {
	reply, err := h.Call("agent", "get", name)
	if err != nil {
		return false, err
	}
	if reply.Error == nil {
		return true, nil
	}
	if reply.Error.Code == "agent_not_found" {
		return false, nil
	}
	return false, exit.Environmentf("herdr: %s: %s", reply.Error.Code, reply.Error.Message)
}

// Inbox runs `inbox`.
func Inbox(args InboxArgs) (exit.Code, error) {
	e, err := readEvent(args.File)
	if err != nil {
		return 0, err
	}
	if e.Payload.Type == "answer" {
		return inboxAnswer(e)
	}
	if e.Payload.Type == "reaction" {
		return inboxReaction(e)
	}
	if e.Payload.Type != "message" {
		fmt.Fprintf(os.Stdout, "ignored %s: a %s, not a message\n", e.MsgID, e.Payload.Type)
		return exit.Ok, nil
	}
	if e.Payload.Thread == "" {
		return 0, exit.Refusedf("message %s names no thread", e.MsgID)
	}
	scope, err := identity.CheckScope(e.Payload.Scope)
	if err != nil {
		return 0, err
	}
	// The channels fleet serves are in the scope's config.
	cfg, err := config.LoadScope(scope)
	if err != nil {
		return 0, err
	}
	mapping := ChannelMapping(cfg.Channels, e.Payload.ChannelName, e.Payload.Trigger == "dm")
	conn, err := inboxLedger(scope, e.Payload.Thread, mapping)
	if err != nil || conn == nil {
		if err == nil {
			fmt.Fprintf(os.Stdout, "ignored %s: channel %q is neither %s<R> nor %s<I>\n", e.MsgID, e.Payload.ChannelName,
				cfg.Channels.RepoPrefix, cfg.Channels.InitiativePrefix)
		}
		return exit.Ok, err
	}
	defer conn.Close()
	session := identity.Session(scope)
	h := herdr.New(&session)
	state, err := reserveMessage(conn, e.MsgID, e.Payload.Thread)
	if err != nil {
		return 0, err
	}
	if state != "reserved" {
		fmt.Fprintf(os.Stdout, "message %s was already %s\n", e.MsgID, state)
		return exit.Ok, nil
	}
	msg := inboxMessage(e, mapping)
	if err := answerByMessage(conn, msg); err != nil {
		return 0, err
	}
	code, dropped, err := route(h, conn, scope, cfg, msg)
	if err != nil || code != exit.Ok {
		return code, err
	}
	// A person's message makes the thread one to look at again, until
	// they archive it.
	if dropped {
		markThread(conn, cfg.FednetSocket, msg.Thread)
	} else {
		look(conn, cfg.FednetSocket, msg.Thread, true)
	}
	state = "delivered"
	if dropped {
		state = "dropped"
	} else if err := recordLatest(conn, msg); err != nil {
		return 0, err
	}
	return exit.Ok, setMessage(conn, e.MsgID, state)
}

// inboxMessage is the event's message; one of trigger handoff or relay
// carries the machine it came from and the agent and thread its signed
// last line names.
func inboxMessage(e *event, mapping string) inboundMessage {
	msg := inboundMessage{Thread: e.Payload.Thread, Text: e.Payload.Text, User: e.Payload.User, TS: e.Payload.TS,
		Context: e.Payload.Context, Mapping: mapping}
	if t := e.Payload.Trigger; t == relayHandoff || t == relayPost {
		msg.Relay, msg.FromClient = t, e.Payload.FromClient
		msg.RelayAgent, msg.RelayThread = relaySigned(msg.Text)
	}
	return msg
}

// inboxReaction takes a reaction a person put on the root message of a
// thread the scope knows: `package` (archived) takes the eyes mark off,
// any other is ignored. No agent hears of it. Doing it again changes
// nothing, since fednet may hand the same reaction over twice.
func inboxReaction(e *event) (exit.Code, error) {
	if e.Payload.Reaction != archived {
		fmt.Fprintf(os.Stdout, "ignored %s: reaction %q\n", e.MsgID, e.Payload.Reaction)
		return exit.Ok, nil
	}
	if e.Payload.Thread == "" {
		return 0, exit.Refusedf("reaction %s names no thread", e.MsgID)
	}
	scope, err := identity.CheckScope(e.Payload.Scope)
	if err != nil {
		return 0, err
	}
	cfg, err := config.LoadScope(scope)
	if err != nil {
		return 0, err
	}
	conn, err := inboxLedger(scope, e.Payload.Thread, "")
	if err != nil || conn == nil {
		if err == nil {
			fmt.Fprintf(os.Stdout, "ignored %s: scope %s does not know thread %s\n", e.MsgID, scope, e.Payload.Thread)
		}
		return exit.Ok, err
	}
	defer conn.Close()
	look(conn, cfg.FednetSocket, e.Payload.Thread, false)
	fmt.Fprintf(os.Stdout, "thread %s recorded as archived: eyes off\n", e.Payload.Thread)
	return exit.Ok, nil
}

// recordLatest writes the delivered message onto the thread's row as the
// latest a person posted there, for `job start` to hand to the lead; a
// relayed message, an agent's, is not written.
func recordLatest(conn *sql.DB, msg inboundMessage) error {
	if msg.Relay != "" {
		return nil
	}
	if _, err := conn.Exec("UPDATE threads SET last_text = ?1, last_user = ?2, last_ts = ?3 WHERE thread = ?4",
		msg.Text, msg.User, msg.TS, msg.Thread); err != nil {
		return exit.Database(err)
	}
	return nil
}

// inboxLedger opens the scope's ledger for a message, or returns nil when
// the message is ignored: a channel fleet does not serve (`mapping` empty)
// and a thread the ledger does not already know, which leaves no ledger
// behind.
func inboxLedger(scope, thread, mapping string) (*sql.DB, error) {
	if mapping != "" {
		return db.Open(scope)
	}
	path, err := db.Path(scope)
	if err != nil || !exists(path) {
		return nil, err
	}
	conn, err := db.OpenAt(path)
	if err != nil {
		return nil, err
	}
	known, err := threadByKey(conn, thread)
	if err != nil || known == nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// route delivers the message to the thread's live agent, or starts one.
// exit.Ok means the agent has the message, or (`dropped`) that no agent
// was started because the thread's checkout is not on this machine or the
// thread prompt is broken, and the thread was told. A Linear failure
// starts the agent all the same, without what Linear did not do.
// Shared with `ask-human` and the job's conclusion, which reach the
// job's home thread the same way.
func route(h *herdr.Herdr, conn *sql.DB, scope string, cfg *config.Scope, msg inboundMessage) (code exit.Code, dropped bool, err error) {
	name, state, err := liveThreadAgent(conn, msg.Thread)
	if err != nil {
		return 0, false, err
	}
	if name != "" {
		if err := ensureSession(h, scope); err != nil {
			return 0, false, err
		}
		present, err := herdrHas(h, name)
		if err != nil {
			return 0, false, err
		}
		if present && state == "starting" {
			return resumeThreadStart(h, conn, scope, cfg, msg, name)
		}
		if present {
			text, err := WithHeader(msg.sender(), msg.body())
			if err != nil {
				return 0, false, err
			}
			code, err := Deliver(h, name, text)
			return code, false, err
		}
		fmt.Fprintf(os.Stderr, "note: %s is gone from herdr; its session ended abnormally, starting a new one\n", name)
		if err := endRow(conn, name); err != nil {
			return 0, false, err
		}
		// A tab left open does not hold up the message: `fleet watch`
		// closes it later.
		if err := closeOwnTab(h, conn, name); err != nil {
			fmt.Fprintf(os.Stderr, "note: the tab of %s's ended session is not closed: %v\n", name, err)
		}
	}
	return startThread(h, conn, scope, cfg, msg)
}

// startThread starts a thread agent for the message, or tells the thread
// why none was started (`dropped`).
func startThread(h *herdr.Herdr, conn *sql.DB, scope string, cfg *config.Scope, msg inboundMessage) (code exit.Code, dropped bool, err error) {
	s, err := newThreadStart(h, conn, scope, cfg, msg)
	if err != nil {
		return 0, false, err
	}
	if info, err := os.Stat(s.cwd); err != nil || !info.IsDir() {
		return exit.Ok, true, s.notHere()
	}
	if s.text, err = loadPrompt(cfg, "thread"); err != nil {
		return s.promptBroken(err)
	}
	// The session is started only for a thread that gets an agent.
	if err := ensureSession(h, scope); err != nil {
		return 0, false, err
	}
	if err := s.reserve(); err != nil {
		return 0, false, err
	}
	if err := s.linearSteps(); err != nil {
		var failure *exit.Failure
		if !errors.As(err, &failure) || failure.Code != exit.Environment {
			return 0, false, err
		}
		if err := s.withoutLinear(err); err != nil {
			return 0, false, err
		}
	}
	s.checkoutNote = syncCheckout(s.cwd)
	code, err = s.start()
	return code, false, err
}

// answerByMessage marks answered the thread's pending questions a
// person's message answers: those an older fleet posted as text (a watch
// worker's, a screen's, watch's own at a watch's `max`, a thread
// agent's), and a lead's whose job has ended (moved to the thread, its
// lead gone), unless a card asks the people for it. Only a press answers
// a card, and only the thread agent an open job's lead's question. A
// relayed message, an agent's, answers none.
func answerByMessage(conn *sql.DB, msg inboundMessage) error {
	if msg.Relay != "" {
		return nil
	}
	thread := msg.Thread
	var ids []any
	err := eachRow(conn, "SELECT id, job, asked_by, card, job != '' AND NOT EXISTS (SELECT 1 FROM jobs j WHERE "+
		"j.job = q.job AND j.state = 'open' AND j.started_at <= q.asked_at), EXISTS (SELECT 1 FROM questions c "+
		"WHERE c.for_question = q.id AND c.state = 'pending') FROM questions q WHERE thread = ?1 AND state = 'pending'",
		func(rows *sql.Rows) error {
			var q question
			if err := rows.Scan(&q.id, &q.job, &q.askedBy, &q.card, &q.moved, &q.forwarded); err != nil {
				return err
			}
			if !q.byLead() && !q.card || q.byLead() && q.moved && !q.forwarded {
				ids = append(ids, q.id)
			}
			return nil
		}, thread)
	if err != nil || len(ids) == 0 {
		return err
	}
	if _, err := conn.Exec("UPDATE questions SET state = 'answered', answered_at = ?1 WHERE id IN (?"+
		strings.Repeat(", ?", len(ids)-1)+")", append([]any{db.Now()}, ids...)...); err != nil {
		return exit.Database(err)
	}
	fmt.Fprintf(os.Stdout, "%d pending question(s) in thread %s answered\n", len(ids), thread)
	return nil
}

// cardAnswer is fednet's answer to a press on a card, as it reaches the
// agents: `question_id` is the recipient's own question.
type cardAnswer struct {
	QuestionID  any             `json:"question_id"`
	CardID      string          `json:"card_id"`
	Outcome     string          `json:"outcome"`
	Choice      json.RawMessage `json:"choice,omitempty"`
	Note        string          `json:"note,omitempty"`
	SnoozeUntil string          `json:"snooze_until,omitempty"`
	By          string          `json:"by,omitempty"`
	At          string          `json:"at"`
}

// as is the answer's JSON with `id` as its question_id.
func (a cardAnswer) as(id int64) (string, error) {
	a.QuestionID = id
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(a); err != nil {
		return "", exit.Environmentf("cannot write the answer: %v", err)
	}
	return strings.TrimSpace(b.String()), nil
}

// answeredCard is the question a press answers, as the ledger has it.
type answeredCard struct {
	id                             int64
	thread, cardID, state, askedBy string
	forQuestion                    sql.NullInt64
}

// inboxAnswer takes a press on a card to the ledger, then to the thread's
// agent and, for a card asked for a lead's question, to that lead. The
// card's ref is `<scope>:<id>`; a ref fleet did not make, a question the
// ledger does not have, or one whose card is another, is ignored.
func inboxAnswer(e *event) (exit.Code, error) {
	var a cardAnswer
	if err := json.Unmarshal(e.Payload.Answer, &a); err != nil {
		return 0, exit.Refusedf("answer %s is not an answer: %v", e.MsgID, err)
	}
	scope, conn, q, err := answeredQuestion(e.MsgID, a)
	if err != nil || conn == nil {
		return exit.Ok, err
	}
	defer conn.Close()
	state, err := reserveMessage(conn, e.MsgID, q.thread)
	if err != nil {
		return 0, err
	}
	if state != "reserved" {
		fmt.Fprintf(os.Stdout, "message %s was already %s\n", e.MsgID, state)
		return exit.Ok, nil
	}
	if err := recordAnswer(conn, q, a); err != nil {
		return 0, err
	}
	if cfg, err := config.LoadScope(scope); err == nil {
		markThread(conn, cfg.FednetSocket, q.thread)
	}
	if q.state == "closed" {
		fmt.Fprintf(os.Stdout, "question #%d is closed: its %s is recorded and given to no agent\n", q.id, a.Outcome)
		return exit.Ok, setMessage(conn, e.MsgID, "dropped")
	}
	if err := deliverAnswer(conn, scope, "answer:"+e.MsgID, q, a); err != nil {
		return 0, err
	}
	fmt.Fprintf(os.Stdout, "question #%d: %s, given to the agent of thread %s\n", q.id, a.Outcome, q.thread)
	return exit.Ok, setMessage(conn, e.MsgID, "delivered")
}

// answeredQuestion finds the question an answer is for, by its ref
// `<scope>:<id>` and its card: the scope, its ledger and the question, or
// no ledger when the answer is not for one of the scope's questions.
func answeredQuestion(msgID string, a cardAnswer) (string, *sql.DB, answeredCard, error) {
	ref, _ := a.QuestionID.(string)
	scope, idText, _ := strings.Cut(ref, ":")
	id, err := strconv.ParseInt(idText, 10, 64)
	if _, serr := identity.CheckScope(scope); err != nil || serr != nil || scope == "" {
		fmt.Fprintf(os.Stdout, "ignored %s: question %q is not one of fleet's\n", msgID, ref)
		return "", nil, answeredCard{}, nil
	}
	path, err := db.Path(scope)
	if err != nil || !exists(path) {
		if err == nil {
			fmt.Fprintf(os.Stdout, "ignored %s: scope %s has no ledger\n", msgID, scope)
		}
		return "", nil, answeredCard{}, err
	}
	conn, err := db.OpenAt(path)
	if err != nil {
		return "", nil, answeredCard{}, err
	}
	q := answeredCard{id: id}
	err = conn.QueryRow("SELECT thread, card_id, state, asked_by, for_question FROM questions WHERE id = ?1 AND card = 1", id).
		Scan(&q.thread, &q.cardID, &q.state, &q.askedBy, &q.forQuestion)
	if errors.Is(err, sql.ErrNoRows) || err == nil && q.cardID != a.CardID {
		_ = conn.Close()
		fmt.Fprintf(os.Stdout, "ignored %s: scope %s has no question #%d with card %s\n", msgID, scope, id, a.CardID)
		return "", nil, answeredCard{}, nil
	}
	if err != nil {
		_ = conn.Close()
		return "", nil, answeredCard{}, exit.Database(err)
	}
	return scope, conn, q, nil
}

// deliverAnswer gives the answer to the thread's agent, then to the lead
// whose question the card asks for, each a step recorded under `key`.
func deliverAnswer(conn *sql.DB, scope, key string, q answeredCard, a cardAnswer) error {
	cfg, err := config.LoadScope(scope)
	if err != nil {
		return err
	}
	session := identity.Session(scope)
	h := herdr.New(&session)
	err = runStep(conn, key, "thread", func() error {
		text, err := a.as(q.id)
		if err != nil {
			return err
		}
		msg := inboundMessage{Thread: q.thread, Text: text, Answer: true, QuestionID: q.id}
		if (question{askedBy: q.askedBy}).byHelper() {
			msg.AskedBy = q.askedBy
		}
		code, _, err := route(h, conn, scope, cfg, msg)
		if err == nil && code != exit.Ok {
			err = exit.New(code, fmt.Sprintf("the answer to question #%d did not reach the agent of thread %s (exit %d)", q.id, q.thread, code))
		}
		return err
	})
	if err != nil || !q.forQuestion.Valid {
		return err
	}
	return runStep(conn, key, "lead", func() error { return answerToLead(h, conn, q, a) })
}

// recordAnswer writes the press on the question's row, kept as its
// answer, and on the lead's question it asks for: a submit or a person's
// close answers both; a snooze keeps them pending until the card comes
// back, from when their reminders count again.
func recordAnswer(conn *sql.DB, q answeredCard, a cardAnswer) error {
	type stmt struct {
		query string
		args  []any
	}
	answer, err := a.as(q.id)
	if err != nil {
		return err
	}
	stmts := []stmt{{"UPDATE questions SET answer = ?1 WHERE id = ?2", []any{answer, q.id}}}
	switch a.Outcome {
	case "answered", "closed":
		stmts = append(stmts, stmt{"UPDATE questions SET state = CASE WHEN state = 'closed' THEN state ELSE 'answered' END, " +
			"answered_at = COALESCE(answered_at, ?1), outcome = ?2, snooze_until = NULL WHERE id = ?3",
			[]any{db.Now(), a.Outcome, q.id}})
		if q.state != "closed" && q.forQuestion.Valid {
			stmts = append(stmts, stmt{"UPDATE questions SET state = 'answered', answered_at = ?1 WHERE id = ?2 AND state = 'pending'",
				[]any{db.Now(), q.forQuestion.Int64}})
		}
	case "snoozed":
		// A time fleet cannot read leaves the card unsnoozed for watch;
		// refusing would have fednet run the hook again for good.
		until := sql.NullInt64{}
		if t, err := time.Parse(time.RFC3339, a.SnoozeUntil); err == nil {
			until = sql.NullInt64{Int64: t.Unix(), Valid: true}
		} else {
			fmt.Fprintf(os.Stderr, "fleet: the snooze of question #%d has a snooze_until that is not RFC 3339 (%q); "+
				"it is recorded without one\n", q.id, a.SnoozeUntil)
		}
		stmts = append(stmts, stmt{"UPDATE questions SET outcome = ?1, snooze_until = ?2, reminders = 0 WHERE id = ?3",
			[]any{a.Outcome, until, q.id}})
	default:
		stmts = append(stmts, stmt{"UPDATE questions SET outcome = ?1, reminders = 0 WHERE id = ?2", []any{a.Outcome, q.id}})
	}
	for _, st := range stmts {
		if _, err := conn.Exec(st.query, st.args...); err != nil {
			return exit.Database(err)
		}
	}
	return nil
}

// answerToLead gives the press to the lead whose question the card asks
// for, under that question's id; a lead no longer live is told nothing.
func answerToLead(h *herdr.Herdr, conn *sql.DB, q answeredCard, a cardAnswer) error {
	var lead, asker string
	err := conn.QueryRow("SELECT q.asked_by, COALESCE((SELECT name FROM agents WHERE name = q.asked_by AND role = 'lead' "+
		"AND state != 'ended'), '') FROM questions q WHERE q.id = ?1", q.forQuestion.Int64).Scan(&asker, &lead)
	if err != nil {
		return exit.Database(err)
	}
	if lead == "" {
		fmt.Fprintf(os.Stdout, "%s is not live: the answer to its question #%d goes only to the thread's agent\n", asker,
			q.forQuestion.Int64)
		return nil
	}
	text, err := a.as(q.forQuestion.Int64)
	if err != nil {
		return err
	}
	body, err := WithHeader(inboxSender, fmt.Sprintf("[ANSWER #%d]\nA press on the card your home thread's agent "+
		"posted in thread %s for your question #%d. Act on its outcome: answered (the choice and the note), snoozed "+
		"(it comes back at snooze_until), closed (no need to answer: drop it or ask another way), resurfaced (back in "+
		"front of the people: wait).\n\n%s\n", q.forQuestion.Int64, q.thread, q.forQuestion.Int64, text))
	if err != nil {
		return err
	}
	code, err := Deliver(h, lead, body)
	switch {
	case err != nil:
		return err
	case code == exit.NotFound:
		fmt.Fprintf(os.Stdout, "%s is gone: the answer to its question #%d goes only to the thread's agent\n", lead,
			q.forQuestion.Int64)
	case code != exit.Ok:
		return exit.New(code, fmt.Sprintf("the answer to question #%d did not reach %s (exit %d)", q.forQuestion.Int64, lead, code))
	}
	return nil
}

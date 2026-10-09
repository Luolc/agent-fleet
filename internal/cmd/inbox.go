// `fleet inbox <event-file>`: the fednet client's hook. One message from a
// person, routed to its thread's agent, which is started when needed.

package cmd

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"

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
		"new thread the channel's `context`, and three optional fields: `trigger` (`dm` for a " +
		"direct message to the bot), `channel_name` and `scope`. Any other payload type is " +
		"ignored, exit 0.\n\n" +
		"Which messages fleet serves, by channel: `repo-<R>` (thread agents run in ~/dev/<R>), " +
		"`x-repo-<I>` (in ~/x-repo/<I>/, the checkout of the initiative's repo; " +
		"`x-repo-general` is ~/x-repo/general/) and direct messages (~/x-repo/general/). A " +
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
		"({\"socket\": ...}; absent: fleet cannot post to the thread). --scope does not apply.\n\n" +
		"First the msg_id is reserved in the scope's ledger (table `inbox`): a message already " +
		"delivered or dropped is exit 0 at once, so a retry does nothing twice. Then the route: " +
		"a thread whose agent is live gets the message as `fleet send` would, headed `[FROM: " +
		"inbox]`. Otherwise a thread agent is started: `thread-<slug>` (the slug is the key, " +
		"lower-cased, [a-z0-9-]) in a tab of the herdr workspace `threads`, in the thread's " +
		"directory, with FLEET_ROLE=thread, FLEET_THREAD, FLEET_SCOPE and FLEET_ISSUE. Its row is reserved first (one live agent " +
		"per thread), so a rerun after a kill cannot start a second one. With a Linear team, a " +
		"new thread gets a thread ticket (label `thread`, no project, description with the " +
		"channel, thread and first message) claimed for the agent; a known thread gets its " +
		"ticket claimed again, a `Session <n> started` comment, and its earlier session " +
		"summaries. If a Linear step fails, no agent is started, the message is dropped, one " +
		"line saying Linear is unavailable is posted to the thread (`fednet client post`) and " +
		"the exit is 0; when that line cannot be posted (no socket configured, or the post " +
		"fails), the exit is 5 and the message is kept for fednet's retry. The agent's first " +
		"message is the built-in thread prompt, the channel context, the summaries and the " +
		"message. A run killed after the agent started leaves its row `starting`: the retry " +
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
		// Trigger is `dm` for a direct message to the bot.
		Trigger string `json:"trigger"`
		// ChannelName is the channel's name, absent in a direct message.
		ChannelName string `json:"channel_name"`
		// Scope is the fleet the hub routes the channel to; absent means
		// the default.
		Scope string `json:"scope"`
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
	mapping := ChannelMapping(e.Payload.ChannelName, e.Payload.Trigger == "dm")
	conn, err := inboxLedger(scope, e.Payload.Thread, mapping)
	if err != nil || conn == nil {
		if err == nil {
			fmt.Fprintf(os.Stdout, "ignored %s: channel %q is neither repo-<R> nor x-repo-<I>\n", e.MsgID, e.Payload.ChannelName)
		}
		return exit.Ok, err
	}
	defer conn.Close()
	session := identity.Session(scope)
	h := herdr.New(&session)
	cfg, err := config.LoadScope(scope)
	if err != nil {
		return 0, err
	}
	state, err := reserveMessage(conn, e.MsgID, e.Payload.Thread)
	if err != nil {
		return 0, err
	}
	if state != "reserved" {
		fmt.Fprintf(os.Stdout, "message %s was already %s\n", e.MsgID, state)
		return exit.Ok, nil
	}
	msg := inboundMessage{Thread: e.Payload.Thread, Text: e.Payload.Text, User: e.Payload.User, TS: e.Payload.TS,
		Context: e.Payload.Context, Mapping: mapping}
	// A person's message in the thread answers what was pending there.
	if res, err := conn.Exec("UPDATE questions SET state = 'answered', answered_at = ?1 WHERE thread = ?2 AND state = 'pending'",
		db.Now(), msg.Thread); err != nil {
		return 0, exit.Database(err)
	} else if n, _ := res.RowsAffected(); n > 0 {
		fmt.Fprintf(os.Stdout, "%d pending question(s) in thread %s answered\n", n, msg.Thread)
	}
	code, dropped, err := route(h, conn, scope, cfg, msg)
	if err != nil || code != exit.Ok {
		return code, err
	}
	state = "delivered"
	if dropped {
		state = "dropped"
	}
	return exit.Ok, setMessage(conn, e.MsgID, state)
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
// was started because the thread's checkout is not on this machine or
// Linear is unavailable, and the thread was told.
// Shared with `ask-human` and the job's conclusion, which reach the
// job's home thread the same way.
func route(h *herdr.Herdr, conn *sql.DB, scope string, cfg *config.Scope, msg inboundMessage) (code exit.Code, dropped bool, err error) {
	name, state, err := liveThreadAgent(conn, msg.Thread)
	if err != nil {
		return 0, false, err
	}
	if name != "" {
		present, err := herdrHas(h, name)
		if err != nil {
			return 0, false, err
		}
		if present && state == "starting" {
			code, err := resumeThreadStart(h, conn, scope, cfg, msg, name)
			return code, false, err
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
	if err := s.reserve(); err != nil {
		return 0, false, err
	}
	if err := s.linearSteps(); err != nil {
		var failure *exit.Failure
		if !errors.As(err, &failure) || failure.Code != exit.Environment {
			return 0, false, err
		}
		return exit.Ok, true, s.linearUnavailable(err)
	}
	code, err = s.start()
	return code, false, err
}

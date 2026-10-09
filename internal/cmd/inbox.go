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
		"new thread the channel's `context`, and an optional `target` naming the ledger and " +
		"herdr session (`default` when absent). Any other payload type is ignored, exit 0.\n\n" +
		"The target's settings come from $XDG_CONFIG_HOME/fleet/<target>.json (~/.config when " +
		"unset): `linear` ({\"team\": ...}; absent: thread tickets are off, threads still run) " +
		"and `fednet` ({\"socket\": ...}; absent: fleet cannot post to the thread). The herdr " +
		"session is --session, else the target's name.\n\n" +
		"First the msg_id is reserved in the target's ledger (table `inbox`): a message already " +
		"delivered or dropped is exit 0 at once, so a retry does nothing twice. Then the route: " +
		"a thread whose agent is live gets the message as `fleet send` would, headed `[FROM: " +
		"inbox]`. Otherwise a thread agent is started: `thread-<slug>` (the slug is the key, " +
		"lower-cased, [a-z0-9-]) in a tab of the herdr workspace `threads`, in ~/cross-repo/threads/ " +
		"for the default target or ~/dev/<target> otherwise, with FLEET_ROLE=thread, " +
		"FLEET_THREAD, FLEET_TARGET and FLEET_ISSUE. Its row is reserved first (one live agent " +
		"per thread), so a rerun after a kill cannot start a second one. With a Linear team, a " +
		"new thread gets a thread ticket (label `thread`, no project, description with the " +
		"channel, thread and first message) claimed for the agent; a known thread gets its " +
		"ticket claimed again, a `Session <n> started` comment, and its earlier session " +
		"summaries. If a Linear step fails, no agent is started, the message is dropped, one " +
		"line saying Linear is unavailable is posted to the thread (`fednet client post`) and " +
		"the exit is 0. The agent's first message is the built-in thread prompt, the channel " +
		"context, the summaries and the message. A live row whose agent is gone from herdr is " +
		"ended (the session ended abnormally) and a new agent started.\n\n" +
		"Exit: 0 when the message is delivered, ignored or dropped (fednet marks it delivered); " +
		"1 when the event file cannot be read or is not an event, or the target is not a name; " +
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
		Target  string `json:"target"`
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
func Inbox(h *herdr.Herdr, args InboxArgs) (exit.Code, error) {
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
	target, err := identity.CheckTarget(e.Payload.Target)
	if err != nil {
		return 0, err
	}
	if h.Session == nil {
		h = herdr.New(&target)
	}
	cfg, err := config.LoadTarget(target)
	if err != nil {
		return 0, err
	}
	conn, err := db.Open(target)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	state, err := reserveMessage(conn, e.MsgID, e.Payload.Thread)
	if err != nil {
		return 0, err
	}
	if state != "reserved" {
		fmt.Fprintf(os.Stdout, "message %s was already %s\n", e.MsgID, state)
		return exit.Ok, nil
	}
	msg := inboundMessage{Thread: e.Payload.Thread, Text: e.Payload.Text, User: e.Payload.User, TS: e.Payload.TS,
		Context: e.Payload.Context}
	code, dropped, err := route(h, conn, target, cfg, msg)
	if err != nil || code != exit.Ok {
		return code, err
	}
	state = "delivered"
	if dropped {
		state = "dropped"
	}
	return exit.Ok, setMessage(conn, e.MsgID, state)
}

// route delivers the message to the thread's live agent, or starts one.
// exit.Ok means the agent has the message, or (`dropped`) that no agent
// was started because Linear is unavailable and the thread was told.
func route(h *herdr.Herdr, conn *sql.DB, target string, cfg *config.Target, msg inboundMessage) (code exit.Code, dropped bool, err error) {
	name, _, err := liveThreadAgent(conn, msg.Thread)
	if err != nil {
		return 0, false, err
	}
	if name != "" {
		present, err := herdrHas(h, name)
		if err != nil {
			return 0, false, err
		}
		if present {
			text, err := WithHeader(inboxSender, msg.body())
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
	s, err := newThreadStart(h, conn, target, cfg, msg)
	if err != nil {
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
		return exit.Ok, true, s.linearUnavailable(err)
	}
	code, err = s.start()
	return code, false, err
}

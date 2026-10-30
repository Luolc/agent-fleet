// `fleet send <to>`: one message to another agent, with the header.

package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
	"github.com/Luolc/agent-fleet/internal/identity"
)

// SendAbout and SendLongAbout are the help texts of `send`.
const (
	SendAbout     = "Send one message to another agent, with the [FROM: <you>] header"
	SendLongAbout = "Send one message to another agent, with the [FROM: <you>] header.\n\n" +
		"The body is read from stdin, or from --file; it is never an argument, so there is " +
		"nothing to quote. The header line `[FROM: $FLEET_AGENT]` is prepended; a body that " +
		"already starts with `[FROM:` is refused so headers are never forged or doubled. The " +
		"message is delivered with `herdr agent prompt <to> <text> --wait --until working " +
		"--timeout 20000` and the result is mapped to the exit codes.\n\n" +
		"There is no automatic resend: exit 2 means the outcome is unknown, not that the " +
		"message was lost.\n\n" +
		"<to> is an agent's name, or a thread key `CHANNEL/TS` (as the THREAD column of `fleet status` " +
		"shows it): then the message goes to that thread's live thread agent, and exit 4 says so when " +
		"the thread has none, with the route to take.\n\n" +
		"A thread agent's message that reaches a lead answers the lead's questions pending in " +
		"the thread agent's thread (`fleet ask-thread`): the thread agent has settled them, by " +
		"itself or with the people; but not one a card of the thread agent's still asks the " +
		"people for (`fleet ask-human --for`), which that card's answer settles. A ledger that " +
		"cannot be updated is said on stderr; the exit code stays the delivery's.\n\n" +
		"herdr reports no blocked state for a Pi agent, so a Pi target's screen is read first: " +
		"unless it shows Pi's editor (idle, or working with the editor still open), nothing is " +
		"typed and the send ends with exit 3.\n\n" +
		"A Claude Code target whose message was stopped by a safety classifier again after Claude " +
		"Code retried it waits on Claude Code's `Session paused` dialog, whose first option, " +
		"`Switch to <fallback>`, moves the session to another model; herdr may read that dialog " +
		"as idle, so text typed there would answer it. Unless herdr finds the target working, " +
		"its transcript is read first, as `fleet watch` reads it (the records' shape, never the " +
		"conversation's text): a session stopped by a refusal with no reply since that ended its " +
		"turn, and not one waiting at its prompt after a stop with no fallback, gets nothing typed " +
		"and no key, and the send ends with exit 8, its screen not printed. Press no key on that " +
		"screen either: `fleet watch` replaces such a session with a new one (send again once it " +
		"has), or, for a worker, its lead may end it with `fleet worker end` and start another to " +
		"take over. A target fleet cannot tell about (no session or transcript found, a read that " +
		"fails or takes over 5 s) gets the message as before.\n\n" +
		"Exit: 0 when herdr reported agent_prompted; 1 for an empty body, a forged header or a " +
		"missing FLEET_AGENT; 2 when herdr gave no clear signal (timeout, stalled); 3 when " +
		"the target is blocked (its visible screen is printed so you can see what is in the " +
		"way); 4 when the target is not found; 5 when herdr itself fails; 8 when the target is " +
		"paused on Claude Code's `Session paused` dialog (nothing typed; press no key there)."
)

// SendArgs are the arguments of `send`.
type SendArgs struct {
	// To is the name of the receiving agent, as shown by `fleet status`, or a
	// thread key `CHANNEL/TS`, which stands for that thread's live thread agent.
	To string
	// File, when set, is read for the body instead of stdin.
	File *string
}

// WithHeader prepends the header. Fails (exit 1) on an empty body or a body
// that already carries a header.
func WithHeader(sender, body string) (string, error) {
	if strings.TrimSpace(body) == "" {
		return "", exit.Refusedf("the message body is empty")
	}
	if strings.HasPrefix(strings.TrimLeftFunc(body, unicode.IsSpace), "[FROM:") {
		return "", exit.Refusedf(
			"the body already starts with a [FROM: …] header; fleet adds the header itself")
	}
	return fmt.Sprintf("[FROM: %s]\n%s", sender, body), nil
}

// Deliver delivers `text` (already carrying its header) to `to` and maps
// the outcome to an exit code. Shared with `done`. A target of a kind
// herdr reports no blocked state for (Pi) is read first: a screen that is
// not its input box is exit 3 with the screen, nothing typed, as herdr's
// agent_blocked would be. A Claude Code target paused on its `Session
// paused` dialog is exit 8, nothing typed and its screen not printed.
func Deliver(h *herdr.Herdr, to, text string) (exit.Code, error) {
	if code, err := beforePrompt(h, to); err != nil || code != exit.Ok {
		return code, err
	}
	outcome, err := h.Prompt(to, text)
	if err != nil {
		return 0, err
	}
	switch outcome.Kind {
	case herdr.Prompted:
		fmt.Fprintf(os.Stdout, "delivered to %s\n", to)
	case herdr.Unknown:
		fmt.Fprintf(os.Stderr, "no clear signal from %s (%s); the outcome is unknown\n", to, outcome.Reason)
	case herdr.Blocked:
		fmt.Fprintf(os.Stderr, "%s is blocked by an interactive prompt; its screen:\n", to)
		screen, err := h.Screen(to)
		if err != nil {
			return 0, err
		}
		fmt.Fprint(os.Stdout, screen)
	case herdr.NotFound:
		fmt.Fprintf(os.Stderr, "agent %s not found\n", to)
	}
	return outcome.Exit(), nil
}

// beforePrompt is Deliver's check before the prompt: exit 3 with the
// screen printed when `to` is of a kind whose screen is read first and
// the screen is not its input box, and exit 8 when it is a Claude Code
// session paused on its dialog. An agent herdr does not know is left to
// the prompt.
func beforePrompt(h *herdr.Herdr, to string) (exit.Code, error) {
	agent := herdrAgent(h, to)
	if agent == nil {
		return exit.Ok, nil
	}
	if pausedOnDialog(h, to, agent) {
		fmt.Fprintf(os.Stderr, "%s is paused on Claude Code's `Session paused` dialog (a message of its turn was "+
			"stopped by a safety classifier again after Claude Code retried it), so nothing was typed. Press no key "+
			"on its screen: a key there can pick `Switch to <fallback>` and move the session to another model. "+
			"`fleet watch` replaces such a session with a new one; send again once it has. A worker's lead may "+
			"instead end it with `fleet worker end` and start another to take over.\n", to)
		return exit.Paused, nil
	}
	kind, _ := agent["agent"].(string)
	k := kindOf(kind)
	if !k.readFirst {
		return exit.Ok, nil
	}
	screen, err := h.Screen(to)
	if err != nil {
		return 0, err
	}
	if k.inputBox(screen) {
		return exit.Ok, nil
	}
	fmt.Fprintf(os.Stderr, "%s (%s) is not at its input box, so nothing was typed; its screen:\n", to, kind)
	fmt.Fprint(os.Stdout, screen)
	return exit.Blocked, nil
}

// herdrAgent is the `agent` of herdr's `agent get` reply for `name`; nil
// when herdr does not know it or gives no reply.
func herdrAgent(h *herdr.Herdr, name string) map[string]any {
	reply, err := h.Call("agent", "get", name)
	if err != nil || reply.Error != nil {
		return nil
	}
	agent, _ := herdr.Lookup(reply.Result, "agent").(map[string]any)
	return agent
}

// pauseReadBudget is how long a delivery reads a transcript to tell
// whether its session is paused; a read not done by then cannot tell.
const pauseReadBudget = 5 * time.Second

// pausedOnDialog is whether `agent` (herdr's `agent get` of `name`) is a
// Claude Code session paused on its `Session paused` dialog, as far as
// fleet can tell: herdr's kind is Claude Code's or none, herdr does not
// find it working, and its transcript, read as the watch `model-changes`
// reads it (the records' shape, never the conversation's text), has it
// stopped, and not at its prompt. A session fleet cannot tell about (no
// pane, session or transcript, a read that failed or did not end within
// pauseReadBudget) is not, the failed or unfinished read said on stderr.
func pausedOnDialog(h *herdr.Herdr, name string, agent map[string]any) bool {
	kind, _ := agent["agent"].(string)
	status, _ := agent["agent_status"].(string)
	pane, _ := agent["pane_id"].(string)
	if kind != "" && kind != KindClaude || status == "working" || pane == "" {
		return false
	}
	dir := claudeDir()
	session, err := sessionOf(h, dir, pane)
	path := ""
	if err == nil && session != "" {
		path, err = findTranscript(dir, session)
	}
	if err == nil && path == "" {
		return false
	}
	var st transcriptState
	complete := false
	if err == nil {
		start := time.Now()
		_, st, complete, err = scanTranscript(path, transcriptState{},
			func() bool { return time.Since(start) > pauseReadBudget })
	}
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "note: cannot tell whether %s is paused on Claude Code's dialog (%v); typing as before\n",
			name, err)
		return false
	case !complete:
		fmt.Fprintf(os.Stderr, "note: cannot tell whether %s is paused on Claude Code's dialog (its transcript was "+
			"not read to its end within %v); typing as before\n", name, pauseReadBudget)
		return false
	}
	return st.stopped && !st.atPrompt
}

// Send runs `send`.
func Send(h *herdr.Herdr, args SendArgs) (exit.Code, error) {
	me, err := identity.AgentName()
	if err != nil {
		return 0, err
	}
	var body string
	if args.File != nil {
		data, err := os.ReadFile(*args.File)
		if err != nil {
			return 0, exit.Refusedf("cannot read %s: %v", *args.File, err)
		}
		body = string(data)
	} else {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return 0, exit.IO(err)
		}
		body = string(data)
	}
	text, err := WithHeader(me, body)
	if err != nil {
		return 0, err
	}
	to, code, err := resolveTo(args.To)
	if err != nil || code != exit.Ok {
		return code, err
	}
	code, err = Deliver(h, to, text)
	if err == nil && code == exit.NotFound && to != args.To {
		fmt.Fprintf(os.Stderr, "the ledger has %s as thread %s's agent, but herdr has no such agent; nothing was sent. "+
			"Post to the thread in Slack with `fleet thread post --deliver --thread %s --body-file <file>`: "+
			"people see it, no agent is told\n", to, args.To, args.To)
	}
	if err == nil && code == exit.Ok && os.Getenv("FLEET_ROLE") == identity.Thread.String() {
		answerLead(os.Getenv("FLEET_THREAD"), to)
	}
	return code, err
}

// resolveTo is the agent `to` names: itself, or for a thread key
// `CHANNEL/TS` (an agent's name has no `/`) the live thread agent of that
// thread. A thread with none is exit 4 with what to do instead.
func resolveTo(to string) (string, exit.Code, error) {
	if !strings.Contains(to, "/") {
		return to, exit.Ok, nil
	}
	conn, err := OpenLedger()
	if err != nil {
		return "", 0, err
	}
	defer conn.Close()
	name, _, err := liveThreadAgent(conn, to)
	if err != nil {
		return "", 0, err
	}
	if name != "" {
		return name, exit.Ok, nil
	}
	row, err := threadByKey(conn, to)
	if err != nil {
		return "", 0, err
	}
	if row == nil {
		fmt.Fprintf(os.Stderr, "thread %s is not in this scope's ledger, so no agent here has it; "+
			"if another machine owns it, post there with `fleet thread post --deliver --thread %s --body-file <file>`\n", to, to)
	} else {
		fmt.Fprintf(os.Stderr, "thread %s has no live thread agent (none is running, whatever `fleet status` shows for other agents); "+
			"nothing was sent. Post to it in Slack with `fleet thread post --deliver --thread %s --body-file <file>`: "+
			"people see it, no agent is told\n", to, to)
	}
	return "", exit.NotFound, nil
}

// answerLead marks the questions `lead` asked in `thread` and that are
// still pending answered, but those a card of the thread agent's still
// asks the people for: that card's answer settles them. A failure is only
// said: the message is out, and a nonzero exit would have the thread
// agent send it again.
func answerLead(thread, lead string) {
	if thread == "" {
		return
	}
	err := func() error {
		conn, err := OpenLedger()
		if err != nil {
			return err
		}
		defer conn.Close()
		res, err := conn.Exec("UPDATE questions SET state = 'answered', answered_at = ?1 "+
			"WHERE thread = ?2 AND asked_by = ?3 AND state = 'pending' AND NOT EXISTS (SELECT 1 FROM questions c "+
			"WHERE c.for_question = questions.id AND c.state = 'pending')", db.Now(), thread, lead)
		if err != nil {
			return exit.Database(err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			fmt.Fprintf(os.Stdout, "%d pending question(s) of %s in thread %s answered\n", n, lead, thread)
		}
		return nil
	}()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet: the questions of %s in thread %s stay pending: %v\n", lead, thread, err)
	}
}

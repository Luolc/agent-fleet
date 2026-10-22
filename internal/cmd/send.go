// `fleet send <to>`: one message to another agent, with the header.

package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
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
		"Exit: 0 when herdr reported agent_prompted; 1 for an empty body, a forged header or a " +
		"missing FLEET_AGENT; 2 when herdr gave no clear signal (timeout, stalled); 3 when " +
		"the target is blocked (its visible screen is printed so you can see what is in the " +
		"way); 4 when the target is not found; 5 when herdr itself fails."
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
// the outcome to an exit code. Shared with `done`.
func Deliver(h *herdr.Herdr, to, text string) (exit.Code, error) {
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

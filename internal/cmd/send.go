// `fleet send <to>`: one message to another agent, with the header.

package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

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
		"Exit: 0 when herdr reported agent_prompted; 1 for an empty body, a forged header or a " +
		"missing FLEET_AGENT; 2 when herdr gave no clear signal (timeout, stalled); 3 when " +
		"the target is blocked (its visible screen is printed so you can see what is in the " +
		"way); 4 when the target is not found; 5 when herdr itself fails."
)

// SendArgs are the arguments of `send`.
type SendArgs struct {
	// To is the name of the receiving agent, as shown by `fleet status`.
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
	return Deliver(h, args.To, text)
}

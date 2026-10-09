// Package herdr runs `herdr` and reads its JSON replies.
//
// Every herdr CLI command prints one JSON object: `{"id":…,"result":{…}}`
// on success or `{"id":…,"error":{"code":…,"message":…}}` on failure
// (herdr 0.9.3). `agent read` is the exception and prints plain text.
package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/Luolc/agent-fleet/internal/exit"
)

// callTimeout bounds one herdr call; herdr's own longest wait is 30 s. A
// variable so the test can shorten it.
var callTimeout = 60 * time.Second

// waitDelay is how long Wait keeps the pipes open after herdr exits. A
// variable so the test can shorten it.
var waitDelay = 5 * time.Second

// Herdr is how to reach the herdr server. Inside a pane herdr finds its
// own session through `HERDR_SOCKET_PATH`; from cron or a test the session
// is named.
type Herdr struct {
	Session *string
}

// Reply is a parsed reply. `Error` is set (and `Result` nil) when herdr
// answered with an error; it carries herdr's error code (for example
// `agent_not_found`) and message.
type Reply struct {
	Result json.RawMessage
	Error  *ReplyError
}

// ReplyError is herdr's error object.
type ReplyError struct {
	Code    string
	Message string
}

// New makes a Herdr for the given session (nil: the pane's own).
func New(session *string) *Herdr {
	return &Herdr{Session: session}
}

// envPrefixes are the variables herdr may see, by exact name or prefix.
var envNames = []string{"PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "TERM", "LANG"}
var envPrefixes = []string{"LC_", "XDG_", "HERDR_"}

func env() []string {
	var kept []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		keep := false
		for _, n := range envNames {
			keep = keep || name == n
		}
		for _, p := range envPrefixes {
			keep = keep || strings.HasPrefix(name, p)
		}
		if keep {
			kept = append(kept, kv)
		}
	}
	return kept
}

// Run runs `herdr [--session S] args...` and returns stdout, stderr and
// the exit error, if any: a *exit.Failure when herdr could not be run or
// hit the deadline, an *exec.ExitError when it exited non-zero. For a
// command that prints plain text (`pane read`); the JSON ones go through
// Call.
func (h *Herdr) Run(args ...string) (stdout, stderr []byte, err error) {
	return h.run(args...)
}

// run runs `herdr [--session S] args...` and returns stdout, stderr and
// the exit error, if any.
func (h *Herdr) run(args ...string) (stdout, stderr []byte, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	argv := make([]string, 0, len(args)+2)
	if h.Session != nil {
		argv = append(argv, "--session", *h.Session)
	}
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, "herdr", argv...)
	cmd.Env = env()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = waitDelay
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err = cmd.Run()
	// Whatever herdr left behind in its process group (a descendant holding
	// the pipes past WaitDelay, or everything after the deadline) goes with
	// it; nothing herdr starts for a call is meant to outlive the call.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if ctx.Err() != nil {
		// Only the operation is named: args may carry a message body.
		return nil, nil, exit.Environmentf("herdr %s timed out after %v", strings.Join(args[:min(2, len(args))], " "), callTimeout)
	}
	// herdr itself has exited and what it printed is in hand; the
	// descendant that kept the pipes open was just killed.
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return nil, nil, exit.Environmentf("cannot run herdr: %v", err)
	}
	return out.Bytes(), errOut.Bytes(), err
}

// Call runs a herdr command and parses its JSON reply. A reply that is not
// JSON at all, or a herdr that cannot be started, is an environment error.
func (h *Herdr) Call(args ...string) (*Reply, error) {
	out, errOut, err := h.run(args...)
	var failure *exit.Failure
	if errors.As(err, &failure) {
		return nil, failure
	}
	stdout, stderr := string(out), string(errOut)
	text := ""
	for _, candidate := range []string{strings.TrimSpace(stdout), strings.TrimSpace(stderr)} {
		if strings.HasPrefix(candidate, "{") {
			text = candidate
			break
		}
	}
	if text == "" {
		// "exit exit status: N": the source prints the status's own Display
		// after the word exit.
		return nil, exit.Environmentf("herdr gave no JSON reply (exit %s): %s",
			status(err), firstLine(stderr, stdout))
	}
	// Exact keys: a map lookup, never a struct, which would match
	// case-insensitively.
	var value map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		return nil, exit.Environmentf("herdr reply is not JSON: %v", err)
	}
	if raw, ok := value["error"]; ok {
		field := func(name string) string {
			s, _ := Lookup(raw, name).(string)
			return s
		}
		return &Reply{Error: &ReplyError{Code: field("code"), Message: field("message")}}, nil
	}
	if raw, ok := value["result"]; ok {
		return &Reply{Result: raw}, nil
	}
	return nil, exit.Environmentf("herdr reply has neither result nor error")
}

// CallOK is Call, but a herdr error becomes an environment failure.
func (h *Herdr) CallOK(args ...string) (json.RawMessage, error) {
	reply, err := h.Call(args...)
	if err != nil {
		return nil, err
	}
	if reply.Error != nil {
		return nil, exit.Environmentf("herdr: %s: %s", reply.Error.Code, reply.Error.Message)
	}
	return reply.Result, nil
}

// Prompt is `herdr agent prompt <target> <text> --wait --until working
// --timeout 20000`, mapped to the exit-code contract.
func (h *Herdr) Prompt(target, text string) (PromptOutcome, error) {
	reply, err := h.Call("agent", "prompt", target, text,
		"--wait", "--until", "working", "--timeout", "20000")
	if err != nil {
		return PromptOutcome{}, err
	}
	if reply.Error == nil {
		if kind, _ := Lookup(reply.Result, "type").(string); kind == "agent_prompted" {
			return PromptOutcome{Kind: Prompted}, nil
		}
		return PromptOutcome{Kind: Unknown,
			Reason: fmt.Sprintf("unexpected herdr reply: %s", Display(reply.Result))}, nil
	}
	code, message := reply.Error.Code, reply.Error.Message
	switch code {
	case "agent_blocked":
		return PromptOutcome{Kind: Blocked}, nil
	case "agent_not_found":
		return PromptOutcome{Kind: NotFound}, nil
	case "timeout", "agent_prompt_stalled":
		return PromptOutcome{Kind: Unknown, Reason: code + ": " + message}, nil
	default:
		return PromptOutcome{}, exit.Environmentf("herdr: %s: %s", code, message)
	}
}

// Screen is the target's visible screen as plain text (`herdr agent read
// --source visible`). A failed read is an environment error, not an empty
// screen.
func (h *Herdr) Screen(target string) (string, error) {
	out, errOut, err := h.run("agent", "read", target, "--source", "visible")
	var failure *exit.Failure
	if errors.As(err, &failure) {
		return "", failure
	}
	if err != nil {
		return "", exit.Environmentf("herdr agent read %s failed (%s): %s",
			target, status(err), firstLine(string(errOut), string(out)))
	}
	return string(out), nil
}

// PromptKind is what `herdr agent prompt` reported, in the terms of the
// exit-code table.
type PromptKind int

// The outcomes.
const (
	// Prompted is `agent_prompted`: exit 0.
	Prompted PromptKind = iota
	// Unknown is timeout or stalled: exit 2. Reason is herdr's reason.
	Unknown
	// Blocked is `agent_blocked`: exit 3.
	Blocked
	// NotFound is `agent_not_found`: exit 4.
	NotFound
)

// PromptOutcome is the outcome and, for Unknown, herdr's reason.
type PromptOutcome struct {
	Kind   PromptKind
	Reason string
}

// Exit maps the outcome to its exit code.
func (o PromptOutcome) Exit() exit.Code {
	switch o.Kind {
	case Prompted:
		return exit.Ok
	case Unknown:
		return exit.Unknown
	case Blocked:
		return exit.Blocked
	default:
		return exit.NotFound
	}
}

// status renders the process status as Rust's ExitStatus Display does.
func status(err error) string {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return fmt.Sprintf("signal: %d", ws.Signal())
		}
		return fmt.Sprintf("exit status: %d", exitErr.ExitCode())
	}
	return "exit status: 0"
}

// firstLine is the first non-empty line of stderr, else of stdout, else "".
// A line ends at "\n" or "\r\n", as Rust's `lines()` splits.
func firstLine(texts ...string) string {
	for _, text := range texts {
		line, _, _ := strings.Cut(text, "\n")
		line = strings.TrimSuffix(line, "\r")
		if line != "" {
			return line
		}
	}
	return ""
}

// Lookup is `value.get(key)` on a JSON object: the value under the exact
// key, decoded with numbers kept as json.Number, or nil when `raw` is not
// an object or has no such key.
func Lookup(raw json.RawMessage, key string) any {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return nil
	}
	field, ok := object[key]
	if !ok {
		return nil
	}
	return Decode(field)
}

// Decode is one JSON value as Go data with numbers kept as json.Number;
// nil when it does not parse.
func Decode(raw json.RawMessage) any {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil
	}
	return value
}

// Display renders a reply as serde_json's Display does: compact, object
// keys sorted, `<`, `>` and `&` unescaped.
func Display(raw json.RawMessage) string {
	value := Decode(raw)
	if value == nil {
		return string(raw)
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(value) != nil {
		return string(raw)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

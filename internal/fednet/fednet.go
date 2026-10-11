// Package fednet runs `fednet client`: `post` for what fleet itself says
// in a thread and what a thread agent posts through `fleet thread post`,
// `delete` for the reminder a newer one replaces, `progress` for a thread
// agent's progress card, and `read-thread` for when a thread last had a
// message.
package fednet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
)

// postArgv is `client post -socket <socket> -thread <thread> [-file
// <f>]... [-mention <user>]... -- <text>`.
func postArgv(socket, thread, text string, files, mentions []string) []string {
	args := []string{"client", "post", "-socket", socket, "-thread", thread}
	for _, f := range files {
		args = append(args, "-file", f)
	}
	for _, m := range mentions {
		args = append(args, "-mention", m)
	}
	return append(args, "--", text)
}

// progressArgv is `client progress -socket <socket> -thread <thread>
// [-done] [-title <title>] [-item <item>]...`: the card is replaced whole
// each time, so the title and items are all of it; with done they are the
// closed card's wording, when given.
func progressArgv(socket, thread, title string, items []string, done bool) []string {
	args := []string{"client", "progress", "-socket", socket, "-thread", thread}
	if done {
		args = append(args, "-done")
	}
	if title != "" {
		args = append(args, "-title", title)
	}
	for _, item := range items {
		args = append(args, "-item", item)
	}
	return args
}

// Post is `fednet client post -socket <socket> -thread <thread> -- <text>`,
// through herdr's runner (deadline, process group, environment
// allow-list). A non-zero exit is an environment failure that names the
// step and fednet's status, never its output.
func Post(socket, thread, text string) error {
	return hidden("fednet client post", postArgv(socket, thread, text, nil, nil))
}

// Footer is `fednet client post -socket <socket> -thread <thread> -footer
// -- <text>`: one line of small grey text (a context block) in the
// thread, its `[text](url)` links kept, what fleet says when a session
// ends. As Post, fednet's output is hidden.
func Footer(socket, thread, text string) error {
	return hidden("fednet client post", []string{"client", "post", "-socket", socket, "-thread", thread, "-footer", "--", text})
}

// ErrMention marks a post fednet refused for its mentions, queuing
// nothing: exit 2 from a fednet too old for `-mention` (an older hub says
// so, an older client does not know the flag), or 3 for a user not on the
// hub's user list.
var ErrMention = errors.New("fednet refused the mention")

// PostID is Post that returns the msg_id fednet prints once the client
// has queued the post, with `mentions` (Slack user ids) mentioned at its
// start. A refusal of the mentions wraps ErrMention, with fednet's first
// line of stderr.
func PostID(socket, thread, text string, mentions ...string) (string, error) {
	stdout, stderr, code, err := relay("fednet client post", postArgv(socket, thread, text, nil, mentions))
	if err == nil && len(mentions) > 0 && (code == 2 || code == 3) {
		return "", fmt.Errorf("%w (exit status: %d): %s", ErrMention, code, firstLine(stderr))
	}
	if err == nil && code != 0 {
		err = exit.Environmentf("fednet client post failed (exit status: %d); its output is not shown, run it yourself to see why", code)
	}
	if err != nil {
		return "", err
	}
	return string(bytes.TrimSpace(stdout)), nil
}

// Delete is `fednet client delete -socket <socket> <msg-id>`, which
// deletes from Slack a post this machine made: fednet's exit code (0 when
// deleted) and its first line of stderr. A fednet that could not be run,
// hit the deadline or was killed by a signal is the error.
func Delete(socket, msgID string) (code int, reason string, err error) {
	_, stderr, code, err := relay("fednet client delete", []string{"client", "delete", "-socket", socket, "--", msgID})
	return code, firstLine(stderr), err
}

// firstLine is the first line of fednet's output, trimmed.
func firstLine(out []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line
}

// ErrThread marks a failure of one thread's read: fednet answered, but
// not with that thread (its exit 1 or 2, a thread it does not have, say),
// unlike fednet unreachable or busy (exit 4 or 5).
var ErrThread = errors.New("fednet could not read this thread")

// Latest is the newest message of a thread (`fednet client read-thread
// -json`, which the hub reads from Slack): its Slack timestamp and the
// same in whole seconds. A thread with no message, or a reply that is
// not fednet's, is an environment failure; one fednet answered with exit
// 1 or 2 also wraps ErrThread.
func Latest(socket, thread string) (ts string, secs int64, err error) {
	const op = "fednet client read-thread"
	stdout, stderr, code, err := relay(op, []string{"client", "read-thread", "-socket", socket, "-json", thread})
	switch {
	case err != nil:
		return "", 0, err
	case code == 1 || code == 2:
		return "", 0, fmt.Errorf("%s %s failed (exit status: %d): %s: %w", op, thread, code,
			strings.TrimSpace(string(stderr)), ErrThread)
	case code != 0:
		return "", 0, exit.Environmentf("%s %s failed (exit status: %d)", op, thread, code)
	}
	var reply struct {
		Messages []struct {
			TS string `json:"ts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(stdout, &reply); err != nil {
		return "", 0, exit.Environmentf("%s %s: not a JSON reply: %v", op, thread, err)
	}
	newest := -1.0
	for _, m := range reply.Messages {
		at, err := strconv.ParseFloat(m.TS, 64)
		if err != nil {
			return "", 0, exit.Environmentf("%s %s: message timestamp %q is not a number", op, thread, m.TS)
		}
		if at > newest {
			ts, newest = m.TS, at
		}
	}
	if ts == "" {
		return "", 0, exit.Environmentf("%s %s: the thread has no message", op, thread)
	}
	return ts, int64(newest), nil
}

func hidden(op string, argv []string) error {
	_, _, err := herdr.Exec(op, nil, "fednet", argv...)
	var failure *exit.Failure
	if errors.As(err, &failure) {
		return failure
	}
	if err != nil {
		return exit.Environmentf("%s failed (%s); its output is not shown, run it yourself to see why",
			op, herdr.Status(err))
	}
	return nil
}

// Relay is Post for an agent's own post, with files to upload, and
// fednet's output handed back as it is: its stdout and stderr, and its
// exit code when it exited non-zero (0 otherwise). A fednet that could not
// be run, hit the deadline or was killed by a signal is a *exit.Failure.
func Relay(socket, thread, text string, files []string) (stdout, stderr []byte, code int, err error) {
	return relay("fednet client post", postArgv(socket, thread, text, files, nil))
}

// Progress is Relay for an agent's progress card: `fednet client progress`
// with the whole card (title and items, each `<text>:<doing|done|error>`),
// or `-done` to complete it, with the closed card's wording when given.
func Progress(socket, thread, title string, items []string, done bool) (stdout, stderr []byte, code int, err error) {
	return relay("fednet client progress", progressArgv(socket, thread, title, items, done))
}

func relay(op string, argv []string) (stdout, stderr []byte, code int, err error) {
	stdout, stderr, err = herdr.Exec(op, nil, "fednet", argv...)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 {
		return stdout, stderr, exitErr.ExitCode(), nil
	}
	if err != nil && !errors.As(err, new(*exit.Failure)) {
		err = exit.Environmentf("%s failed (%s)", op, herdr.Status(err))
	}
	return stdout, stderr, 0, err
}

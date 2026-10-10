// Package fednet runs `fednet client post`: what fleet itself says in a
// thread, and what a thread agent posts through `fleet thread post`.
package fednet

import (
	"errors"
	"os/exec"

	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
)

// argv is `client post -socket <socket> -thread <thread> [-file <f>]... -- <text>`.
func argv(socket, thread, text string, files []string) []string {
	args := []string{"client", "post", "-socket", socket, "-thread", thread}
	for _, f := range files {
		args = append(args, "-file", f)
	}
	return append(args, "--", text)
}

// Post is `fednet client post -socket <socket> -thread <thread> -- <text>`,
// through herdr's runner (deadline, process group, environment
// allow-list). A non-zero exit is an environment failure that names the
// step and fednet's status, never its output.
func Post(socket, thread, text string) error {
	op := "fednet client post"
	_, _, err := herdr.Exec(op, nil, "fednet", argv(socket, thread, text, nil)...)
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
	op := "fednet client post"
	stdout, stderr, err = herdr.Exec(op, nil, "fednet", argv(socket, thread, text, files)...)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 {
		return stdout, stderr, exitErr.ExitCode(), nil
	}
	if err != nil && !errors.As(err, new(*exit.Failure)) {
		err = exit.Environmentf("%s failed (%s)", op, herdr.Status(err))
	}
	return stdout, stderr, 0, err
}

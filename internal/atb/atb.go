// Package atb runs `atb linear` to write a worker report and release the
// worker's issue.
package atb

import (
	"errors"

	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
)

// envNames are what atb needs on top of herdr's allow-list: the key or the
// command that prints it, the endpoint, and the home its key cache is
// under. Only their names pass through here; the key stays in the
// environment.
var envNames = []string{"LINEAR_API_KEY", "LINEAR_API_KEY_CMD", "LINEAR_API_URL", "ATB_HOME"}

// Comment is `atb linear comment <issue> --body-file <file>`.
func Comment(issue, file string) error {
	return run("comment", issue, "--body-file", file)
}

// Release is `atb linear release <issue> --agent <agent> --reason done
// --done`, or `--reason abandoned --abandon` when abandoned.
func Release(issue, agent string, abandon bool) error {
	if abandon {
		return run("release", issue, "--agent", agent, "--reason", "abandoned", "--abandon")
	}
	return run("release", issue, "--agent", agent, "--reason", "done", "--done")
}

// run runs `atb linear <sub> <issue> args...`. Any non-zero exit is an
// environment failure naming the step and atb's status. atb's output is
// dropped: atb holds the key, so what it prints may carry it.
func run(sub, issue string, args ...string) error {
	op := "atb linear " + sub + " " + issue
	argv := append([]string{"linear", sub, issue}, args...)
	_, _, err := herdr.Exec(op, envNames, "atb", argv...)
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

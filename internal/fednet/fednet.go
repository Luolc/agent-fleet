// Package fednet runs `fednet client`: what fleet itself says in a thread.
// Agents call fednet themselves for everything else.
package fednet

import (
	"errors"

	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
)

// Post is `fednet client post -socket <socket> -thread <thread> -- <text>`,
// through herdr's runner (deadline, process group, environment
// allow-list). A non-zero exit is an environment failure that names the
// step and fednet's status, never its output.
func Post(socket, thread, text string) error {
	op := "fednet client post"
	_, _, err := herdr.Exec(op, nil, "fednet", "client", "post", "-socket", socket, "-thread", thread, "--", text)
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

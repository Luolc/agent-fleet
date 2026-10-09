// Package atb runs `atb linear`: create and claim an agent's work order,
// write its worker report and release it.
package atb

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
)

// envNames are what atb needs on top of herdr's allow-list: the key or the
// command that prints it, the endpoint, and the home its key cache is
// under. Only their names pass through here; the key stays in the
// environment.
var envNames = []string{"LINEAR_API_KEY", "LINEAR_API_KEY_CMD", "LINEAR_API_URL", "ATB_HOME"}

// Issue is a Linear issue as `atb linear create --json` prints it.
type Issue struct {
	Identifier string `json:"identifier"`
	URL        string `json:"url"`
}

var identifierPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]*-[0-9]+$`)

// Create is `atb linear create --team <team> --project <project> --parent
// <parent> --title <title> --description-file <file> --json`. Only an
// identifier and an https URL are taken from its output; anything else
// is a failure that does not show the output.
func Create(team, project, parent, title, file string) (Issue, error) {
	op := "atb linear create"
	out, err := run(op, "linear", "create", "--team", team, "--project", project, "--parent", parent,
		"--title", title, "--description-file", file, "--json")
	if err != nil {
		return Issue{}, err
	}
	var issue Issue
	if json.Unmarshal(out, &issue) != nil || !identifierPattern.MatchString(issue.Identifier) ||
		!strings.HasPrefix(issue.URL, "https://") {
		return Issue{}, exit.Environmentf("%s printed no identifier and URL; its output is not shown", op)
	}
	return issue, nil
}

// Claim is `atb linear claim <issue> --agent <agent> --source <source>
// --scope <scope>`.
func Claim(issue, agent, source, scope string) error {
	_, err := run("atb linear claim "+issue, "linear", "claim", issue, "--agent", agent,
		"--source", source, "--scope", scope)
	return err
}

// Comment is `atb linear comment <issue> --body-file <file>`.
func Comment(issue, file string) error {
	_, err := run("atb linear comment "+issue, "linear", "comment", issue, "--body-file", file)
	return err
}

// Release is `atb linear release <issue> --agent <agent> --reason done
// --done`, or `--abandon` in place of `--done`.
func Release(issue, agent string, abandon bool) error {
	outcome := "--done"
	if abandon {
		outcome = "--abandon"
	}
	_, err := run("atb linear release "+issue, "linear", "release", issue, "--agent", agent,
		"--reason", "done", outcome)
	return err
}

// run runs atb with `argv`; `op` names the step in errors. Any non-zero
// exit is an environment failure naming the step and atb's status. Its
// stdout is returned for the caller to pick from and its stderr dropped,
// and neither ever goes into an error: atb holds the key, so what it
// prints may carry it.
func run(op string, argv ...string) ([]byte, error) {
	out, _, err := herdr.Exec(op, envNames, "atb", argv...)
	var failure *exit.Failure
	if errors.As(err, &failure) {
		return nil, failure
	}
	if err != nil {
		return nil, exit.Environmentf("%s failed (%s); its output is not shown, run it yourself to see why",
			op, herdr.Status(err))
	}
	return out, nil
}

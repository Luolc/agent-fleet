// Package atb runs `atb linear`: create and claim an issue, read a parent
// issue's team and project, write a worker report and release an issue.
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

// CheckIdentifier refuses what is not a Linear identifier such as ABC-12.
func CheckIdentifier(issue string) error {
	if !identifierPattern.MatchString(issue) {
		return exit.Refusedf("%q is not a Linear issue identifier such as ABC-12", issue)
	}
	return nil
}

// Create is `atb linear create --team <team> --project <project> [--parent
// <parent>] --title <title> --description-file <file> --json`; an empty
// parent makes a top-level issue. Only an identifier and an https URL are
// taken from its output; anything else is a failure that does not show
// the output.
func Create(team, project, parent, title, file string) (Issue, error) {
	op := "atb linear create"
	argv := []string{"linear", "create", "--team", team, "--project", project}
	if parent != "" {
		argv = append(argv, "--parent", parent)
	}
	argv = append(argv, "--title", title, "--description-file", file, "--json")
	out, err := run(op, argv...)
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

// TeamProject reads `issue`'s team key and project name with `atb linear
// query`, for the work orders of a job whose parent issue decides where
// they go. An issue without a project is refused (exit 1); a query that
// fails or prints no team is exit 5.
func TeamProject(issue string) (team, project string, err error) {
	if err := CheckIdentifier(issue); err != nil {
		return "", "", err
	}
	op := "atb linear query " + issue
	out, err := run(op, "linear", "query", `{ issue(id: "`+issue+`") { team { key } project { name } } }`)
	if err != nil {
		return "", "", err
	}
	// The reply is the query's data, with or without a `data` wrapper.
	var reply struct {
		Data  *issueData `json:"data"`
		Issue *issueNode `json:"issue"`
	}
	if json.Unmarshal(out, &reply) != nil {
		return "", "", exit.Environmentf("%s printed no JSON; its output is not shown", op)
	}
	node := reply.Issue
	if node == nil && reply.Data != nil {
		node = reply.Data.Issue
	}
	if node == nil || node.Team == nil || node.Team.Key == "" {
		return "", "", exit.Environmentf("%s printed no team for %s; its output is not shown", op, issue)
	}
	if node.Project == nil || node.Project.Name == "" {
		return "", "", exit.Refusedf("%s has no project in Linear; a job's work orders go to its parent's project", issue)
	}
	return node.Team.Key, node.Project.Name, nil
}

type issueData struct {
	Issue *issueNode `json:"issue"`
}

type issueNode struct {
	Team    *struct{ Key string }  `json:"team"`
	Project *struct{ Name string } `json:"project"`
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
// --done`, or `--reason abandoned --abandon` when abandoned.
func Release(issue, agent string, abandon bool) error {
	if abandon {
		_, err := run("atb linear release "+issue, "linear", "release", issue, "--agent", agent,
			"--reason", "abandoned", "--abandon")
		return err
	}
	_, err := run("atb linear release "+issue, "linear", "release", issue, "--agent", agent, "--reason", "done", "--done")
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

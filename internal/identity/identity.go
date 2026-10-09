// Package identity is the caller's identity, read from the `FLEET_*`
// variables that `job start` and `spawn` inject into a pane
// (docs/design.md).
package identity

import (
	"os"

	"github.com/Luolc/agent-fleet/internal/exit"
)

// Role is a role an agent can hold. The string forms are the `FLEET_ROLE`
// values.
type Role int

// The roles: a thread agent (one conversation on a Slack thread; it starts
// jobs), a lead (one per job) and a worker (started by a lead).
const (
	Thread Role = iota
	Lead
	Worker
)

// ParseRole reads a `FLEET_ROLE` value; ok is false for an unknown one.
func ParseRole(value string) (role Role, ok bool) {
	switch value {
	case "thread":
		return Thread, true
	case "lead":
		return Lead, true
	case "worker":
		return Worker, true
	default:
		return 0, false
	}
}

func (r Role) String() string {
	switch r {
	case Thread:
		return "thread"
	case Lead:
		return "lead"
	default:
		return "worker"
	}
}

// DefaultTarget is the target when `FLEET_TARGET` is unset or empty: the
// one ledger of a development machine.
const DefaultTarget = "default"

// Identity is who is calling. `Parent` and `Job` are empty for a thread
// agent; `Issue`, the agent's Linear work order, is empty when it has none;
// `Target` names the ledger and is never empty.
type Identity struct {
	Agent  string
	Role   Role
	Parent string
	Target string
	Job    string
	Issue  string
}

const hint = "this pane was not started by fleet; agents get these variables from `fleet job start` or `fleet spawn`"

// AgentName is the caller's name alone: all that `send` needs. Fails with
// exit code 1 when `FLEET_AGENT` is unset.
func AgentName() (string, error) {
	agent := os.Getenv("FLEET_AGENT")
	if agent == "" {
		return "", exit.Refusedf("FLEET_AGENT is not set: %s", hint)
	}
	return agent, nil
}

// Target is the caller's target from `FLEET_TARGET`, or DefaultTarget when
// that is unset or empty. Refused when it cannot name a directory.
func Target() (string, error) {
	return CheckTarget(os.Getenv("FLEET_TARGET"))
}

// CheckTarget is Target for a value given on the command line or in the
// environment: empty means the default; `/`, `.` and `..` are refused.
func CheckTarget(value string) (string, error) {
	if value == "" {
		return DefaultTarget, nil
	}
	for _, c := range value {
		if c == '/' || c == 0 {
			return "", exit.Refusedf("target %q must be a directory name", value)
		}
	}
	if value == "." || value == ".." {
		return "", exit.Refusedf("target %q must be a directory name", value)
	}
	return value, nil
}

// FromEnv reads the full identity from the environment. Fails with exit
// code 1 when `FLEET_AGENT` is unset, `FLEET_ROLE` is not a known role or
// `FLEET_TARGET` is not a directory name.
func FromEnv() (*Identity, error) {
	agent, err := AgentName()
	if err != nil {
		return nil, err
	}
	roleValue := os.Getenv("FLEET_ROLE")
	role, ok := ParseRole(roleValue)
	if !ok {
		return nil, exit.Refusedf("FLEET_ROLE is %q, expected thread, lead or worker: %s", roleValue, hint)
	}
	target, err := Target()
	if err != nil {
		return nil, err
	}
	return &Identity{
		Agent:  agent,
		Role:   role,
		Parent: os.Getenv("FLEET_PARENT"),
		Target: target,
		Job:    os.Getenv("FLEET_JOB"),
		Issue:  os.Getenv("FLEET_ISSUE"),
	}, nil
}

// EnvPairs are the variables to inject into a pane for an agent with this
// identity.
func (id *Identity) EnvPairs() [6][2]string {
	return [6][2]string{
		{"FLEET_AGENT", id.Agent},
		{"FLEET_ROLE", id.Role.String()},
		{"FLEET_PARENT", id.Parent},
		{"FLEET_TARGET", id.Target},
		{"FLEET_JOB", id.Job},
		{"FLEET_ISSUE", id.Issue},
	}
}

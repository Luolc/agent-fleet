// Package identity is the caller's identity, read from the `FLEET_*`
// variables that `spawn` injects into a pane (docs/design.md).
package identity

import (
	"os"

	"github.com/Luolc/agent-fleet/internal/exit"
)

// Role is a role an agent can hold. The string forms are the `FLEET_ROLE`
// values.
type Role int

// The roles.
const (
	Orchestra Role = iota
	HumanInterface
	Lead
	Worker
)

// ParseRole reads a `FLEET_ROLE` value; ok is false for an unknown one.
func ParseRole(value string) (role Role, ok bool) {
	switch value {
	case "orchestra":
		return Orchestra, true
	case "human-interface":
		return HumanInterface, true
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
	case Orchestra:
		return "orchestra"
	case HumanInterface:
		return "human-interface"
	case Lead:
		return "lead"
	default:
		return "worker"
	}
}

// Identity is who is calling. `Parent` and `Job` are empty for `orchestra`
// and `human-interface`.
type Identity struct {
	Agent  string
	Role   Role
	Parent string
	Repo   string
	Job    string
}

const hint = "this pane was not started by fleet; agents get these variables from `fleet spawn`"

// AgentName is the caller's name alone: all that `send` needs. Fails with
// exit code 1 when `FLEET_AGENT` is unset.
func AgentName() (string, error) {
	agent := os.Getenv("FLEET_AGENT")
	if agent == "" {
		return "", exit.Refusedf("FLEET_AGENT is not set: %s", hint)
	}
	return agent, nil
}

// FromEnv reads the full identity from the environment. Fails with exit
// code 1 when `FLEET_AGENT` is unset or `FLEET_ROLE` is not a known role.
func FromEnv() (*Identity, error) {
	agent, err := AgentName()
	if err != nil {
		return nil, err
	}
	roleValue := os.Getenv("FLEET_ROLE")
	role, ok := ParseRole(roleValue)
	if !ok {
		return nil, exit.Refusedf(
			"FLEET_ROLE is %q, expected orchestra, human-interface, lead or worker: %s",
			roleValue, hint)
	}
	return &Identity{
		Agent:  agent,
		Role:   role,
		Parent: os.Getenv("FLEET_PARENT"),
		Repo:   os.Getenv("FLEET_REPO"),
		Job:    os.Getenv("FLEET_JOB"),
	}, nil
}

// EnvPairs are the variables to inject into a pane for an agent with this
// identity.
func (id *Identity) EnvPairs() [5][2]string {
	return [5][2]string{
		{"FLEET_AGENT", id.Agent},
		{"FLEET_ROLE", id.Role.String()},
		{"FLEET_PARENT", id.Parent},
		{"FLEET_REPO", id.Repo},
		{"FLEET_JOB", id.Job},
	}
}

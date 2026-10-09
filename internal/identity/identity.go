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

// DefaultScope is the scope when `FLEET_SCOPE` is unset or empty.
const DefaultScope = "main"

// Identity is who is calling. `Parent` and `Job` are empty for a thread
// agent; `Issue`, the agent's Linear work order (a thread agent's thread
// ticket), is empty when it has none; `Scope` names the fleet (ledger,
// config and herdr session) and is never empty; `Thread` is the thread key
// of a thread agent, empty for the other roles.
type Identity struct {
	Agent  string
	Role   Role
	Parent string
	Scope  string
	Job    string
	Issue  string
	Thread string
}

const hint = "this pane was not started by fleet; agents get these variables from `fleet inbox`, `fleet job start` or `fleet spawn`"

// AgentName is the caller's name alone: all that `send` needs. Fails with
// exit code 1 when `FLEET_AGENT` is unset.
func AgentName() (string, error) {
	agent := os.Getenv("FLEET_AGENT")
	if agent == "" {
		return "", exit.Refusedf("FLEET_AGENT is not set: %s", hint)
	}
	return agent, nil
}

// Scope is the caller's scope from `FLEET_SCOPE`, or DefaultScope when
// that is unset or empty.
func Scope() (string, error) {
	return CheckScope(os.Getenv("FLEET_SCOPE"))
}

// CheckScope is Scope for a value given in a message or the environment:
// empty means the default; otherwise only [a-z0-9-], starting with a
// letter or digit, since the name is part of a file name and of the herdr
// session's.
func CheckScope(value string) (string, error) {
	if value == "" {
		return DefaultScope, nil
	}
	for i, c := range value {
		letterOrDigit := c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
		if !letterOrDigit && (c != '-' || i == 0) {
			return "", exit.Refusedf("scope %q: only [a-z0-9-], not starting with `-`", value)
		}
	}
	return value, nil
}

// Session is the herdr session of `scope`.
func Session(scope string) string {
	return "fleet-" + scope
}

// FromEnv reads the full identity from the environment. Fails with exit
// code 1 when `FLEET_AGENT` is unset, `FLEET_ROLE` is not a known role or
// `FLEET_SCOPE` is not a scope name.
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
	scope, err := Scope()
	if err != nil {
		return nil, err
	}
	return &Identity{
		Agent:  agent,
		Role:   role,
		Parent: os.Getenv("FLEET_PARENT"),
		Scope:  scope,
		Job:    os.Getenv("FLEET_JOB"),
		Issue:  os.Getenv("FLEET_ISSUE"),
		Thread: os.Getenv("FLEET_THREAD"),
	}, nil
}

// EnvPairs are the variables to inject into a pane for an agent with this
// identity. `FLEET_THREAD` is set only in a thread agent's pane: the other
// roles have no thread of their own (a job's home thread is in the
// ledger).
func (id *Identity) EnvPairs() [][2]string {
	pairs := [][2]string{
		{"FLEET_AGENT", id.Agent},
		{"FLEET_ROLE", id.Role.String()},
		{"FLEET_PARENT", id.Parent},
		{"FLEET_SCOPE", id.Scope},
		{"FLEET_JOB", id.Job},
		{"FLEET_ISSUE", id.Issue},
	}
	if id.Role == Thread {
		pairs = append(pairs, [2]string{"FLEET_THREAD", id.Thread})
	}
	return pairs
}

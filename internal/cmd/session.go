// The scope's herdr session and the `threads` workspace in it, made ready
// by `fleet inbox` before a thread agent starts.

package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
	"github.com/Luolc/agent-fleet/internal/identity"
)

// shellTab is the tab of the `threads` workspace that runs no agent; it
// keeps the workspace open when no thread agent is live.
const shellTab = "shell"

// sessionWait and sessionPoll are how long and how often `inbox` checks
// that a session it started answers.
const (
	sessionWait = 20 * time.Second
	sessionPoll = 200 * time.Millisecond
)

// sessionRunning is whether herdr's server for h's session runs (`herdr
// status server --json`).
func sessionRunning(h *herdr.Herdr) (bool, error) {
	out, _, err := h.Run("status", "server", "--json")
	var failure *exit.Failure
	if errors.As(err, &failure) {
		return false, failure
	}
	var status struct {
		Running bool `json:"running"`
	}
	if err != nil || json.Unmarshal(out, &status) != nil {
		return false, exit.Environmentf("herdr status server gave no status (exit %s)", herdr.Status(err))
	}
	return status.Running, nil
}

// ensureSession starts the scope's herdr session when it is not running:
// `systemctl --user start fleet-scope@<scope>`, then waits until the
// server answers. The server runs under systemd because the fednet client
// kills the hook's whole process group when the hook exits.
func ensureSession(h *herdr.Herdr, scope string) error {
	running, err := sessionRunning(h)
	if err != nil || running {
		return err
	}
	unit := "fleet-scope@" + scope + ".service"
	if os.Getenv("XDG_RUNTIME_DIR") == "" {
		return exit.Environmentf("the herdr session %s is not running, and XDG_RUNTIME_DIR is not set, so "+
			"`systemctl --user start %s` cannot reach the user's systemd: set Environment=XDG_RUNTIME_DIR=/run/user/<uid> "+
			"in the fednet client's unit and pass it with -hook-env XDG_RUNTIME_DIR", identity.Session(scope), unit)
	}
	op := "systemctl --user start " + unit
	_, _, err = herdr.Exec(op, nil, "systemctl", "--user", "start", unit)
	var failure *exit.Failure
	if errors.As(err, &failure) {
		return failure
	}
	if err != nil {
		return exit.Environmentf("%s failed (%s); see `systemctl --user status %s`", op, herdr.Status(err), unit)
	}
	for deadline := time.Now().Add(sessionWait); ; time.Sleep(sessionPoll) {
		if running, err = sessionRunning(h); err != nil || running {
			if running {
				fmt.Fprintf(os.Stdout, "started the herdr session of scope %s (%s)\n", scope, unit)
			}
			return err
		}
		if time.Now().After(deadline) {
			return exit.Environmentf("%s: the session is not running after %v", op, sessionWait)
		}
	}
}

// threadsWorkspaceID is the `threads` workspace with its `shell` tab,
// either made or restored when missing (the pane named `<scope>-shell`,
// in `home`). Run before each thread agent's start, so a workspace or tab
// closed by hand comes back.
func threadsWorkspaceID(h *herdr.Herdr, scope, home string) (string, error) {
	workspaces, err := WorkspacesLabelled(h, threadsWorkspace)
	if err != nil {
		return "", err
	}
	if len(workspaces) == 0 {
		result, err := h.CallOK("workspace", "create", "--label", threadsWorkspace, "--no-focus", "--cwd", home)
		if err != nil {
			return "", err
		}
		place, err := placeFrom(result)
		if err != nil {
			return "", err
		}
		if _, err := h.CallOK("tab", "rename", place.TabID, shellTab); err != nil {
			return "", err
		}
		_, err = h.CallOK("pane", "rename", place.PaneID, scope+"-shell")
		return place.WorkspaceID, err
	}
	id := workspaces[0]
	tabs, err := h.CallOK("tab", "list", "--workspace", id)
	if err != nil {
		return "", err
	}
	list, _ := herdr.Lookup(tabs, "tabs").([]any)
	for _, entry := range list {
		if tab, ok := entry.(map[string]any); ok && tab["label"] == shellTab {
			return id, nil
		}
	}
	result, err := h.CallOK("tab", "create", "--workspace", id, "--label", shellTab, "--no-focus", "--cwd", home)
	if err != nil {
		return "", err
	}
	place, err := placeFrom(result)
	if err != nil {
		return "", err
	}
	_, err = h.CallOK("pane", "rename", place.PaneID, scope+"-shell")
	return id, err
}

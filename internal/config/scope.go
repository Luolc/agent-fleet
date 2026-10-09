package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Luolc/agent-fleet/internal/exit"
)

// Scope is a scope's own settings: what no repo can hold, read from
// `$XDG_CONFIG_HOME/fleet/<scope>.json` (`~/.config` when
// XDG_CONFIG_HOME is unset or empty). Every field is optional.
type Scope struct {
	// LinearTeam is the team key thread tickets are created in; empty
	// means thread tickets are off, and threads still run.
	LinearTeam string
	// FednetSocket is the fednet client's socket, for `fednet client
	// post`; empty means fleet cannot post to a thread.
	FednetSocket string
}

// scopeFile is the JSON as written.
type scopeFile struct {
	Linear *struct {
		Team string `json:"team"`
	} `json:"linear"`
	Fednet *struct {
		Socket string `json:"socket"`
	} `json:"fednet"`
}

// ScopePath is the config file of `scope`.
func ScopePath(scope string) (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home := os.Getenv("HOME")
		if home == "" {
			return "", exit.Environmentf("neither XDG_CONFIG_HOME nor HOME is set, cannot locate the scope config")
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "fleet", scope+".json"), nil
}

// LoadScope reads the settings of `scope`. A missing file is all
// defaults; a file that cannot be read or is not valid is exit 1.
func LoadScope(scope string) (*Scope, error) {
	path, err := ScopePath(scope)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Scope{}, nil
	}
	if err != nil {
		return nil, exit.Refusedf("cannot read %s: %v", path, err)
	}
	t, err := ParseScope(data)
	if err != nil {
		return nil, exit.Refusedf("%s: %v", path, err)
	}
	return t, nil
}

// ParseScope reads a scope config's content. Unknown keys and wrong
// types are errors; a section that is given must name its value.
func ParseScope(data []byte) (*Scope, error) {
	var f scopeFile
	if err := decodeStrict(data, &f); err != nil {
		return nil, err
	}
	t := &Scope{}
	if f.Linear != nil {
		if f.Linear.Team == "" {
			return nil, errors.New("linear.team: must be set when linear is")
		}
		t.LinearTeam = f.Linear.Team
	}
	if f.Fednet != nil {
		if f.Fednet.Socket == "" {
			return nil, errors.New("fednet.socket: must be set when fednet is")
		}
		t.FednetSocket = f.Fednet.Socket
	}
	return t, nil
}

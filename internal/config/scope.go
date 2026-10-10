package config

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

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
	// Paths are the machine's directories, absolute.
	Paths Paths
	// Channels are the channel names fleet serves.
	Channels Channels
	// home is what `~/` stands for, in Paths and in Tilde.
	home string
}

// Tilde is `path` as the prompts show it: `~/...` under the home
// directory, else as it is.
func (s *Scope) Tilde(path string) string {
	if rest, ok := strings.CutPrefix(path, s.home+"/"); ok && s.home != "" {
		return "~/" + rest
	}
	return path
}

// Paths are where fleet finds checkouts and puts worktrees, `~/` expanded.
type Paths struct {
	// Checkouts holds the repos' main checkouts, `<Checkouts>/<repo>`.
	Checkouts string
	// Initiatives holds the initiatives' checkouts, `<Initiatives>/<I>`.
	Initiatives string
	// Worktrees holds `fleet worktree`'s worktrees, `<Worktrees>/<repo>/<leaf>`.
	Worktrees string
	// Scratch is where the prompts tell agents to keep their files.
	Scratch string
}

// Channels decide which channel a thread belongs to: `<RepoPrefix><R>` to
// the repo R, `<InitiativePrefix><I>` to the initiative I, whose repo has
// the same name; a direct message to GeneralInitiative.
type Channels struct {
	RepoPrefix        string
	InitiativePrefix  string
	GeneralInitiative string
}

// DefaultPaths and DefaultChannels are what an absent key means.
var (
	DefaultPaths    = Paths{Checkouts: "~/dev", Initiatives: "~/x-repo", Worktrees: "~/wt", Scratch: "~/scratch"}
	DefaultChannels = Channels{RepoPrefix: "repo-", InitiativePrefix: "x-repo-", GeneralInitiative: "general"}
)

// scopeFile is the JSON as written.
type scopeFile struct {
	Linear *struct {
		Team string `json:"team"`
	} `json:"linear"`
	Fednet *struct {
		Socket string `json:"socket"`
	} `json:"fednet"`
	Paths *struct {
		Checkouts   *string `json:"checkouts"`
		Initiatives *string `json:"initiatives"`
		Worktrees   *string `json:"worktrees"`
		Scratch     *string `json:"scratch"`
	} `json:"paths"`
	Channels *struct {
		RepoPrefix        *string `json:"repo_prefix"`
		InitiativePrefix  *string `json:"initiative_prefix"`
		GeneralInitiative *string `json:"general_initiative"`
	} `json:"channels"`
}

// setting is one key of the `paths` or `channels` section: nil when the
// key is absent.
type setting struct {
	key   string
	value *string
	into  *string
}

// apply sets each given key, refusing an empty value: an absent key
// means the default.
func apply(settings []setting) error {
	for _, s := range settings {
		if s.value == nil {
			continue
		}
		if *s.value == "" {
			return errors.New(s.key + ": must not be empty; leave the key out for the default")
		}
		*s.into = *s.value
	}
	return nil
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
	home := os.Getenv("HOME")
	if home == "" {
		return nil, exit.Environmentf("HOME is not set, cannot expand the scope config's paths")
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		data = []byte("{}")
	} else if err != nil {
		return nil, exit.Refusedf("cannot read %s: %v", path, err)
	}
	t, err := ParseScope(data, home)
	if err != nil {
		return nil, exit.Refusedf("%s: %v", path, err)
	}
	return t, nil
}

// ParseScope reads a scope config's content, expanding `~/` in paths to
// `home`. Unknown keys, wrong types and nulls are errors; a section that
// is given must name its value.
func ParseScope(data []byte, home string) (*Scope, error) {
	var f scopeFile
	if err := decodeStrict(data, &f); err != nil {
		return nil, err
	}
	// A top-level null decodes into an empty struct, all defaults.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || raw == nil {
		return nil, errors.New("not a valid config: not a JSON object")
	}
	if err := refuseNulls(data, ""); err != nil {
		return nil, err
	}
	t := &Scope{Paths: DefaultPaths, Channels: DefaultChannels, home: filepath.Clean(home)}
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
	if p := f.Paths; p != nil {
		if err := apply([]setting{{"paths.checkouts", p.Checkouts, &t.Paths.Checkouts},
			{"paths.initiatives", p.Initiatives, &t.Paths.Initiatives}, {"paths.worktrees", p.Worktrees, &t.Paths.Worktrees},
			{"paths.scratch", p.Scratch, &t.Paths.Scratch}}); err != nil {
			return nil, err
		}
	}
	if c := f.Channels; c != nil {
		if err := apply([]setting{{"channels.repo_prefix", c.RepoPrefix, &t.Channels.RepoPrefix},
			{"channels.initiative_prefix", c.InitiativePrefix, &t.Channels.InitiativePrefix},
			{"channels.general_initiative", c.GeneralInitiative, &t.Channels.GeneralInitiative}}); err != nil {
			return nil, err
		}
	}
	for _, p := range []struct {
		key  string
		path *string
	}{{"paths.checkouts", &t.Paths.Checkouts}, {"paths.initiatives", &t.Paths.Initiatives},
		{"paths.worktrees", &t.Paths.Worktrees}, {"paths.scratch", &t.Paths.Scratch}} {
		expanded, err := expand(*p.path, home)
		if err != nil {
			return nil, errors.New(p.key + ": " + err.Error())
		}
		*p.path = expanded
	}
	if err := t.Channels.check(); err != nil {
		return nil, err
	}
	return t, nil
}

// refuseNulls refuses a null anywhere in the object `data`, naming its
// key: a null would decode as an absent key, which means the default, so
// `"initiative_prefix": null` must not quietly keep serving `x-repo-*`.
func refuseNulls(data []byte, prefix string) error {
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	for key, value := range raw {
		if string(value) == "null" {
			return errors.New(prefix + key + ": must not be null; leave the key out for the default")
		}
		if err := refuseNulls(value, prefix+key+"."); err != nil {
			return err
		}
	}
	return nil
}

// expand is `path` absolute: as it is when absolute, `~/` replaced by
// `home`; anything else is an error.
func expand(path, home string) (string, error) {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		return filepath.Join(home, rest), nil
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("must be an absolute path or start with ~/")
	}
	return filepath.Clean(path), nil
}

var channelPrefix = regexp.MustCompile(`^[a-z0-9_-]+$`)

// check refuses names that are not channel names, and prefixes that would
// read one channel name two ways.
func (c Channels) check() error {
	if !channelPrefix.MatchString(c.RepoPrefix) {
		return errors.New("channels.repo_prefix: must be made of [a-z0-9_-]")
	}
	if !channelPrefix.MatchString(c.InitiativePrefix) {
		return errors.New("channels.initiative_prefix: must be made of [a-z0-9_-]")
	}
	if !channelPrefix.MatchString(c.GeneralInitiative) {
		return errors.New("channels.general_initiative: must be made of [a-z0-9_-]")
	}
	if strings.HasPrefix(c.RepoPrefix, c.InitiativePrefix) || strings.HasPrefix(c.InitiativePrefix, c.RepoPrefix) {
		return errors.New("channels: repo_prefix and initiative_prefix must not start one with the other")
	}
	return nil
}

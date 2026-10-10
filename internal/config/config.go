// Package config is a repo's fleet settings: `.fleet/config.json` in its
// main checkout (docs/design.md). Only numbers and switches that fleet
// itself reads live here; what agents read goes in the repo's AGENTS.md.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Luolc/agent-fleet/internal/exit"
)

// Linear is where a repo's work orders go: a team key and a project name
// within that team.
type Linear struct {
	Team    string `json:"team"`
	Project string `json:"project"`
}

// Config is a repo's settings, defaults filled in.
type Config struct {
	// MaxAgentsPerJob is the live agents a job may hold, counting the lead.
	MaxAgentsPerJob int
	// ResourceCheck is whether spawn checks the load and memory first.
	ResourceCheck bool
	// Linear is nil when the repo does not use Linear.
	Linear *Linear
}

// file is the JSON as written: a nil field was not given.
type file struct {
	MaxAgentsPerJob *int    `json:"max_agents_per_job"`
	ResourceCheck   *bool   `json:"resource_check"`
	Linear          *Linear `json:"linear"`
}

// Default is the config of a job whose checkout has no `.fleet/config.json`.
func Default() *Config {
	return &Config{MaxAgentsPerJob: 16, ResourceCheck: true}
}

// Path is the config file of the main checkout `checkout`.
func Path(checkout string) string {
	return filepath.Join(checkout, ".fleet", "config.json")
}

// Load reads the config of the main checkout `checkout`. A missing file is
// all defaults; a file that cannot be read or is not a valid config is
// refused (exit 1), naming the key when there is one.
func Load(checkout string) (*Config, error) {
	path := Path(checkout)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return nil, exit.Refusedf("cannot read %s: %v", path, err)
	}
	c, err := Parse(data)
	if err != nil {
		return nil, exit.Refusedf("%s: %v", path, err)
	}
	return c, nil
}

// decodeStrict decodes one JSON object into `into`: unknown keys, wrong
// types and anything after the object are errors naming the key.
func decodeStrict(data []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field != "" {
			return errors.New(typeErr.Field + ": expected " + typeErr.Type.String() + ", got " + typeErr.Value)
		}
		return errors.New("not a valid config: " + err.Error())
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("not a valid config: more than one JSON value")
	}
	return nil
}

// Parse reads a config file's content. Unknown keys and wrong types are
// errors.
func Parse(data []byte) (*Config, error) {
	var f file
	if err := decodeStrict(data, &f); err != nil {
		return nil, err
	}
	// A null would decode as an absent key, and an absent key means the
	// default; `"linear": null` must not turn Linear off.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || raw == nil {
		return nil, errors.New("not a valid config: not a JSON object")
	}
	for _, key := range []string{"max_agents_per_job", "resource_check", "linear"} {
		if value, ok := raw[key]; ok && string(value) == "null" {
			return nil, errors.New(key + ": must not be null; leave the key out for the default")
		}
	}
	c := Default()
	if f.MaxAgentsPerJob != nil {
		if *f.MaxAgentsPerJob < 1 {
			return nil, errors.New("max_agents_per_job: must be at least 1")
		}
		c.MaxAgentsPerJob = *f.MaxAgentsPerJob
	}
	if f.ResourceCheck != nil {
		c.ResourceCheck = *f.ResourceCheck
	}
	if f.Linear != nil {
		if f.Linear.Team == "" {
			return nil, errors.New("linear.team: must be set when linear is")
		}
		if f.Linear.Project == "" {
			return nil, errors.New("linear.project: must be set when linear is")
		}
		c.Linear = f.Linear
	}
	return c, nil
}

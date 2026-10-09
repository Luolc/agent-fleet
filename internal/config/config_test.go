package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Luolc/agent-fleet/internal/exit"
)

func TestAMissingFileIsAllDefaults(t *testing.T) {
	c, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxAgentsPerJob != 4 || !c.ResourceCheck || c.Linear != nil {
		t.Errorf("%+v", c)
	}
}

func TestEveryKeyIsRead(t *testing.T) {
	c, err := Parse([]byte(`{"max_agents_per_job": 2, "resource_check": false,
		"linear": {"team": "EX", "project": "Example project"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxAgentsPerJob != 2 || c.ResourceCheck || c.Linear == nil ||
		c.Linear.Team != "EX" || c.Linear.Project != "Example project" {
		t.Errorf("%+v %+v", c, c.Linear)
	}
}

func TestAnInvalidFileIsRefusedNamingTheKey(t *testing.T) {
	for body, says := range map[string]string{
		`{"max_agents_per_job": "4"}`:            "max_agents_per_job",
		`{"max_agents_per_job": 0}`:              "max_agents_per_job",
		`{"resource_check": 1}`:                  "resource_check",
		`{"linear": {"project": "P"}}`:           "linear.team",
		`{"linear": {"team": "EX"}}`:             "linear.project",
		`{"linear": {"team": 7, "project": ""}}`: "linear.team",
		`{"max_agent_per_job": 4}`:               `"max_agent_per_job"`,
		`{"resource_check": true`:                "not a valid config",
		`{"max_agents_per_job": null}`:           "max_agents_per_job: must not be null",
		`{"resource_check": null}`:               "resource_check: must not be null",
		`{"linear": null}`:                       "linear: must not be null",
		"{\"linear\" :\n  null }":                "linear: must not be null",
		`null`:                                   "not a JSON object",
		`{} {}`:                                  "more than one JSON value",
	} {
		if _, err := Parse([]byte(body)); err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("%s: %v, want it to name %s", body, err, says)
		}
	}
}

func TestLoadRefusesWithTheFileAndExit1(t *testing.T) {
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, ".fleet"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(checkout), []byte(`{"max_agents_per_job": -1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(checkout)
	var failure *exit.Failure
	if !errors.As(err, &failure) || failure.Code != exit.Refused ||
		!strings.Contains(err.Error(), Path(checkout)+": max_agents_per_job") {
		t.Errorf("%v", err)
	}
}

func TestTargetConfigIsOptionalAndStrict(t *testing.T) {
	got, err := ParseTarget([]byte(`{"linear": {"team": "EX"}, "fednet": {"socket": "/run/fednet.sock"}}`))
	if err != nil || got.LinearTeam != "EX" || got.FednetSocket != "/run/fednet.sock" {
		t.Errorf("%+v, %v", got, err)
	}
	got, err = ParseTarget([]byte(`{}`))
	if err != nil || got.LinearTeam != "" || got.FednetSocket != "" {
		t.Errorf("empty object: %+v, %v", got, err)
	}
	for _, bad := range []string{`{"linear": {}}`, `{"fednet": {"socket": ""}}`, `{"team": "EX"}`, `[]`, `{"linear": {"team": 1}}`} {
		if _, err := ParseTarget([]byte(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", t.TempDir())
	target, err := LoadTarget("default")
	if err != nil || target.LinearTeam != "" {
		t.Errorf("missing file: %+v, %v", target, err)
	}
	path, err := TargetPath("default")
	if err != nil || !strings.HasSuffix(path, "/.config/fleet/default.json") {
		t.Errorf("path = %q, %v", path, err)
	}
}

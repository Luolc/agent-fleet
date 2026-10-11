package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Luolc/agent-fleet/internal/exit"
)

func TestAMissingFileIsAllDefaults(t *testing.T) {
	c, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxAgentsPerJob != 16 || !c.ResourceCheck || c.Linear != nil {
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

func TestScopeConfigIsOptionalAndStrict(t *testing.T) {
	got, err := ParseScope([]byte(`{"linear": {"team": "EX"}, "fednet": {"socket": "/run/fednet.sock"}, "unblock": {"project": "Fleet"}}`), "/home/u")
	if err != nil || got.LinearTeam != "EX" || got.FednetSocket != "/run/fednet.sock" || got.UnblockProject != "Fleet" {
		t.Errorf("%+v, %v", got, err)
	}
	got, err = ParseScope([]byte(`{}`), "/home/u")
	if err != nil || got.LinearTeam != "" || got.FednetSocket != "" || got.UnblockProject != "" {
		t.Errorf("empty object: %+v, %v", got, err)
	}
	for _, bad := range []string{`{"linear": {}}`, `{"fednet": {"socket": ""}}`, `{"unblock": {}}`, `{"team": "EX"}`, `[]`, `{"linear": {"team": 1}}`} {
		if _, err := ParseScope([]byte(bad), "/home/u"); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", t.TempDir())
	scope, err := LoadScope("main")
	if err != nil || scope.LinearTeam != "" {
		t.Errorf("missing file: %+v, %v", scope, err)
	}
	path, err := ScopePath("main")
	if err != nil || !strings.HasSuffix(path, "/.config/fleet/main.json") {
		t.Errorf("path = %q, %v", path, err)
	}
}

func TestScopePathsAndChannelsDefaultToTheConventionsAndCanBeSet(t *testing.T) {
	got, err := ParseScope([]byte(`{}`), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{Checkouts: "/home/u/dev", Initiatives: "/home/u/x-repo", Worktrees: "/home/u/wt", Scratch: "/home/u/scratch"}
	if got.Paths != want || got.Channels != DefaultChannels {
		t.Errorf("defaults: %+v %+v", got.Paths, got.Channels)
	}
	if got.Tilde("/home/u/dev/r") != "~/dev/r" || got.Tilde("/srv/r") != "/srv/r" || got.Tilde("/home/user/r") != "/home/user/r" {
		t.Errorf("Tilde: %q %q %q", got.Tilde("/home/u/dev/r"), got.Tilde("/srv/r"), got.Tilde("/home/user/r"))
	}
	got, err = ParseScope([]byte(`{"paths": {"checkouts": "/srv/src/", "worktrees": "~/trees"},
		"channels": {"repo_prefix": "proj-", "initiative_prefix": "multi-", "general_initiative": "lobby"}}`), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	want = Paths{Checkouts: "/srv/src", Initiatives: "/home/u/x-repo", Worktrees: "/home/u/trees", Scratch: "/home/u/scratch"}
	if got.Paths != want || got.Channels != (Channels{"proj-", "multi-", "lobby"}) {
		t.Errorf("set: %+v %+v", got.Paths, got.Channels)
	}
}

func TestScopePathsAndChannelsRefuseWhatCannotWork(t *testing.T) {
	for body, says := range map[string]string{
		`{"paths": {"checkouts": "dev"}}`:                                     "paths.checkouts: must be an absolute path",
		`{"channels": {"repo_prefix": "x-", "initiative_prefix": "x-repo-"}}`: "must not start one with the other",
		`{"channels": {"initiative_prefix": null}}`:                           "channels.initiative_prefix: must not be null",
		`null`: "not a JSON object",
	} {
		if _, err := ParseScope([]byte(body), "/home/u"); err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("%s: %v, want it to say %s", body, err, says)
		}
	}
}

func TestWatchLimitsAreReadAndCheckedInBothFiles(t *testing.T) {
	c, err := Parse([]byte(`{"watch": {"worker_stale": "5m"}}`))
	if err != nil || c.Watch.WorkerStale != 5*time.Minute || c.Watch.LeadQuestion != 72*time.Hour {
		t.Errorf("%+v, %v", c, err)
	}
	s, err := ParseScope([]byte(`{"watch": {"lead_question": "48h", "thread_quiet": "1h", "reminders": ["1h", "2h"],
		"parent_stale": "24h", "parent_agents": 3}}`), "/home/u")
	if err != nil || s.Watch.LeadQuestion != 48*time.Hour || s.Watch.ThreadQuiet != time.Hour ||
		!slices.Equal(s.Watch.Reminders, []time.Duration{time.Hour, 2 * time.Hour}) || s.Watch.ThreadIdle != 72*time.Hour ||
		s.Watch.ParentStale != 24*time.Hour || s.Watch.ParentAgents != 3 {
		t.Errorf("%+v, %v", s, err)
	}
	if !slices.Equal(DefaultWatch.Reminders, []time.Duration{30 * time.Minute, 3 * time.Hour, 24 * time.Hour}) {
		t.Errorf("%v", DefaultWatch.Reminders)
	}
	for body, says := range map[string]string{
		`{"watch": {"thread_quiet": "1h"}}`:   `"thread_quiet"`,
		`{"watch": {"worker_stale": "soon"}}`: "watch.worker_stale: must be a positive duration",
		`{"watch": {"worker_stale": 600}}`:    "watch.worker_stale",
		`{"watch": {"lead_question": null}}`:  "watch.lead_question: must not be null",
		`{"watch": {"lead_question": "-1h"}}`: "watch.lead_question: must be a positive duration",
	} {
		if _, err := Parse([]byte(body)); err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("%s: %v, want it to name %s", body, err, says)
		}
	}
	for body, says := range map[string]string{
		`{"watch": {"reminders": ["3h", "30m"]}}`: "watch.reminders: must be in increasing order",
		`{"watch": {"reminders": "30m"}}`:         "watch.reminders",
		`{"watch": {"thread_idle": "0s"}}`:        "watch.thread_idle: must be a positive duration",
		`{"watch": {"stale": "1h"}}`:              `"stale"`,
		`{"watch": {"parent_agents": 0}}`:         "watch.parent_agents: must be a positive number",
		`{"watch": {"parent_agents": "2"}}`:       "watch.parent_agents",
	} {
		if _, err := ParseScope([]byte(body), "/home/u"); err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("%s: %v, want it to name %s", body, err, says)
		}
	}
}

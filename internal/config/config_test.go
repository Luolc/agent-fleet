package config

import (
	"errors"
	"fmt"
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
	got, err = ParseScope([]byte(`{"fednet": {"socket": "/run/fednet.sock", "slack_url": "https://slack.example.test/"}}`), "/home/u")
	if err != nil || got.SlackURL != "https://slack.example.test" {
		t.Errorf("slack_url: %+v, %v", got, err)
	}
	got, err = ParseScope([]byte(`{"fednet": {"socket": "/run/fednet.sock", "slack_url": "https://slack.example.test?"}}`), "/home/u")
	if err != nil || got.SlackURL != "https://slack.example.test" {
		t.Errorf("slack_url ending in ?: %+v, %v", got, err)
	}
	for _, bad := range []string{`""`, `"http://slack.example.test"`, `"slack.example.test"`, `"https://slack.example.test/archives"`} {
		if _, err := ParseScope([]byte(`{"fednet": {"socket": "/run/fednet.sock", "slack_url": `+bad+`}}`), "/home/u"); err == nil ||
			!strings.Contains(err.Error(), "fednet.slack_url") {
			t.Errorf("slack_url %s: %v", bad, err)
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

func TestTheLoadMultiplierDefaultsTo1AndMustBeAbove0(t *testing.T) {
	for body, want := range map[string]float64{`{}`: 1, `{"resources": {"load_multiplier": 1.5}}`: 1.5,
		`{"resources": {"load_multiplier": 0.5}}`: 0.5} {
		got, err := ParseScope([]byte(body), "/home/u")
		if err != nil || got.LoadMultiplier != want {
			t.Errorf("%s: %+v, %v", body, got, err)
		}
	}
	for body, says := range map[string]string{
		`{"resources": {"load_multiplier": 0}}`:     "resources.load_multiplier: must be above 0",
		`{"resources": {"load_multiplier": -1}}`:    "resources.load_multiplier: must be above 0",
		`{"resources": {"load_multiplier": null}}`:  "resources.load_multiplier: must not be null",
		`{"resources": {"load_multiplier": "1.5"}}`: "resources.load_multiplier: expected float64",
		`{"resources": {}}`:                         "resources: must set load_multiplier, min_available_mib or agent_mib",
		`{"resources": null}`:                       "resources: must not be null",
	} {
		if _, err := ParseScope([]byte(body), "/home/u"); err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("%s: %v, want it to say %s", body, err, says)
		}
	}
}

func TestTheMemoryFloorAndTheAgentsShareDefaultTo2048And512MiB(t *testing.T) {
	for body, want := range map[string][3]float64{
		`{}`:                                 {1, 2048, 512},
		`{"resources": {"agent_mib": 1024}}`: {1, 2048, 1024},
		`{"resources": {"min_available_mib": 0, "agent_mib": 0}}`:          {1, 0, 0},
		`{"resources": {"load_multiplier": 2, "min_available_mib": 4096}}`: {2, 4096, 512},
	} {
		got, err := ParseScope([]byte(body), "/home/u")
		if err != nil || got.LoadMultiplier != want[0] || float64(got.MinAvailableMiB) != want[1] || float64(got.AgentMiB) != want[2] {
			t.Errorf("%s: %+v, %v", body, got, err)
		}
	}
	for body, says := range map[string]string{
		`{"resources": {"min_available_mib": -1}}`: "resources.min_available_mib: must be at least 0",
		`{"resources": {"agent_mib": -512}}`:       "resources.agent_mib: must be at least 0",
		`{"resources": {"agent_mib": 1.5}}`:        "resources.agent_mib",
		`{"resources": {"agent_mib": null}}`:       "resources.agent_mib: must not be null",
	} {
		if _, err := ParseScope([]byte(body), "/home/u"); err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("%s: %v, want it to say %s", body, err, says)
		}
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
	s, err := ParseScope([]byte(`{"watch": {"lead_question": "48h", "thread_quiet": "1h", "reminders": ["1h", "2h"]}}`),
		"/home/u")
	if err != nil || s.Watch.LeadQuestion != 48*time.Hour || s.Watch.ThreadQuiet != time.Hour ||
		!slices.Equal(s.Watch.Reminders, []time.Duration{time.Hour, 2 * time.Hour}) || s.Watch.ThreadIdle != 72*time.Hour ||
		len(s.Ignored) != 0 {
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
		`{"watch": {"parent_agents": "2"}}`:       "watch.parent_agents",
	} {
		if _, err := ParseScope([]byte(body), "/home/u"); err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("%s: %v, want it to name %s", body, err, says)
		}
	}
}

// The keys of the rules that left fleet are accepted, so `inbox` goes on
// serving, and listed for `watch` to name; their files are not read.
func TestTheRetiredKeysAreAcceptedAndListed(t *testing.T) {
	s, err := ParseScope([]byte(`{"watch": {"parent_stale": "24h", "parent_agents": 3},
		"prompts": {"revisit": {"replace": "missing.md"}, "unblock": {"append": "u.md"}, "screen_guidance": {"append": "g.md"},
			"lead": {"append": "l.md"}}}`), "/home/u")
	want := []string{"watch.parent_stale", "watch.parent_agents", "prompts.revisit", "prompts.screen_guidance", "prompts.unblock"}
	if err != nil || !slices.Equal(s.Ignored, want) || len(s.Prompts) != 1 {
		t.Errorf("%+v, %v", s, err)
	}
}

func TestWatchesKeepTheDefaultsAndReadEachEntry(t *testing.T) {
	s, err := ParseScope([]byte(`{}`), "/home/u")
	if err != nil || !slices.EqualFunc(s.Watches, DefaultWatches(), func(a, b Entry) bool { return a.Name == b.Name }) {
		t.Fatalf("%+v, %v", s, err)
	}
	s, err = ParseScope([]byte(`{"watches": {"stale-workers": {"every": "15m", "prompt": "~/stale.md"},
		"quiet-threads": {"source": "quiet-threads", "off": true},
		"ci": {"source": {"command": ["/bin/ci-red", "--all"]}, "do": "tell", "env": ["EXAMPLE_TOKEN"], "per_run": 3,
			"again": "2h", "max": 4},
		"nightly": {"do": "worker", "every": "24h", "prompt": "nightly.md", "model": "example-small", "effort": "low"},
		"unblock": {"every": "10m"}, "model-changes": {"off": true}}}`), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{
		{Name: "ci", Command: []string{"/bin/ci-red", "--all"}, Do: "tell", Env: []string{"EXAMPLE_TOKEN"}, PerRun: 3,
			Again: 2 * time.Hour, Max: 4},
		{Name: "model-changes", Source: ModelChanges, Do: "tell", Off: true},
		{Name: "nightly", Every: 24 * time.Hour, Do: "worker", Prompt: "nightly.md", Model: "example-small", Effort: "low",
			PerRun: 1},
		{Name: "quiet-threads", Source: QuietThreads, Do: "tell", Off: true},
		{Name: "stale-workers", Source: StaleAgents, Do: "tell", Every: 15 * time.Minute, Prompt: "/home/u/stale.md"},
		{Name: "unblock", Source: BlockedScreens, Do: "worker", Every: 10 * time.Minute},
	}
	if fmt.Sprint(s.Watches) != fmt.Sprint(want) {
		t.Errorf("\n got %+v\nwant %+v", s.Watches, want)
	}
	if s, err = ParseScope([]byte(`{"watches": {"ci": {"source": {"command": ["c"]}, "do": "tell"}}}`), "/home/u"); err != nil ||
		s.Watches[0].PerRun != 1 || s.Watches[0].Again != 0 || s.Watches[0].Max != 0 {
		t.Errorf("%+v, %v", s, err)
	}
	command := `"source": {"command": ["c"]}, "do": "tell"`
	for body, says := range map[string]string{
		`{"watches": {"CI": {` + command + `}}}`:                             "watches.CI: a name is made of [a-z0-9-]",
		`{"watches": {"ci": {"do": "tell"}}}`:                                "watches.ci.source: must be given",
		`{"watches": {"ci": {"source": {"command": ["c"]}}}}`:                "watches.ci.do: must be given",
		`{"watches": {"ci": {"source": {"command": []}, "do": "tell"}}}`:     "watches.ci.source.command: must name a program",
		`{"watches": {"ci": {"source": {"cmd": ["c"]}, "do": "tell"}}}`:      `watches.ci.source: must be a built-in source's name or {"command": [...]}`,
		`{"watches": {"ci": {"source": "stale-agents", "do": "tell"}}}`:      "watches.ci.source: stale-agents is the source of the default stale-workers",
		`{"watches": {"ci": {"source": "blocked-screens", "do": "worker"}}}`: "watches.ci.source: blocked-screens is the source of the default unblock",
		`{"watches": {"ci": {"source": "model-changes", "do": "tell"}}}`:     "watches.ci.source: model-changes is the source of the default model-changes",
		`{"watches": {"ci": {"source": "lint", "do": "tell"}}}`:              `watches.ci.source: unknown source "lint"`,
		`{"watches": {"ci": {"source": {"command": ["c"]}, "do": "ask"}}}`:   `watches.ci.do: unknown "ask"`,
		`{"watches": {"ci": {` + command + `, "env": ["A-B"]}}}`:             `watches.ci.env: "A-B" is not a variable's name`,
		`{"watches": {"ci": {` + command + `, "per_run": 0}}}`:               "watches.ci.per_run: must be a positive number",
		`{"watches": {"ci": {` + command + `, "again": "soon"}}}`:            "watches.ci.again: must be a positive duration",
		`{"watches": {"ci": {` + command + `, "model": "opus"}}}`:            "watches.ci.model: only a watch with `do: worker` starts an agent",
		`{"watches": {"ci": {"do": "worker", "effort": ""}}}`:                "watches.ci.effort: must not be empty",
		`{"watches": {"unblock": {"model": "opus"}}}`:                        "watches.unblock.model: a built-in source takes only every, prompt and off",
		`{"watches": {"ci": {` + command + `, "prompt": ""}}}`:               "watches.ci.prompt: must not be empty",
		`{"watches": {"ci": null}}`:                                          "watches.ci: must not be null",
		`{"watches": {"stale-workers": {"every": "0s"}}}`:                    "watches.stale-workers.every: must be a positive duration",
		`{"watches": {"stale-workers": {"max": 2}}}`:                         "watches.stale-workers.max: a built-in source takes only every, prompt and off",
		`{"watches": {"quiet-threads": {"source": {"command": ["c"]}}}}`:     "watches.quiet-threads.source: the default quiet-threads keeps its source quiet-threads",
		`{"watches": {"quiet-threads": {"source": "stale-agents"}}}`:         "watches.quiet-threads.source: stale-agents is the source of the default stale-workers",
	} {
		if _, err := ParseScope([]byte(body), "/home/u"); err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("%s: %v, want it to name %s", body, err, says)
		}
	}
}

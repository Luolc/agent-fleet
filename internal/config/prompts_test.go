package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Luolc/agent-fleet/internal/exit"
)

func TestPromptsAndModelsAreReadWithTheirPathsResolved(t *testing.T) {
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("HOME", "/home/u")
	if err := os.MkdirAll(filepath.Join(config, "fleet"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{
		"prompts": {
			"lead": {"replace": "prompts/lead.md", "base": "prompts/../lead.base.md", "append": "~/notes/lead.md"},
			"screen_guidance": {"append": "/srv/fleet/screens.md"}
		},
		"models": {"lead": {"model": "example-large", "effort": "medium"}, "worker": {"effort": "low"}}
	}`
	if err := os.WriteFile(filepath.Join(config, "fleet", "main.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadScope("main")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Prompt{
		"lead": {Replace: filepath.Join(config, "fleet/prompts/lead.md"), Append: "/home/u/notes/lead.md",
			Base: filepath.Join(config, "fleet/lead.base.md")},
		"screen_guidance": {Append: "/srv/fleet/screens.md"},
	}
	if !reflect.DeepEqual(got.Prompts, want) {
		t.Errorf("prompts:\n%+v\nwant\n%+v", got.Prompts, want)
	}
	// What a model or effort leaves out keeps the built-in value.
	if want := (Models{Lead: Model{"example-large", "medium"}, Worker: Model{"opus", "low"}}); got.Models != want {
		t.Errorf("models: %+v", got.Models)
	}
	empty, err := ParseScope([]byte(`{}`), "/home/u")
	if err != nil || len(empty.Prompts) != 0 || empty.Models != DefaultModels {
		t.Errorf("no keys: %+v %+v, %v", empty.Prompts, empty.Models, err)
	}
}

func TestPromptsAndModelsRefuseWhatCannotWork(t *testing.T) {
	for body, says := range map[string]string{
		`{"prompts": {"screen-guidance": {"append": "a.md"}}}`:      "prompts.screen-guidance: unknown name; the names are thread, lead, worker, revisit, unblock, screen_guidance",
		`{"prompts": {"lead": {}}}`:                                 "prompts.lead: must have replace or append",
		`{"prompts": {"lead": {"append": "a.md", "base": "b.md"}}}`: "prompts.lead.base: only goes with replace",
		`{"prompts": {"lead": {"append": ""}}}`:                     "prompts.lead.append: must not be empty",
		`{"prompts": {"lead": {"replace": "r.md", "base": ""}}}`:    "prompts.lead.base: must not be empty",
		`{"prompts": {"lead": null}}`:                               "prompts.lead: must not be null",
		`{"prompts": {"lead": {"append": null}}}`:                   "prompts.lead.append: must not be null",
		`{"prompts": {"lead": {"prepend": "a.md"}}}`:                `"prepend"`,
		`{"prompts": {"lead": "a.md"}}`:                             "prompts.lead: expected config.promptFile",
		`{"prompts": null}`:                                         "prompts: must not be null",
		`{"models": {"lead": {"model": ""}}}`:                       "models.lead.model: must not be empty",
		`{"models": {"worker": {"effort": null}}}`:                  "models.worker.effort: must not be null",
		`{"models": {"thread": {"model": "m"}}}`:                    `"thread"`,
		`{"models": {"lead": {"model": 4}}}`:                        "models.lead.model: expected string",
	} {
		if _, err := ParseScope([]byte(body), "/home/u"); err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("%s: %v, want it to say %s", body, err, says)
		}
	}
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	if err := os.MkdirAll(filepath.Join(config, "fleet"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "fleet", "main.json"), []byte(`{"prompts": {"leader": {"append": "a.md"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadScope("main")
	var failure *exit.Failure
	if !errors.As(err, &failure) || failure.Code != exit.Refused || !strings.Contains(err.Error(), "prompts.leader: unknown name") {
		t.Errorf("LoadScope: %v", err)
	}
}

package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Luolc/agent-fleet/internal/config"
)

// shown is one run of `prompt show`.
type shown struct {
	stdout, stderr string
	err            error
}

// promptConfig writes the scope config `main.json` with the content
// `config` and the files `files` (paths relative to the config's
// directory) under a config home of its own, and returns that directory.
func promptConfig(t *testing.T, config string, files map[string]string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("FLEET_SCOPE", "")
	dir := filepath.Join(home, ".config", "fleet")
	files["main.json"] = config
	for name, text := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func show(args PromptShowArgs) shown {
	var stdout, stderr bytes.Buffer
	_, err := PromptShow(&stdout, &stderr, args)
	return shown{stdout.String(), stderr.String(), err}
}

func TestBuiltinIsTheEmbeddedTextByteForByte(t *testing.T) {
	// A config that would change every text, and one that cannot be read:
	// neither reaches --builtin.
	promptConfig(t, `{"prompts": {"worker": {"append": "missing.md"}}}`, map[string]string{})
	for _, name := range config.PromptNames {
		got := show(PromptShowArgs{Name: &name, Builtin: true})
		if got.err != nil || got.stdout != specOf(name).builtin || got.stderr != "" {
			t.Errorf("%s --builtin: err %v, stderr %q, stdout equal: %v", name, got.err, got.stderr,
				got.stdout == specOf(name).builtin)
		}
	}
	for name, want := range map[string]string{"watch:stale-workers": staleWorkersPrompt,
		"watch:quiet-threads": quietThreadsPrompt, "watch:unblock": unblockPrompt + "\n" + screenGuidance} {
		got := show(PromptShowArgs{Name: &name, Builtin: true})
		if got.err != nil || got.stdout != want || got.stderr != "" {
			t.Errorf("%s --builtin: err %v, stderr %q, stdout equal: %v", name, got.err, got.stderr, got.stdout == want)
		}
	}
	ci := "watch:ci"
	if got := show(PromptShowArgs{Name: &ci, Builtin: true}); exitCode(got.err) != 1 || got.stdout != "" ||
		!strings.Contains(got.err.Error(), "the default watches' are watch:quiet-threads, watch:stale-workers, watch:unblock") {
		t.Errorf("watch:ci --builtin: err %v, stdout %q", got.err, got.stdout)
	}
}

func TestAReplacementIsBehindOnlyWhenItsBaseIsNotTheBuiltInText(t *testing.T) {
	conf := `{"prompts": {"lead": {"replace": "prompts/lead.md", "base": "prompts/lead.base.md"}}}`
	lead := "lead"
	for _, c := range []struct {
		base   string
		behind bool
	}{{leadPrompt, false}, {leadPrompt + "An older line.\n", true}} {
		dir := promptConfig(t, conf, map[string]string{"prompts/lead.md": "Lead {{job}}.\n", "prompts/lead.base.md": c.base})
		for _, got := range []shown{show(PromptShowArgs{}), show(PromptShowArgs{Name: &lead})} {
			if got.err != nil {
				t.Fatalf("base behind %v: %v", c.behind, got.err)
			}
			out := got.stdout + got.stderr
			diff := "diff -u ~/.config/fleet/prompts/lead.base.md <(fleet prompt show lead --builtin)"
			if strings.Contains(out, "behind:") != c.behind || strings.Contains(out, diff) != c.behind ||
				strings.Contains(out, "up to date: base (~/.config/fleet/prompts/lead.base.md)") == c.behind {
				t.Errorf("base behind %v (%s):\n%s", c.behind, dir, out)
			}
		}
	}
	promptConfig(t, `{"prompts": {"lead": {"replace": "lead.md"}}}`, map[string]string{"lead.md": "Lead.\n"})
	if got := show(PromptShowArgs{}); !strings.Contains(got.stdout, "  no base: which built-in text the replacement started from is unknown\n") {
		t.Errorf("no base:\n%s", got.stdout)
	}
}

func TestTheOverviewNotesAConfigTheUserCanWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anything")
	}
	conf := `{"prompts": {"worker": {"append": "prompts/worker.append.md"}},
		"watches": {"quiet-threads": {"prompt": "watches/quiet.md"}}}`
	note := "note: you can write ~/.config/fleet, ~/.config/fleet/main.json, ~/.config/fleet/prompts, ~/.config/fleet/prompts/worker.append.md, " +
		"~/.config/fleet/watches, ~/.config/fleet/watches/quiet.md: "
	dir := promptConfig(t, conf, map[string]string{"prompts/worker.append.md": "More.\n", "watches/quiet.md": "Ask {{key}}.\n"})
	if got := show(PromptShowArgs{}); got.err != nil || !strings.Contains(got.stdout, note) {
		t.Errorf("writable: %v\n%s", got.err, got.stdout)
	}
	for _, path := range []string{"prompts/worker.append.md", "watches/quiet.md", "main.json"} {
		if err := os.Chmod(filepath.Join(dir, path), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"prompts", "watches", "."} {
		if err := os.Chmod(filepath.Join(dir, path), 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, path), 0o755) })
	}
	got := show(PromptShowArgs{})
	if got.err != nil || strings.Contains(got.stdout, "note:") || !strings.Contains(got.stdout, "worker: built-in + built-in common + prompts.worker.append") {
		t.Errorf("read-only: %v\n%s", got.err, got.stdout)
	}
}

func TestEveryTextThatCannotBeUsedIsListedAndExits1(t *testing.T) {
	promptConfig(t, `{"prompts": {"worker": {"append": "w.md"}, "lead": {"append": "l.md"}, "common": {"replace": "missing.md"}}}`,
		map[string]string{"w.md": "Read {{repo}}.\n", "l.md": " \n"})
	got := show(PromptShowArgs{})
	if exitCode(got.err) != 1 || !strings.Contains(got.err.Error(), "4 of the texts cannot be used") {
		t.Fatalf("err %v", got.err)
	}
	for _, line := range []string{
		"common: error: prompts.common.replace (~/.config/fleet/missing.md): cannot read the file",
		"thread: error: prompts.common.replace (~/.config/fleet/missing.md): cannot read the file",
		"lead: error: prompts.lead.append (~/.config/fleet/l.md): the file is empty\n",
		"worker: error: prompts.worker.append (~/.config/fleet/w.md:1:6): unknown slot {{repo}}",
	} {
		if !strings.Contains(got.stdout, line) {
			t.Errorf("no %q in\n%s", line, got.stdout)
		}
	}
	worker := "worker"
	if got := show(PromptShowArgs{Name: &worker}); exitCode(got.err) != 1 || got.stdout != "" {
		t.Errorf("show worker: err %v, stdout %q", got.err, got.stdout)
	}
}

func TestOneTextShowsEachPartUnderItsSourceWithOnlyTheModelSlotsFilled(t *testing.T) {
	promptConfig(t, `{"models": {"lead": {"model": "example-large", "effort": "low"}},
		"prompts": {"thread": {"replace": "t.md", "append": "a.md"}}}`,
		map[string]string{"t.md": "Start leads with {{lead_model}} at {{lead_effort}} in {{cwd}}.\n", "a.md": "One more rule for {{agent}}.\n"})
	thread := "thread"
	got := show(PromptShowArgs{Name: &thread})
	want := "==> prompts.thread.replace (~/.config/fleet/t.md) <==\nStart leads with example-large at low in {{cwd}}.\n\n" +
		"==> built-in common <==\n" + commonPrompt + "\n" +
		"==> prompts.thread.append (~/.config/fleet/a.md) <==\nOne more rule for {{agent}}.\n"
	if got.err != nil || got.stdout != want {
		t.Errorf("err %v, stdout:\n%s", got.err, got.stdout)
	}
	for _, note := range []string{"note: {{lead_model}} {{lead_effort}} only keeps a file written for an older fleet",
		"note: the replacement does not use {{agent}} {{checkouts}}"} {
		if !strings.Contains(got.stderr, note) {
			t.Errorf("no %q in stderr: %s", note, got.stderr)
		}
	}
	// The built-in texts use no model slot, so nothing is noted.
	lead := "lead"
	if got := show(PromptShowArgs{Name: &lead}); got.err != nil || got.stderr != "" {
		t.Errorf("built-in lead: err %v, stderr %q", got.err, got.stderr)
	}
}

// watchesConfig changes two defaults' prompts (one file missing), turns
// the third off, and adds a `tell` watch with a prompt and a `worker`
// timer without one.
const watchesConfig = `{"watches": {
	"stale-workers": {"prompt": "watches/stale.md"},
	"quiet-threads": {"prompt": "watches/missing.md"},
	"unblock": {"off": true},
	"ci": {"source": {"command": ["ci-source"]}, "do": "tell", "prompt": "watches/ci.md"},
	"tick": {"do": "worker", "every": "24h"}}}`

func TestTheOverviewSaysWhereEachWatchsPromptComesFrom(t *testing.T) {
	promptConfig(t, watchesConfig, map[string]string{"watches/stale.md": "Check {{key}}.\n", "watches/ci.md": "CI {{text}}.\n"})
	got := show(PromptShowArgs{})
	if exitCode(got.err) != 1 || !strings.Contains(got.err.Error(), "1 of the texts cannot be used") {
		t.Fatalf("err %v", got.err)
	}
	want := "worker: built-in + built-in common\n" +
		"watch:ci: watches.ci.prompt (~/.config/fleet/watches/ci.md)\n" +
		"watch:quiet-threads: error: watches.quiet-threads.prompt (~/.config/fleet/watches/missing.md): cannot read the file: no such file or directory\n" +
		"watch:stale-workers: watches.stale-workers.prompt (~/.config/fleet/watches/stale.md)\n" +
		"watch:tick: no prompt\n" +
		"watch:unblock: off\n"
	if !strings.Contains(got.stdout, want) {
		t.Errorf("no\n%s\nin\n%s", want, got.stdout)
	}
	promptConfig(t, `{}`, map[string]string{})
	want = "worker: built-in + built-in common\nwatch:quiet-threads: built-in\nwatch:stale-workers: built-in\nwatch:unblock: built-in\n"
	if got := show(PromptShowArgs{}); got.err != nil || !strings.Contains(got.stdout, want) {
		t.Errorf("defaults: err %v, no\n%s\nin\n%s", got.err, want, got.stdout)
	}
}

func TestAWatchsPromptShowsUnderItsSourceWithItsSlotsAsWritten(t *testing.T) {
	promptConfig(t, watchesConfig, map[string]string{"watches/stale.md": "Check {{key}}.\n", "watches/ci.md": "CI {{text}}."})
	for _, c := range []struct{ name, stdout, stderr string }{
		{"watch:ci", "==> watches.ci.prompt (~/.config/fleet/watches/ci.md) <==\nCI {{text}}.\n", ""},
		{"watch:unblock", "==> built-in <==\n" + unblockPrompt + "\n" + screenGuidance, "note: the watch is off: `watch` does not run it\n"},
		{"watch:tick", "", "note: the watch has no prompt: its messages carry the subject's text without one\n"},
	} {
		got := show(PromptShowArgs{Name: &c.name})
		if got.err != nil || got.stdout != c.stdout || got.stderr != c.stderr {
			t.Errorf("%s: err %v, stderr %q, stdout:\n%s", c.name, got.err, got.stderr, got.stdout)
		}
	}
	for name, refusal := range map[string]string{
		"watch:quiet-threads": "watches.quiet-threads.prompt (~/.config/fleet/watches/missing.md): cannot read the file",
		"watch:nope":          `no watch "nope" in the scope; its watches are watch:ci, watch:quiet-threads, watch:stale-workers, watch:tick, watch:unblock`,
		"watches":             `no prompt "watches"; the names are common, thread, lead, worker, and watch:<name> for a watch's`,
	} {
		got := show(PromptShowArgs{Name: &name})
		if exitCode(got.err) != 1 || got.stdout != "" || !strings.Contains(got.err.Error(), refusal) {
			t.Errorf("%s: err %v, stdout %q", name, got.err, got.stdout)
		}
	}
}

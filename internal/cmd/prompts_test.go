package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Luolc/agent-fleet/internal/atb"
	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/identity"
)

// The first messages the tests read, each through loadPrompt with the
// scope's config, as the starts put them together; a config the test
// breaks fails here.

func rolePrompt(sc *config.Scope, id *identity.Identity, cwd, repo, leadCwd string, cap int, issue atb.Issue, parentIssue string) string {
	text, err := loadPrompt(sc, id.Role.String())
	if err != nil {
		panic(err)
	}
	return text.fill(roleSlots(sc, id, cwd, repo, leadCwd, cap, issue, parentIssue))
}

func (s *threadStart) prompt() string {
	var err error
	if s.text, err = loadPrompt(s.cfg, "thread"); err != nil {
		panic(err)
	}
	return s.firstMessage()
}

func (u *unblocking) revisitBody(p atb.Parent, name string) (string, error) {
	text, err := loadPrompt(u.cfg, "revisit")
	if err != nil {
		return "", err
	}
	return u.revisitMessage(text, p, name)
}

func (u *unblocking) helperPrompt(row stopRow, helper, screen, issue string) (string, error) {
	prompt, err := loadPrompt(u.cfg, "unblock")
	if err != nil {
		return "", err
	}
	guidance, err := loadPrompt(u.cfg, "screen_guidance")
	if err != nil {
		return "", err
	}
	return u.helperMessage(prompt, guidance, row, helper, screen, issue)
}

// promptScope is a scope config whose `prompts` are `prompts` and whose
// `models` are `models` (JSON, "" for none), with the files of `files`
// written in a directory of their own; a path in `prompts` is relative
// to that directory.
func promptScope(t *testing.T, prompts map[string]map[string]string, models string, files map[string]string) *config.Scope {
	t.Helper()
	dir := t.TempDir()
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	absolute := map[string]map[string]string{}
	for name, fields := range prompts {
		absolute[name] = map[string]string{}
		for field, path := range fields {
			absolute[name][field] = filepath.Join(dir, path)
		}
	}
	data, err := json.Marshal(map[string]any{"prompts": absolute})
	if err != nil {
		t.Fatal(err)
	}
	if models != "" {
		data = []byte(strings.TrimSuffix(string(data), "}") + `,"models":` + models + "}")
	}
	sc, err := config.ParseScope(data, "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// renders are the first messages of the six texts' agents (the guidance
// in the helper's) under the scope `sc`, from the contexts of the golden
// files `goldenOf` names.
func renders(t *testing.T, sc *config.Scope) map[string]string {
	t.Helper()
	conn, err := db.OpenAt(filepath.Join(t.TempDir(), "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	lead := &identity.Identity{Agent: "wire-lead", Role: identity.Lead, Parent: "thread-x", Scope: "main", Job: "wire"}
	worker := &identity.Identity{Agent: "wire-a", Role: identity.Worker, Parent: "wire-lead", Scope: "main", Job: "wire"}
	issue := atb.Issue{Identifier: "QT-12", URL: "https://linear.example.test/QT-12"}
	thread := &threadStart{cfg: sc, mapping: "x-repo-example-init", cwd: MappingDir(sc, "x-repo-example-init"),
		id:  &identity.Identity{Agent: "thread-c1-1-0", Role: identity.Thread, Scope: "main", Thread: "C1/1.0"},
		msg: inboundMessage{Thread: "C1/1.0", Text: "import the A table", User: "U0ABC", TS: "1700000000.000"}}
	u := &unblocking{watchRun: &watchRun{conn: conn, scope: "main", cfg: sc, now: 1700000000},
		dir: "/home/u/.local/state/fleet/main-unblock"}
	revisit, err := u.revisitBody(parent("EX-2", "started", 80*time.Hour, 1), revisitName("EX-2"))
	if err != nil {
		t.Fatal(err)
	}
	row := stopRow{ID: 7, Name: "wire-a", Role: "worker", Job: "wire", Parent: "wire-lead", Cwd: "/home/u/wt/r/wire", State: "active"}
	helper, err := u.helperPrompt(row, helperName(row.ID), bashPrompt, "")
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"thread":  thread.prompt(),
		"lead":    rolePrompt(sc, lead, "/home/u/dev/example-dataset", "example-dataset", "/home/u/dev/example-dataset", 4, issue, "QT-10"),
		"worker":  rolePrompt(sc, worker, "/home/u/wt/example-dataset/wire", "example-dataset", "/home/u/dev/example-dataset", 4, atb.Issue{}, ""),
		"revisit": revisit,
		"unblock": helper,
	}
}

// goldenOf names the golden file of each of renders' first messages.
var goldenOf = map[string]string{"thread": "thread-initiative", "lead": "lead-single-issue", "worker": "worker-single",
	"revisit": "revisit", "unblock": "unblock"}

// golden is the golden file of renders' first message `name`.
func golden(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "prompts", goldenOf[name]+".golden"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestEveryBuiltInSlotIsInItsTextsTable(t *testing.T) {
	for _, name := range config.PromptNames {
		spec := specOf(name)
		for _, at := range slotPattern.FindAllStringSubmatch(spec.builtin, -1) {
			if !slices.Contains(spec.slots, at[1]) {
				t.Errorf("the built-in %s uses {{%s}}, which its table lacks", name, at[1])
			}
		}
	}
}

func TestAReplacementMayUseEverySlotOfItsTextAndAllAreFilled(t *testing.T) {
	prompts, files := map[string]map[string]string{}, map[string]string{}
	for _, name := range config.PromptNames {
		var text strings.Builder
		for _, slot := range specOf(name).slots {
			text.WriteString(name + " " + slot + "={{" + slot + "}}\n")
		}
		if text.Len() == 0 {
			text.WriteString("guidance without slots\n")
		}
		prompts[name] = map[string]string{"replace": name + ".md"}
		files[name+".md"] = text.String()
	}
	for name, got := range renders(t, promptScope(t, prompts, "", files)) {
		if strings.Contains(got, "{{") {
			t.Errorf("%s: a slot is left unfilled: %q", name, got)
		}
		if !strings.Contains(got, name+" agent=") {
			t.Errorf("%s: the replacement is not in the first message: %q", name, got)
		}
	}
}

func TestATextIsTheReplacementThenTheAdditionThenTheSafetyDefault(t *testing.T) {
	files := map[string]string{"r.md": "Replaced, {{agent}}.\n", "a.md": "Added, {{job}}.", "base.md": "the old built-in\n"}
	cases := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{"lead", map[string]string{"append": "a.md"}, golden(t, "lead") + "\nAdded, wire.\n"},
		{"lead", map[string]string{"replace": "r.md", "base": "base.md"}, "Replaced, wire-lead.\n\n" + safetyPrompt},
		{"lead", map[string]string{"replace": "r.md", "append": "a.md"}, "Replaced, wire-lead.\n\nAdded, wire.\n\n" + safetyPrompt},
		{"worker", map[string]string{"replace": "r.md"}, "Replaced, wire-a.\n\n" + safetyPrompt},
	}
	for _, c := range cases {
		sc := promptScope(t, map[string]map[string]string{c.name: c.fields}, "", files)
		if got := renders(t, sc)[c.name]; got != c.want {
			t.Errorf("%s %v:\n%q\nwant\n%q", c.name, c.fields, got, c.want)
		}
	}
	got := renders(t, promptScope(t, map[string]map[string]string{"thread": {"replace": "r.md"}}, "", files))["thread"]
	if !strings.HasPrefix(got, "Replaced, thread-c1-1-0.\n\n"+safetyPrompt+"\n## The message\n") {
		t.Errorf("thread: the safety default is not between the replacement and fleet's sections: %q", got)
	}
	// revisit agents and helpers never get it: they run no fleet commands.
	for _, name := range []string{"revisit", "unblock"} {
		got := renders(t, promptScope(t, map[string]map[string]string{name: {"replace": "r.md"}}, "", files))[name]
		if strings.Contains(got, "## Safety") || !strings.HasPrefix(got, "Replaced, ") {
			t.Errorf("%s replaced: %q", name, got)
		}
	}
}

func TestTheGuidanceAdditionComesBeforeTheScreen(t *testing.T) {
	sc := promptScope(t, map[string]map[string]string{"screen_guidance": {"append": "g.md"}}, "",
		map[string]string{"g.md": "- A `go vet` confirmation: allow it.\n"})
	got := renders(t, sc)["unblock"]
	if !strings.Contains(got, screenGuidance+"\n- A `go vet` confirmation: allow it.\n\n## The screen\n") {
		t.Errorf("the addition is not right after the guidance and before the screen: %q", got)
	}
}

func TestAnUnknownSlotIsRefusedAtItsLineAndColumn(t *testing.T) {
	sc := promptScope(t, map[string]map[string]string{"lead": {"append": "lead.append.md"}}, "",
		map[string]string{"lead.append.md": "First line.\nUse {{job}} and {{ x }} and é{{repo}} here.\n"})
	_, err := loadPrompt(sc, "lead")
	var broken *promptError
	if !errors.As(err, &broken) {
		t.Fatalf("not a promptError: %v", err)
	}
	if code := exitCode(err); code != exit.Refused {
		t.Errorf("exit %d, want 1", code)
	}
	want := "prompts.lead.append (" + sc.Prompts["lead"].Append + ":2:30): unknown slot {{repo}}; lead has {{agent}} " +
		"{{agents_md}} {{cap}} {{cwd}} {{issue}} {{issue_note}} {{job}} {{lead}} {{lead_effort}} {{lead_model}} " +
		"{{linear}} {{parent}} {{repo_line}} {{scope}} {{scratch}} {{worker_effort}} {{worker_model}}"
	if err.Error() != want {
		t.Errorf("got  %s\nwant %s", err, want)
	}
	sc = promptScope(t, map[string]map[string]string{"screen_guidance": {"replace": "g.md"}}, "",
		map[string]string{"g.md": "Press {{key}}.\n"})
	if _, err := loadPrompt(sc, "screen_guidance"); err == nil || !strings.HasSuffix(err.Error(), ":1:7): unknown slot {{key}}; screen_guidance has no slots") {
		t.Errorf("guidance with a slot: %v", err)
	}
}

func TestAMissingEmptyOrUnreadableFileIsRefused(t *testing.T) {
	for name, c := range map[string]struct {
		fields map[string]string
		want   string
	}{
		"missing replacement": {map[string]string{"replace": "none.md"}, "prompts.worker.replace (%s/none.md): cannot read the file: no such file or directory"},
		"empty addition":      {map[string]string{"append": "blank.md"}, "prompts.worker.append (%s/blank.md): the file is empty"},
		"missing base":        {map[string]string{"replace": "w.md", "base": "none.md"}, "prompts.worker.base (%s/none.md): cannot read the file"},
		"directory":           {map[string]string{"append": "."}, "prompts.worker.append (%s): cannot read the file"},
	} {
		sc := promptScope(t, map[string]map[string]string{"worker": c.fields}, "", map[string]string{"blank.md": " \n\n", "w.md": "W\n"})
		dir := filepath.Dir(sc.Prompts["worker"].Replace + sc.Prompts["worker"].Append)
		if c.fields["append"] == "." {
			dir = sc.Prompts["worker"].Append
		}
		_, err := loadPrompt(sc, "worker")
		var broken *promptError
		if !errors.As(err, &broken) || !strings.HasPrefix(err.Error(), strings.Replace(c.want, "%s", dir, 1)) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestTheSameMistakeHasOneDigestAndAnotherAnother(t *testing.T) {
	digest := func(text string) string {
		sc := promptScope(t, map[string]map[string]string{"unblock": {"append": "u.md"}}, "", map[string]string{"u.md": text})
		_, err := loadPrompt(sc, "unblock")
		var broken *promptError
		if !errors.As(err, &broken) {
			t.Fatalf("not a promptError: %v", err)
		}
		return broken.digest
	}
	// Each scope has its own directory, so the paths in the messages differ; the
	// same directory is what watch sees run after run.
	sc := promptScope(t, map[string]map[string]string{"unblock": {"append": "u.md"}}, "", map[string]string{"u.md": "{{x}}\n"})
	_, first := loadPrompt(sc, "unblock")
	_, again := loadPrompt(sc, "unblock")
	if first.(*promptError).digest != again.(*promptError).digest {
		t.Error("the same file gave two digests")
	}
	if err := os.WriteFile(sc.Prompts["unblock"].Append, []byte("{{x}}\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, changed := loadPrompt(sc, "unblock"); changed.(*promptError).digest == first.(*promptError).digest {
		t.Error("a changed file kept its digest")
	}
	if digest("{{x}}\n") == digest("{{y}}\n") {
		t.Error("two mistakes share a digest")
	}
}

// The control arms of the golden files: with no prompts or models the
// first messages are the golden files; the same contexts with an addition
// or other models are the golden text plus the addition, or the golden
// text with only the model sentences changed. So the golden files tell a
// start that read the config from one that did not.
func TestAnAdditionOrAModelChangesOnlyWhatItSays(t *testing.T) {
	for name, got := range renders(t, promptScope(t, nil, "", nil)) {
		if got != golden(t, name) {
			t.Errorf("%s without prompts or models is not its golden file", name)
		}
	}
	added := renders(t, promptScope(t, map[string]map[string]string{"worker": {"append": "w.md"}}, "",
		map[string]string{"w.md": "List every file you changed.\n"}))
	if added["worker"] != golden(t, "worker")+"\nList every file you changed.\n" {
		t.Errorf("worker with an addition:\n%q", added["worker"])
	}
	other := renders(t, promptScope(t, nil, `{"lead": {"model": "example-large"}, "worker": {"effort": "low"}}`, nil))
	for name, sentences := range map[string][2]string{
		"lead":   {"Both run `--model opus --effort high`. Research gets one read-only worker (`opus`, `high`)", "Both run `--model opus --effort low`. Research gets one read-only worker (`opus`, `low`)"},
		"thread": {"--model opus --effort high` starts a job: its lead, on `opus` at `high` effort", "--model example-large --effort high` starts a job: its lead, on `example-large` at `high` effort"},
	} {
		if !strings.Contains(golden(t, name), sentences[0]) {
			t.Errorf("%s: the golden file lacks %q", name, sentences[0])
		}
		if want := strings.Replace(golden(t, name), sentences[0], sentences[1], 1); other[name] != want {
			t.Errorf("%s with other models differs elsewhere:\n%q", name, other[name])
		}
	}
	if other["worker"] != golden(t, "worker") {
		t.Error("the models changed the worker prompt, which names none")
	}
}

// exitCode is the exit code `err` carries.
func exitCode(err error) exit.Code {
	var failure *exit.Failure
	if errors.As(err, &failure) {
		return failure.Code
	}
	return exit.Environment
}

package cmd

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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

// helperPrompt is the first message of the `unblock` watch's worker for
// the stopped agent `row`, as startHelper puts it together.
func (w *workers) helperPrompt(row stopRow, screen, issue string) (string, error) {
	role, err := w.workerPrompt()
	if err != nil {
		return "", err
	}
	e := w.builtinWatch(config.BlockedScreens)
	prompt, err := watchPrompt(w.cfg, e)
	if err != nil {
		return "", err
	}
	_, _, body, err := w.screenMessage(e, role, prompt, row, screen, issue)
	return body, err
}

// testWorkers are the watch workers of a run in the scope `main` at a
// fixed clock, with the ledger `conn` and the config `sc`.
func testWorkers(conn *sql.DB, sc *config.Scope) *workers {
	return &workers{watchRun: &watchRun{conn: conn, scope: "main", cfg: sc, now: 1700000000},
		dir: "/home/u/.local/state/fleet/main-watch", legacyDir: "/home/u/.local/state/fleet/main-unblock",
		busy: map[string]bool{}}
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

// renders are the first messages of the three roles' agents and of the
// `unblock` watch's worker under the scope `sc`, from the contexts of the
// golden files `goldenOf` names.
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
	row := stopRow{ID: 7, Name: "wire-a", Role: "worker", Job: "wire", Parent: "wire-lead", Cwd: "/home/u/wt/r/wire", State: "active"}
	helper, err := testWorkers(conn, sc).helperPrompt(row, bashPrompt, "")
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"thread":  thread.prompt(),
		"lead":    rolePrompt(sc, lead, "/home/u/dev/example-dataset", "example-dataset", "/home/u/dev/example-dataset", 4, issue, "QT-10"),
		"worker":  rolePrompt(sc, worker, "/home/u/wt/example-dataset/wire", "example-dataset", "/home/u/dev/example-dataset", 4, atb.Issue{}, ""),
		"unblock": helper,
	}
}

// goldenOf names the golden file of each of renders' first messages.
var goldenOf = map[string]string{"thread": "thread-initiative", "lead": "lead-single-issue", "worker": "worker-single",
	"unblock": "unblock"}

// textOf is the built-in text each of renders' first messages starts
// with: the unblock watch's worker has the worker text.
var textOf = map[string]string{"thread": "thread", "lead": "lead", "worker": "worker", "unblock": "worker"}

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
		if !strings.Contains(got, textOf[name]+" agent=") {
			t.Errorf("%s: the replacement is not in the first message: %q", name, got)
		}
	}
}

func TestATextIsTheReplacementThenTheAddition(t *testing.T) {
	files := map[string]string{"r.md": "Replaced, {{agent}}.\n", "a.md": "Added, {{job}}.", "base.md": "the old built-in\n",
		"c.md": "Shared.\n", "ca.md": "Shared too.\n"}
	cases := []struct {
		prompts map[string]map[string]string
		name    string
		want    string
	}{
		{map[string]map[string]string{"lead": {"append": "a.md"}}, "lead", golden(t, "lead") + "\nAdded, wire.\n"},
		{map[string]map[string]string{"lead": {"replace": "r.md", "base": "base.md"}}, "lead",
			"Replaced, wire-lead.\n\n" + commonPrompt},
		{map[string]map[string]string{"lead": {"replace": "r.md", "append": "a.md"}}, "lead",
			"Replaced, wire-lead.\n\n" + commonPrompt + "\nAdded, wire.\n"},
		{map[string]map[string]string{"worker": {"replace": "r.md"}}, "worker", "Replaced, wire-a.\n\n" + commonPrompt},
		// common's own parts sit between the role's text and its addition.
		{map[string]map[string]string{"common": {"replace": "c.md", "append": "ca.md"}, "worker": {"replace": "r.md", "append": "a.md"}},
			"worker", "Replaced, wire-a.\n\nShared.\n\nShared too.\n\nAdded, wire.\n"},
		{map[string]map[string]string{"common": {"replace": "c.md"}}, "lead",
			strings.Replace(golden(t, "lead"), commonPrompt, "Shared.\n", 1)},
	}
	for _, c := range cases {
		if got := renders(t, promptScope(t, c.prompts, "", files))[c.name]; got != c.want {
			t.Errorf("%s %v:\n%q\nwant\n%q", c.name, c.prompts, got, c.want)
		}
	}
	got := renders(t, promptScope(t, map[string]map[string]string{"thread": {"replace": "r.md"}}, "", files))["thread"]
	if !strings.HasPrefix(got, "Replaced, thread-c1-1-0.\n\n"+commonPrompt+"\n## The message\n") {
		t.Errorf("thread: common and fleet's sections do not follow the replacement: %q", got)
	}
	// The unblock watch's worker is a worker: the worker addition reaches
	// it after common, before fleet's sections.
	got = renders(t, promptScope(t, map[string]map[string]string{"worker": {"append": "a.md"}}, "", files))["unblock"]
	if !strings.Contains(got, commonPrompt+"\nAdded, .\n\n## Your task\n") {
		t.Errorf("unblock: the worker addition is not between common and the task: %q", got)
	}
}

// The unblock watch's prompt file takes the place of the built-in text
// and the guidance together; the worker text stays.
func TestTheUnblockWatchsPromptReplacesItsTextAndTheGuidance(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "u.md"), []byte("Press only `esc`; ask in {{question_file}}.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sc, err := config.ParseScope([]byte(`{"watches": {"unblock": {"prompt": "`+filepath.Join(dir, "u.md")+`"}}}`), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	got := renders(t, sc)["unblock"]
	if strings.Contains(got, "## Guidance") || strings.Contains(got, "Your only task is that screen") ||
		!strings.HasSuffix(got, "```\n\nPress only `esc`; ask in /home/u/.local/state/fleet/main-watch/watch-unblock-7-question.md.\n") ||
		!strings.HasPrefix(got, "You are a worker run by fleet: watch-unblock-7,") {
		t.Errorf("the prompt file did not take the place of the text and the guidance: %q", got)
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
		"{{linear}} {{one_task}} {{parent}} {{repo_line}} {{scope}} {{scratch}} {{worker_effort}} {{worker_model}}"
	if err.Error() != want {
		t.Errorf("got  %s\nwant %s", err, want)
	}
	sc = promptScope(t, map[string]map[string]string{"common": {"replace": "c.md"}}, "",
		map[string]string{"c.md": "Press {{key}}.\n"})
	if _, err := loadPrompt(sc, "common"); err == nil || !strings.HasSuffix(err.Error(), ":1:7): unknown slot {{key}}; common has no slots") {
		t.Errorf("common with a slot: %v", err)
	}
}

// A watch's prompt has {{key}} and {{text}}, and a worker watch's also
// {{question_file}}.
func TestAWatchPromptHasTheSlotsOfItsAct(t *testing.T) {
	file := filepath.Join(t.TempDir(), "p.md")
	if err := os.WriteFile(file, []byte("{{key}} {{text}} {{question_file}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for do, want := range map[string]string{"worker": "", "tell": "unknown slot {{question_file}}; a watch's prompt has {{key}} {{text}}"} {
		_, err := watchPrompt(&config.Scope{}, config.Entry{Name: "w", Do: do, Prompt: file})
		if want == "" && err != nil || want != "" && (err == nil || !strings.HasSuffix(err.Error(), want)) {
			t.Errorf("do %s: %v", do, err)
		}
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
		sc := promptScope(t, map[string]map[string]string{"worker": {"append": "u.md"}}, "", map[string]string{"u.md": text})
		_, err := loadPrompt(sc, "worker")
		var broken *promptError
		if !errors.As(err, &broken) {
			t.Fatalf("not a promptError: %v", err)
		}
		return broken.digest
	}
	// Each scope has its own directory, so the paths in the messages differ; the
	// same directory is what watch sees run after run.
	sc := promptScope(t, map[string]map[string]string{"worker": {"append": "u.md"}}, "", map[string]string{"u.md": "{{x}}\n"})
	_, first := loadPrompt(sc, "worker")
	_, again := loadPrompt(sc, "worker")
	if first.(*promptError).digest != again.(*promptError).digest {
		t.Error("the same file gave two digests")
	}
	if err := os.WriteFile(sc.Prompts["worker"].Append, []byte("{{x}}\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, changed := loadPrompt(sc, "worker"); changed.(*promptError).digest == first.(*promptError).digest {
		t.Error("a changed file kept its digest")
	}
	if digest("{{x}}\n") == digest("{{y}}\n") {
		t.Error("two mistakes share a digest")
	}
}

// The control arms of the golden files: with no prompts or models the
// first messages are the golden files; the same contexts with an addition
// are the golden text plus the addition. So the golden files tell a start
// that read the config from one that did not. The models change no
// built-in text; they fill only the model slots a replacement still uses,
// with the old values where they give none.
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
	models := `{"lead": {"model": "example-large"}, "worker": {"effort": "low"}}`
	for name, got := range renders(t, promptScope(t, nil, models, nil)) {
		if got != golden(t, name) {
			t.Errorf("%s: the models changed the built-in text", name)
		}
	}
	replaced := renders(t, promptScope(t, map[string]map[string]string{"thread": {"replace": "t.md"}}, models,
		map[string]string{"t.md": "{{lead_model}} {{lead_effort}} {{worker_model}} {{worker_effort}}\n"}))
	if want := "example-large high opus low\n\n"; !strings.HasPrefix(replaced["thread"], want) {
		t.Errorf("model slots: %q, want it to start with %q", replaced["thread"], want)
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

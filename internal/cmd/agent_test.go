package cmd

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/exit"
)

const trust = "────────\n Accessing workspace:\n\n /w/x\n\n Quick safety check: Is this a project you created or one you trust?\n\n ❯ No, exit\n   Yes, I trust this folder\n\n Enter to confirm · Esc to cancel\n"

func onYes(screen string) string {
	return strings.ReplaceAll(strings.ReplaceAll(screen, " ❯ No, exit", "   No, exit"), "   Yes, I trust", " ❯ Yes, I trust")
}

func TestTrustDialogIsAnsweredOneKeyAtATimeFromTheCursorForTheAgentsOwnDirectory(t *testing.T) {
	key := func(screen, cwd string) string {
		k, err := TrustKey(screen, cwd)
		if err != nil {
			return "exit " + strconv.Itoa(int(code(err))) + ": " + err.Error()
		}
		return k
	}
	if got := key(trust, "/w/x"); got != "down" {
		t.Errorf("cancel highlighted: %q", got)
	}
	if got := key(onYes(trust), "/w/x"); got != "enter" {
		t.Errorf("yes highlighted: %q", got)
	}
	if got := key(" ❯ 1. Dark mode\n   2. Light mode\n", "/w/x"); got != "" {
		t.Errorf("menu: %q", got)
	}
	// Another directory's dialog is not fleet's to accept: exit 3 naming
	// both, nothing pressed.
	if got := key(trust, "/w/y"); !strings.HasPrefix(got, "exit 3: ") || !strings.Contains(got, "for /w/x, not its own directory /w/y") ||
		!strings.Contains(got, "nothing pressed") {
		t.Errorf("another directory: %q", got)
	}
	// A path wrapped over two lines reads whole; a cursor that cannot be
	// placed is exit 3.
	wrapped := strings.Replace(trust, "\n /w/x\n", "\n /w/x/a-long-\n directory\n", 1)
	if got := key(wrapped, "/w/x/a-long-directory"); got != "down" {
		t.Errorf("wrapped path: %q", got)
	}
	if got := key(strings.ReplaceAll(trust, "❯", " "), "/w/x"); !strings.HasPrefix(got, "exit 3: ") {
		t.Errorf("no cursor: %q", got)
	}
}

// scripted is a scripted herdr for reachInputBox: the screen is the one
// indexed by the number of keys pressed so far (the last one repeating),
// so a key that changes nothing is a lost press; statuses are returned in
// order, the last one repeating; the keys pressed go to `sent`.
func scripted(screens, statuses []string, sent *[]string) error {
	st := 0
	return reachInputBox(
		func() (string, error) { return screens[min(len(*sent), len(screens)-1)], nil },
		func() (string, error) {
			item := statuses[min(st, len(statuses)-1)]
			st++
			return item, nil
		},
		func(key string) error {
			*sent = append(*sent, key)
			return nil
		},
		"/w/x",
		30*time.Millisecond,
		time.Millisecond,
	)
}

func code(err error) exit.Code {
	var failure *exit.Failure
	if errors.As(err, &failure) {
		return failure.Code
	}
	return -1
}

func TestReadyNeedsTheInputBoxAndHerdrOutOfBlocked(t *testing.T) {
	rule := strings.Repeat("─", 40)
	idle := rule + "\n❯ \n" + rule + "\n"
	var sent []string
	// Each key is chosen from the screen read after the one before: down
	// moves the cursor, enter confirms what the cursor is on.
	if err := scripted([]string{trust, onYes(trust), idle}, []string{"blocked", "blocked", "idle"}, &sent); err != nil {
		t.Fatal(err)
	}
	if strings.Join(sent, ",") != "down,enter" {
		t.Errorf("sent %q", sent)
	}
	// A lost press: the cursor has not moved, so down is pressed again,
	// never enter on the cancel option.
	sent = nil
	if err := scripted([]string{trust, trust, onYes(trust), idle}, []string{"blocked", "blocked", "blocked", "idle"}, &sent); err != nil {
		t.Fatal(err)
	}
	if strings.Join(sent, ",") != "down,down,enter" {
		t.Errorf("lost press: sent %q", sent)
	}
	// The cursor never moves: the presses stop at the bound, exit 3.
	sent = nil
	if got := code(scripted([]string{trust}, []string{"blocked"}, &sent)); got != exit.Blocked || len(sent) != maxTrustKeys {
		t.Errorf("stuck cursor: code %d, sent %q", got, sent)
	}
	// A dialog for another directory: exit 3, nothing pressed.
	sent = nil
	other := strings.Replace(trust, " /w/x\n", " /w/other\n", 1)
	if got := code(scripted([]string{other, idle}, []string{"blocked", "idle"}, &sent)); got != exit.Blocked || len(sent) != 0 {
		t.Errorf("other directory: code %d, sent %q", got, sent)
	}
	// The input box is drawn but herdr stays blocked past the deadline.
	sent = nil
	if got := code(scripted([]string{trust, onYes(trust), idle}, []string{"blocked"}, &sent)); got != exit.Blocked {
		t.Errorf("stays blocked: code %d", got)
	}
	// Started straight at the input box: no keys.
	sent = nil
	if err := scripted([]string{idle}, []string{"idle"}, &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 0 {
		t.Errorf("sent %q", sent)
	}
	// herdr says idle, but the screen is a menu.
	sent = nil
	if got := code(scripted([]string{" ❯ 1. Dark mode\n"}, []string{"idle"}, &sent)); got != exit.Blocked {
		t.Errorf("menu: code %d", got)
	}
}

func TestInputBoxIsRecognisedAndMenusAreNot(t *testing.T) {
	rule := strings.Repeat("─", 40)
	idle := " Welcome\n\n" + rule + "\n❯ \n" + rule + "\n  ? for shortcuts\n"
	if !IsInputBox(idle) {
		t.Error("input box not recognised")
	}
	if IsInputBox(trust) {
		t.Error("trust prompt taken for the input box")
	}
	if IsInputBox(" Choose the text style\n\n ❯ 1. Dark mode\n   2. Light mode\n") {
		t.Error("menu taken for the input box")
	}
}

func TestResourcesNeedLoadBelowCPUsAndMoreThan2GiB(t *testing.T) {
	mem := func(kib uint64) string {
		return "MemTotal: 99999999 kB\nMemAvailable: " + strconv.FormatUint(kib, 10) + " kB\n"
	}
	if err := CheckResources("1.50 1.0 1.0 1/100 42\n", 2, mem(3*1024*1024)); err != nil {
		t.Fatal(err)
	}
	refused := func(load string, cpus int, kib uint64) exit.Code {
		return code(CheckResources(load, cpus, mem(kib)))
	}
	if got := refused("2.00 0 0", 2, 3*1024*1024); got != exit.Refused {
		t.Errorf("load at the cpu count: %d", got)
	}
	if got := refused("0.10 0 0", 2, 2*1024*1024); got != exit.Refused {
		t.Errorf("memory at 2 GiB: %d", got)
	}
	if got := refused("", 2, 3*1024*1024); got != exit.Environment {
		t.Errorf("empty loadavg: %d", got)
	}
}

func TestNamesAreChecked(t *testing.T) {
	for _, good := range []string{"item-7", "a", "improve-prompts"} {
		if err := CheckName(good); err != nil {
			t.Errorf("%s: %v", good, err)
		}
	}
	for _, bad := range []string{"", "Item", "a_b", "a b", "cron", "x/y"} {
		if err := CheckName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if err := CheckAgentName(strings.Repeat("a", 32)); err != nil {
		t.Errorf("32 characters: %v", err)
	}
	for _, bad := range []string{"1-lead", "-a-lead", strings.Repeat("a", 33)} {
		if err := CheckAgentName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestClaudeGetsTheFixedArguments(t *testing.T) {
	opus, medium := "opus", "medium"
	want := `--dangerously-skip-permissions --disallowedTools AskUserQuestion --settings {"remoteControlAtStartup":false} --model opus --effort medium`
	if got := strings.Join(ClaudeArgs(&opus, &medium), " "); got != want {
		t.Errorf("got %q", got)
	}
	if got := len(ClaudeArgs(nil, nil)); got != 5 {
		t.Errorf("without model and effort: %d arguments", got)
	}
}

func TestWorkOrderTitleIsTheFirstNonEmptyLine(t *testing.T) {
	for task, want := range map[string]string{
		"\n  \n## Import the A table  \nbody\n": "Import the A table",
		"#\n# \nfix the timeout\n":              "fix the timeout",
		"   plain first line\n":                 "plain first line",
		strings.Repeat("é", 81) + "\n":          strings.Repeat("é", 80),
		"#\n##\n":                               "",
	} {
		if got := WorkOrderTitle(task); got != want {
			t.Errorf("%q: got %q, want %q", task, got, want)
		}
	}
}

func TestTheResourceCheckFollowsTheConfig(t *testing.T) {
	saved := machineResources
	defer func() { machineResources = saved }()
	machineResources = func() error { return exit.Refusedf("the machine is busy") }
	for _, on := range []bool{true, false} {
		cfg := &config.Config{MaxAgentsPerJob: 4, ResourceCheck: on}
		if refused := resources(cfg) != nil; refused != on {
			t.Errorf("resource_check %v: refused %v", on, refused)
		}
	}
}

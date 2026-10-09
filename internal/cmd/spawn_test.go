package cmd

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Luolc/agent-fleet/internal/exit"
)

const trust = "────────\n Accessing workspace:\n\n /w/x\n\n Quick safety check: Is this a project you created or one you trust?\n\n ❯ No, exit\n   Yes, I trust this folder\n\n Enter to confirm · Esc to cancel\n"

func TestTrustPromptIsAnsweredFromTheHighlight(t *testing.T) {
	if got := strings.Join(TrustKeys(trust), " "); got != "down enter" {
		t.Errorf("cancel highlighted: %q", got)
	}
	onYes := strings.ReplaceAll(strings.ReplaceAll(trust, " ❯ No, exit", "   No, exit"), "   Yes, I trust", " ❯ Yes, I trust")
	if got := strings.Join(TrustKeys(onYes), " "); got != "enter" {
		t.Errorf("yes highlighted: %q", got)
	}
	if got := TrustKeys(" ❯ 1. Dark mode\n   2. Light mode\n"); got != nil {
		t.Errorf("menu: %q", got)
	}
}

// scripted is a scripted herdr for reachInputBox: screens and statuses are
// returned in order, the last one repeating.
func scripted(screens, statuses []string, sent *[]string) error {
	next := func(items []string, i *int) (string, error) {
		item := items[min(*i, len(items)-1)]
		*i++
		return item, nil
	}
	s, st := 0, 0
	return reachInputBox(
		func() (string, error) { return next(screens, &s) },
		func() (string, error) { return next(statuses, &st) },
		func(keys []string) error {
			*sent = append(*sent, strings.Join(keys, " "))
			return nil
		},
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
	if err := scripted([]string{trust, idle}, []string{"blocked", "blocked", "idle"}, &sent); err != nil {
		t.Fatal(err)
	}
	if strings.Join(sent, ",") != "down enter" {
		t.Errorf("sent %q", sent)
	}
	// The input box is drawn but herdr stays blocked past the deadline.
	sent = nil
	if got := code(scripted([]string{trust, idle}, []string{"blocked"}, &sent)); got != exit.Blocked {
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

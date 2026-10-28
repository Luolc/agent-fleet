package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Luolc/agent-fleet/internal/herdr"
)

// The Pi screens below follow what Pi 1.2.0 draws in a 100-column
// terminal: the transcript, the editor between two borders (its top
// border carries the working spinner), and the footer.
var piRule = strings.Repeat("─", 100)

const piFooter = "~/wt/example/item-1\n0.0%/128k (auto)                                       fake-model • high\n"

func piScreen(transcript, topBorder, editor string) string {
	return transcript + "\n\n" + topBorder + "\n" + editor + "\n" + piRule + "\n" + piFooter
}

var (
	piIdle    = piScreen(" Pi can explain its own features and look up its docs.", piRule, "")
	piTyped   = piScreen(" Pi can explain its own features.", piRule, "hello there")
	piWorking = piScreen("\n hello there", "── ⠼ Working "+strings.Repeat("─", 87), "")
	piPasted  = piScreen("\n hello there", "── ⠏ Working "+strings.Repeat("─", 87), "[FROM: item-1-lead]\nline two\nline three")
	// piTrust is the project-trust question, drawn before the editor.
	piTrust = piRule + "\n\n Trust project folder?\n /w/x\n\n This allows pi to load .pi settings and resources, " +
		"install missing project packages, and execute\n project extensions.\n\n → Trust\n   Trust parent folder (/w)\n" +
		"   Trust (this session only)\n   Do not trust\n   Do not trust (this session only)\n\n" +
		" ↑↓ navigate  enter select  escape/ctrl+c cancel\n\n" + piRule + "\n"
	// piConfirm is an extension's confirmation in the editor's place.
	piConfirm = " hello there\n\n" + piRule + "\n\n Delete build?\n Remove ./build for good\n\n → Yes\n   No\n\n" +
		" ↑↓ navigate  enter select  esc cancel\n\n" + piRule + "\n" + piFooter
	// piLoader is a bordered loader in the editor's place.
	piLoader = " hello there\n\n" + piRule + "\n\n ⠋ Logging in...\n\n esc cancel\n\n" + piRule + "\n" + piFooter
)

func TestPiGetsItsModelThinkingAndApprove(t *testing.T) {
	model, high := "example/fake-model", "high"
	if got := strings.Join(PiArgs(&model, &high), " "); got != "--model example/fake-model --thinking high --approve" {
		t.Errorf("got %q", got)
	}
	if got := strings.Join(PiArgs(&model, nil), " "); got != "--model example/fake-model --approve" {
		t.Errorf("without effort: %q", got)
	}
	if got := strings.Join(kindOf(KindPi).args(&model, &high), " "); got != "--model example/fake-model --thinking high --approve" {
		t.Errorf("the pi kind: %q", got)
	}
	// Any kind fleet does not start reads as Claude Code, as before.
	if got := strings.Join(kindOf("").args(nil, nil), " "); got != strings.Join(ClaudeArgs(nil, nil), " ") {
		t.Errorf("no kind: %q", got)
	}
	if CheckKind(KindPi) != nil || CheckKind(KindClaude) != nil || code(CheckKind("gemini")) != 1 {
		t.Error("CheckKind")
	}
}

func TestPiEditorIsRecognisedAndItsDialogsAreNot(t *testing.T) {
	for name, screen := range map[string]string{"idle": piIdle, "typed": piTyped, "working": piWorking, "pasted": piPasted} {
		if !IsPiInputBox(screen) {
			t.Errorf("%s: editor not recognised:\n%s", name, screen)
		}
	}
	for name, screen := range map[string]string{"trust": piTrust, "confirm": piConfirm, "loader": piLoader,
		"blank": "", "transcript only": " hello\n there\n"} {
		if IsPiInputBox(screen) {
			t.Errorf("%s: taken for the editor:\n%s", name, screen)
		}
	}
	// Claude Code's input box is not Pi's question to answer: Claude
	// screens go through IsInputBox, which does not take Pi's editor.
	if IsInputBox(piIdle) {
		t.Error("Pi's editor taken for Claude Code's input box")
	}
}

// piScripted is reachInputBox for Pi on `screens`, one per read (the last
// one repeating), with herdr idle; the keys pressed go to `sent`.
func piScripted(screens []string, sent *[]string) error {
	reads := 0
	return reachInputBox(
		func() (string, error) {
			s := screens[min(reads, len(screens)-1)]
			reads++
			return s, nil
		},
		func() (string, error) { return "idle", nil },
		func(key string) error {
			*sent = append(*sent, key)
			return nil
		},
		"/w/x",
		kinds[KindPi],
		30*time.Millisecond,
		time.Millisecond,
	)
}

func TestPiStartWaitsForItsEditorAndPressesNoKey(t *testing.T) {
	var sent []string
	// herdr may report Pi idle before its first frame.
	if err := piScripted([]string{"", "", piIdle}, &sent); err != nil || len(sent) != 0 {
		t.Errorf("first frame late: %v, keys %q", err, sent)
	}
	// A dialog in the editor's place, Pi's own trust question or Claude
	// Code's (which fleet answers only for Claude), is exit 3 with the
	// screen and no key.
	for name, screen := range map[string]string{"pi trust": piTrust, "claude trust": trust, "confirm": piConfirm} {
		sent = nil
		err := piScripted([]string{screen}, &sent)
		if code(err) != 3 || len(sent) != 0 || !strings.Contains(err.Error(), "is not ready at its input box") {
			t.Errorf("%s: %v, keys %q", name, err, sent)
		}
	}
}

func TestPiFilterKeepsTheTranscriptAndDropsTheEditorAndFooter(t *testing.T) {
	stuck := piScreen(" hello there\n\n Running the migration", "── ⠼ Working "+strings.Repeat("─", 87), "")
	later := strings.ReplaceAll(strings.ReplaceAll(stuck, "⠼", "⠧"), "0.0%/128k", "2.2%/128k")
	if got := FilterPiScreen(stuck); got != " hello there\n\n Running the migration" {
		t.Errorf("filtered %q", got)
	}
	if Hash(kindOf(KindPi).filter(stuck)) != Hash(kindOf(KindPi).filter(later)) {
		t.Errorf("only the spinner and the footer moved, yet the hashes differ")
	}
	moved := strings.Replace(later, "Running the migration", "Ran the migration", 1)
	if Hash(FilterPiScreen(stuck)) == Hash(FilterPiScreen(moved)) {
		t.Errorf("the transcript moved, yet the hashes are the same")
	}
	// A dialog stays on the screen that is hashed and shown to the lead.
	if got := FilterPiScreen(piConfirm); !strings.Contains(got, "Delete build?") {
		t.Errorf("dialog filtered out: %q", got)
	}
}

// A starting Pi row past its grace that shows its editor is not stopped;
// one at a dialog is.
func TestUnblockReadsAStartingPiRowsScreenAsPi(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = --session ]; then shift 2; fi\ncat \"" + dir + "/screen-$3\"\n"
	if err := os.WriteFile(filepath.Join(dir, "herdr"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	for name, screen := range map[string]string{"p-idle": piIdle, "p-trust": piTrust} {
		if err := os.WriteFile(filepath.Join(dir, "screen-"+name), []byte(screen), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	u := &workers{watchRun: &watchRun{h: herdr.New(nil), now: 1700000000}, inHerdr: map[string]InHerdr{
		"p-idle": {Status: "idle", Kind: KindPi}, "p-trust": {Status: "idle", Kind: KindPi}}}
	stopped := func(name string) bool {
		_, ok, err := u.stopped(stopRow{Name: name, State: "starting", StartedAt: u.now - startGraceSecs})
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if stopped("p-idle") {
		t.Error("a Pi row at its editor taken for stopped")
	}
	if !stopped("p-trust") {
		t.Error("a Pi row at a dialog not taken for stopped")
	}
}

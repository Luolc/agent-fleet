package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
)

const bashPrompt = "⏺ fake reply\n" + rule + "\n Bash command\n\n   rm -rf build\n\n Do you want to proceed?\n ❯ 1. Yes\n   2. No\n\n Esc to cancel\n"

const limitMenu = "⏺ fake reply\n" + rule + "\n You have hit your usage limit · resets 3pm (UTC)\n\n ❯ 1. Stop and wait for limit to reset\n   2. Request extra usage\n\n Enter to confirm · Esc to cancel\n"

func TestTheRulesKnowTheOwnTrustDialogAndTheScreensNeverPressed(t *testing.T) {
	for name, c := range map[string]struct {
		screen, cwd string
		want        screenKind
	}{
		"trust, own directory":     {trust, "/w/x", ownTrustScreen},
		"trust, cursor on yes":     {onYes(trust), "/w/x", ownTrustScreen},
		"trust, another directory": {trust, "/w/y", unknownScreen},
		"command confirmation":     {bashPrompt, "/w/x", unknownScreen},
		"usage limit":              {limitMenu, "/w/x", handsOffScreen},
		"model switch":             {rule + "\n The model is overloaded\n ❯ 1. Switch to a smaller model\n   2. Keep current model\n", "/w/x", handsOffScreen},
		// The transcript above the dialog does not decide.
		"limit in the transcript": {"⏺ we hit a rate limit yesterday\n" + bashPrompt, "/w/x", unknownScreen},
	} {
		if got := classifyScreen(c.screen, c.cwd); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

func TestTheHelperGetsTheScreenTheGuidanceAndThePeoplesAnswer(t *testing.T) {
	conn, err := db.OpenAt(filepath.Join(t.TempDir(), "screens.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sc, err := config.ParseScope([]byte(`{}`), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	u := &unblocking{watchRun: &watchRun{conn: conn, scope: "screens", cfg: sc}, dir: "/home/u/.local/state/fleet/screens-unblock"}
	row := stopRow{ID: 7, Name: "s-perm", Role: "worker", Job: "s", Parent: "s-lead", Cwd: "/home/u/wt/r/s", State: "active"}
	got, err := u.helperPrompt(row, helperName(row.ID), bashPrompt, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"You are unblock-7, a screen helper", "in ~/.local/state/fleet/screens-unblock,",
		"The agent s-perm (a worker of job s, lead s-lead, working directory /home/u/wt/r/s)",
		"`herdr agent send-keys s-perm <key>`", "/home/u/.local/state/fleet/screens-unblock/unblock-7-question.md",
		"Ticket: none: tickets are off", "## Guidance", "never press anything; ask", "## The screen", "   rm -rf build"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q not in the prompt", want)
		}
	}
	if strings.Contains(got, "{{") || strings.Contains(got, "\n## Earlier on this screen\n") {
		t.Errorf("a placeholder, or an earlier section without a question: %q", got)
	}
	if _, err := conn.Exec(`INSERT INTO questions (job, thread, asked_by, text, state, asked_at, answered_at)
		VALUES ('s', 'C1/1.0', 'unblock-7', 'Press 1 to run rm -rf build?', 'answered', 1700000000, 1700000100)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO threads (thread, slug, created_at, last_text, last_user, last_ts)
		VALUES ('C1/1.0', 'c1-1-0', 1700000000, 'yes, press 1', 'U0ABC', '1700000100.000')`); err != nil {
		t.Fatal(err)
	}
	got, err = u.helperPrompt(row, helperName(row.ID), bashPrompt, "SC-9")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Ticket: SC-9, labelled blocked-screen; record on it with `atb linear comment SC-9",
		"Act on it only when it plainly answers this screen's question", "Otherwise press nothing and write the question again",
		"## Earlier on this screen", "Asked at 2023-11-14T22:13:20Z (answered):\n\n> Press 1 to run rm -rf build?",
		`{"user":"U0ABC","ts":"1700000100.000","text":"yes, press 1"}`} {
		if !strings.Contains(got, want) {
			t.Errorf("%q not in the prompt with an answer", want)
		}
	}
}

func TestATrustDialogTheRuleDoesNotGetPastStartsNoHelper(t *testing.T) {
	conn, err := db.OpenAt(filepath.Join(t.TempDir(), "screens.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sc, err := config.ParseScope([]byte(`{}`), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	pressed := 0
	settleStopped = func(_ *herdr.Herdr, name, cwd string) error {
		pressed++
		return exit.New(exit.Blocked, name+" is not ready at its input box (herdr status \"blocked\")")
	}
	t.Cleanup(func() { settleStopped = settle })
	u := &unblocking{watchRun: &watchRun{conn: conn, scope: "screens", cfg: sc}, dir: t.TempDir()}
	row := stopRow{ID: 7, Name: "s-trust", Role: "worker", Job: "s", Parent: "s-lead", Cwd: "/w/x", State: "active"}
	err = u.handle(row, trust)
	if pressed != 1 || code(err) != exit.Blocked || !strings.Contains(err.Error(), "did not get it to its input box") {
		t.Fatalf("pressed %d, %v", pressed, err)
	}
	// Nothing else: no helper row, no question, no step; the next run
	// looks at the screen again.
	var rows int
	if err := conn.QueryRow("SELECT (SELECT count(*) FROM agents) + (SELECT count(*) FROM questions) + " +
		"(SELECT count(*) FROM steps)").Scan(&rows); err != nil || rows != 0 {
		t.Errorf("%d rows, %v", rows, err)
	}
}

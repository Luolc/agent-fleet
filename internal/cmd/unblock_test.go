package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

func TestTheUnblockWorkerGetsTheScreenTheGuidanceAndThePeoplesAnswer(t *testing.T) {
	conn, err := db.OpenAt(filepath.Join(t.TempDir(), "screens.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sc, err := config.ParseScope([]byte(`{}`), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	u := testWorkers(conn, sc)
	row := stopRow{ID: 7, Name: "s-perm", Role: "worker", Job: "s", Parent: "s-lead", Cwd: "/home/u/wt/r/s", State: "active"}
	got, err := u.helperPrompt(row, bashPrompt, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"You are a worker run by fleet: watch-unblock-7, one task for fleet's `watch` in scope main",
		"You run in ~/.local/state/fleet/main-watch.", "FLEET_PARENT=watch,",
		"The agent s-perm (a worker of job s, lead s-lead, working directory /home/u/wt/r/s)",
		"`herdr agent send-keys s-perm <key>`", "/home/u/.local/state/fleet/main-watch/watch-unblock-7-question.md",
		"Ticket: none: tickets are off", "## Guidance", "never press anything; ask", "## Your task", "   rm -rf build"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q not in the prompt", want)
		}
	}
	if strings.Contains(got, "{{") || strings.Contains(got, "\n## Earlier\n") {
		t.Errorf("a placeholder, or an earlier section without a question: %q", got)
	}
	// The helper of before the upgrade asked under its old name; the new
	// worker gets that question too.
	if _, err := conn.Exec(`INSERT INTO questions (job, thread, asked_by, text, state, asked_at, answered_at)
		VALUES ('s', 'C1/1.0', 'unblock-7', 'Press 1 to run rm -rf build?', 'answered', 1700000000, 1700000100)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO threads (thread, slug, created_at, last_text, last_user, last_ts)
		VALUES ('C1/1.0', 'c1-1-0', 1700000000, 'yes, press 1', 'U0ABC', '1700000100.000')`); err != nil {
		t.Fatal(err)
	}
	got, err = u.helperPrompt(row, bashPrompt, "SC-9")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Ticket: SC-9, labelled blocked-screen; record on it with `atb linear comment SC-9",
		"act on it only when it plainly answers this screen's question", "otherwise press nothing and write the question again",
		"## Earlier", "Asked at 2023-11-14T22:13:20Z (answered):\n\n> Press 1 to run rm -rf build?",
		`{"user":"U0ABC","ts":"1700000100.000","text":"yes, press 1"}`} {
		if !strings.Contains(got, want) {
			t.Errorf("%q not in the prompt with an answer", want)
		}
	}
	// A question asked on a card comes with the last press on it, and
	// with no message from the thread: a message answers no card.
	if _, err := conn.Exec(`DELETE FROM questions`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO questions (job, thread, asked_by, text, state, asked_at, answered_at, card, card_id, answer)
		VALUES ('s', 'C1/1.0', 'watch-unblock-7', '{"question": "Press 1?"}', 'answered', 1700000000, 1700000100, 1, 'card-1',
		'{"question_id":1,"card_id":"card-1","outcome":"answered","choice":{"index":1,"short":"No"},"note":"press 2","at":"x"}')`); err != nil {
		t.Fatal(err)
	}
	got, err = u.helperPrompt(row, bashPrompt, "SC-9")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Earlier", "Asked at 2023-11-14T22:13:20Z (answered):\n\n> {\"question\": \"Press 1?\"}",
		"The last press on its card:\n\n{\"question_id\":1,\"card_id\":\"card-1\",\"outcome\":\"answered\",\"choice\":{\"index\":1,\"short\":\"No\"},\"note\":\"press 2\""} {
		if !strings.Contains(got, want) {
			t.Errorf("%q not in the prompt with a press", want)
		}
	}
	if strings.Contains(got, "yes, press 1") {
		t.Errorf("the thread's message is in the prompt of a card's press")
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
	t.Cleanup(func() { settleStopped = settleClaude })
	u := testWorkers(conn, sc)
	u.dir = t.TempDir()
	row := stopRow{ID: 7, Name: "s-trust", Role: "worker", Job: "s", Parent: "s-lead", Cwd: "/w/x", State: "active"}
	err = u.handle(u.builtinWatch(config.BlockedScreens), true, row, trust)
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

// The lead of a job started without FLEET_ROLE has no home thread; when
// the people must be asked about its screen, one is opened in its repo's
// channel, as `ask-human` opens it, and the question goes there.
func TestAScreenOfALeadWithoutAHomeThreadOpensOne(t *testing.T) {
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	fake := "#!/bin/sh\necho \"$*\" >> " + calls + "\n[ \"$2\" = open-thread ] && { echo \"$6/1700000099.000\"; exit 0; }\n" +
		"echo '{\"id\":\"card-1\",\"channel\":\"C0123\",\"ts\":\"1.0\",\"permalink\":\"https://slack.example.test/card-1\"}'\n"
	if err := os.WriteFile(filepath.Join(bin, "fednet"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	conn, err := db.OpenAt(filepath.Join(t.TempDir(), "screens.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sc, err := config.ParseScope([]byte(`{"fednet": {"socket": "/run/fednet.sock"}}`), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"INSERT INTO jobs (job, repo, lead_cwd, state, started_at) VALUES ('r', 'example-dataset', '/home/u/dev/example-dataset', 'open', 0)",
		"INSERT INTO threads (thread, slug, channel, mapping, cwd, created_at) VALUES ('C0123/1.0', 'c0123-1-0', 'C0123', 'repo-example-dataset', '', 0)",
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	u := testWorkers(conn, sc)
	row := stopRow{ID: 7, Name: "r-lead", Role: "lead", Job: "r", Parent: plainStarter, State: "active"}
	if err := u.ask(row, "watch-unblock-7", `{"question": "r-lead is at a usage limit; wait for the reset?"}`); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(calls)
	if !strings.HasPrefix(string(got), "client open-thread -socket /run/fednet.sock -channel C0123 -- Thread of job r in example-dataset") ||
		!strings.Contains(string(got), "client ask -socket /run/fednet.sock -thread C0123/1700000099.000 -ref main:1 -card ") {
		t.Errorf("fednet calls = %q", got)
	}
	var home, asked string
	if err := conn.QueryRow("SELECT j.home_thread, q.thread FROM jobs j, questions q WHERE j.job = 'r'").Scan(&home, &asked); err != nil ||
		home != "C0123/1700000099.000" || asked != home {
		t.Errorf("home thread %q, question in %q, %v", home, asked, err)
	}
}

// A screen only a person deals with is asked about on a card; for an
// agent of a job the people may also have the job ended.
func TestAHandsOffScreenIsAskedOnACardWithTheJobsEnd(t *testing.T) {
	sc, err := config.ParseScope([]byte(`{}`), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	u := testWorkers(nil, sc)
	lead := stopRow{ID: 7, Name: "r-lead", Role: "lead", Job: "r", State: "active"}
	got, err := u.screenCard(lead, "r-lead is stopped at a screen fleet never answers", "r-lead (the lead of job r) is "+
		"stopped at a usage limit.", limitMenu)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"question":"r-lead is stopped at a screen fleet never answers","context":"r-lead (the lead of job r) is stopped ` +
		`at a usage limit. A person has to deal with it at the machine (agent r-lead in the herdr session fleet-main).\n\n` +
		"The screen:\\n\\n```\\n"
	if !strings.HasPrefix(got, want) || !strings.HasSuffix(got, `"options":[{"short":"I'll deal with it","long":"A person `+
		`deals with the screen at the machine; fleet does not ask about this stop again.","recommended":true},{"short":"End job r",`+
		"\"long\":\"The thread's agent ends job r by force (`fleet job end r --force`), its work as it stands.\"}]}") {
		t.Errorf("card %s", got)
	}
	thread := stopRow{ID: 8, Name: "thread-c1", Role: "thread", Thread: "C1/1.0", State: "active"}
	if got, err := u.screenCard(thread, "t", "w", limitMenu); err != nil || strings.Contains(got, `"options"`) {
		t.Errorf("a thread agent's card %s, %v", got, err)
	}
}

// A card whose post failed is posted on the next try under the same ref,
// and nothing waits on it meanwhile: the screen is asked about again.
func TestAFailedAskIsPostedNextRunUnderTheSameRef(t *testing.T) {
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	fake := "#!/bin/sh\necho \"$*\" >> " + calls + "\n[ -f " + filepath.Join(bin, "down") + " ] && { echo 'hub unreachable' >&2; exit 5; }\n" +
		"echo '{\"id\":\"card-1\",\"channel\":\"C1\",\"ts\":\"1.0\",\"permalink\":\"https://slack.example.test/card-1\"}'\n"
	if err := os.WriteFile(filepath.Join(bin, "fednet"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "down"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	conn, err := db.OpenAt(filepath.Join(t.TempDir(), "screens.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sc, err := config.ParseScope([]byte(`{"fednet": {"socket": "/run/fednet.sock"}}`), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	u := testWorkers(conn, sc)
	row := stopRow{ID: 7, Name: "thread-c1", Role: "thread", Thread: "C1/1.0", State: "active"}
	e := u.builtinWatch(config.BlockedScreens)
	if err := u.handle(e, false, row, limitMenu); err == nil || !strings.Contains(err.Error(), "exit status 5") {
		t.Fatalf("first run: %v", err)
	}
	if err := os.Remove(filepath.Join(bin, "down")); err != nil {
		t.Fatal(err)
	}
	if err := u.handle(e, false, row, limitMenu); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(calls)
	if strings.Count(string(got), "client ask -socket /run/fednet.sock -thread C1/1.0 -ref main:1 ") != 2 {
		t.Errorf("fednet calls = %q", got)
	}
	var rows, id int
	var state, cardID string
	if err := conn.QueryRow("SELECT count(*), max(id), state, card_id FROM questions").Scan(&rows, &id, &state, &cardID); err != nil ||
		rows != 1 || id != 1 || state != "pending" || cardID != "card-1" {
		t.Errorf("%d question(s), #%d %s %q, %v", rows, id, state, cardID, err)
	}
}

// fleet's screen lines are left out, earliest first, until the context
// fits fednet's limit, and the card says how many.
func TestAWideScreenIsCutToFitTheCard(t *testing.T) {
	sc, err := config.ParseScope([]byte(`{}`), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	u := testWorkers(nil, sc)
	var screen strings.Builder
	for i := range 15 {
		fmt.Fprintf(&screen, "%02d %s\n", i, strings.Repeat("x", 300))
	}
	row := stopRow{ID: 7, Name: "r-lead", Role: "lead", Job: "r", State: "active"}
	got, err := u.screenCard(row, "t", "why.", screen.String())
	if err != nil {
		t.Fatal(err)
	}
	var card struct{ Context string }
	if err := json.Unmarshal([]byte(got), &card); err != nil {
		t.Fatal(err)
	}
	if n := utf8.RuneCountInString(card.Context); n > maxContext || !strings.Contains(card.Context, "The screen (its first 6 "+
		"line(s) left out):") || strings.Contains(card.Context, "05 x") || !strings.Contains(card.Context, "06 x") {
		t.Errorf("%d runes: %s", n, card.Context)
	}
}

// The line fleet puts before a worker's question never takes a text that
// fits over fednet's limit; a text over it alone is left for fednet.
func TestTheHeaderNeverTakesAWorkersTextOverTheLimit(t *testing.T) {
	fits := strings.Repeat("y", maxContext-10)
	if got := withHeader("watch-x asks this.", fits); got != fits {
		t.Errorf("header kept over the limit: %d runes", utf8.RuneCountInString(got))
	}
	short := "Press 1?"
	if got := withHeader("watch-x asks this.", short); got != "watch-x asks this.\n\nPress 1?" {
		t.Errorf("%q", got)
	}
	over := strings.Repeat("y", maxContext+1)
	if got := withHeader("h", over); got != "h\n\n"+over {
		t.Errorf("a text over the limit was changed")
	}
}

// Once the agent is no longer stopped, its screen's questions are closed
// and their posted cards taken back, snoozed or not, even where fednet
// fails; nothing then waits on them.
func TestARecoveredAgentsScreenCardsAreTakenBack(t *testing.T) {
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	fake := "#!/bin/sh\necho \"$*\" >> " + calls + "\ncase \"$*\" in *card-2) echo 'no such card' >&2; exit 1;; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "fednet"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	conn, err := db.OpenAt(filepath.Join(t.TempDir(), "screens.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sc, err := config.ParseScope([]byte(`{"fednet": {"socket": "/run/fednet.sock"}}`), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"INSERT INTO jobs (job, repo, lead_cwd, state, started_at, home_thread) VALUES ('r', 'example-dataset', '/home/u/dev/example-dataset', 'open', 0, 'C1/1.0')",
		"INSERT INTO agents (name, role, job, parent, state, started_at) VALUES ('r-lead', 'lead', 'r', 'thread-c1', 'active', 0)",
		"INSERT INTO questions (job, thread, asked_by, text, state, asked_at, card, card_id) VALUES " +
			"('r', 'C1/1.0', 'watch-unblock-1', '{\"question\": \"limit\"}', 'pending', 0, 1, 'card-1'), " +
			"('r', 'C1/1.0', 'watch-unblock-1', '{\"question\": \"again\"}', 'pending', 0, 1, 'card-2'), " +
			"('r', 'C1/1.0', 'watch-unblock-1', '{\"question\": \"unposted\"}', 'pending', 0, 1, ''), " +
			"('', 'C1/1.0', 'thread-c1', '{\"question\": \"its own\"}', 'pending', 0, 1, 'card-4')",
		"UPDATE questions SET snooze_until = 1800000000 WHERE id = 2",
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	u := testWorkers(conn, sc)
	u.start = time.Now()
	// While it is stopped, nothing is taken back.
	if err := u.closeRecovered(map[string]bool{"watch-unblock-1": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatalf("fednet called while the agent is stopped: %v", err)
	}
	if err := u.unblock(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(calls)
	note := "-note " + recoveredNote + " -- "
	if want := "client ask-close -socket /run/fednet.sock " + note + "card-1\nclient ask-close -socket /run/fednet.sock " +
		note + "card-2\n"; string(got) != want {
		t.Errorf("fednet calls = %q", got)
	}
	var states string
	if err := conn.QueryRow("SELECT group_concat(state, ' ') FROM (SELECT state FROM questions ORDER BY id)").Scan(&states); err != nil ||
		states != "closed closed closed pending" {
		t.Errorf("states %q, %v", states, err)
	}
	if _, err := conn.Exec("UPDATE questions SET state = 'closed' WHERE id = 4"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec("UPDATE jobs SET state = 'ended'"); err != nil {
		t.Fatal(err)
	}
	if err := stillWaiting(conn, "C1/1.0"); err != nil {
		t.Errorf("thread end is held: %v", err)
	}
	threads := map[string]*watchedThread{}
	if err := readQuestions(conn, func(key string) *watchedThread {
		if threads[key] == nil {
			threads[key] = &watchedThread{}
		}
		return threads[key]
	}); err != nil || len(threads) != 0 {
		t.Errorf("questions the reminders read: %v, %v", threads, err)
	}
}

// An agent that stops again after it recovered is asked about again,
// whether or not the people answered the card of its first stop.
func TestEachStopOfAnAgentIsAskedAbout(t *testing.T) {
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	fake := "#!/bin/sh\necho \"$*\" >> " + calls + "\n" +
		"echo '{\"id\":\"card-1\",\"channel\":\"C1\",\"ts\":\"1.0\",\"permalink\":\"https://slack.example.test/card-1\"}'\n"
	if err := os.WriteFile(filepath.Join(bin, "fednet"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	conn, err := db.OpenAt(filepath.Join(t.TempDir(), "screens.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sc, err := config.ParseScope([]byte(`{"fednet": {"socket": "/run/fednet.sock"}}`), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	u := testWorkers(conn, sc)
	row := stopRow{ID: 7, Name: "thread-c1", Role: "thread", Thread: "C1/1.0", State: "active"}
	e := u.builtinWatch(config.BlockedScreens)
	stillStopped := map[string]bool{"watch-unblock-7": true}
	asks := func() int {
		got, _ := os.ReadFile(calls)
		return strings.Count(string(got), "client ask -socket")
	}
	for i, answered := range []bool{false, true} {
		if err := u.handle(e, false, row, limitMenu); err != nil {
			t.Fatal(err)
		}
		if answered {
			if _, err := conn.Exec("UPDATE questions SET state = 'answered' WHERE state = 'pending'"); err != nil {
				t.Fatal(err)
			}
		}
		// While it stays stopped, it is not asked about again.
		if err := u.closeRecovered(stillStopped); err != nil {
			t.Fatal(err)
		}
		if err := u.handle(e, false, row, limitMenu); err != nil {
			t.Fatal(err)
		}
		if got := asks(); got != i+1 {
			t.Fatalf("stop %d (answered %v): %d ask(s) while stopped", i+1, answered, got)
		}
		if err := u.closeRecovered(map[string]bool{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := u.handle(e, false, row, limitMenu); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := conn.QueryRow("SELECT count(*) FROM questions WHERE state = 'pending'").Scan(&pending); err != nil ||
		asks() != 3 || pending != 1 {
		t.Errorf("third stop: %d ask(s), %d pending, %v; want 3 and 1", asks(), pending, err)
	}
}

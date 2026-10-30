package cmd

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
)

// claudeImageParts are the parts of a paste that Claude Code 2.1.296 reads
// as paths of images to attach: the text split at each space before `/`
// and at newlines, a part (trimmed) ending in an image's extension.
func claudeImageParts(text string) []string {
	ext := regexp.MustCompile(`(?i)\.(png|jpe?g|gif|webp)$`)
	var found []string
	for _, part := range strings.Split(strings.ReplaceAll(text, " /", "\n/"), "\n") {
		if ext.MatchString(strings.TrimSpace(part)) {
			found = append(found, part)
		}
	}
	return found
}

// What fleet types must have no part Claude Code reads as an image to attach:
// it would drop the Enter herdr presses while it reads the image, and the
// message would wait in the input box (2026-10-30). Both the path fleet
// writes for a file and the `file: <name> <permalink>` line fednet adds to
// the text end in the file's name.
func TestADeliveredMessageHasNoPartClaudeCodeTakesForAnImage(t *testing.T) {
	e := eventOf(t, filesEvent)
	if parts := claudeImageParts(e.Payload.Text); len(parts) == 0 {
		t.Fatal("the fixture's text has no part an image would be read from")
	}
	text, err := WithHeader(inboxSender, inboxMessage(e, "", "/s").body())
	if err != nil {
		t.Fatal(err)
	}
	if parts := claudeImageParts(text); len(parts) > 0 {
		t.Errorf("parts read as images: %q", parts)
	}
	for _, want := range []string{"path `/var/cache/fednet/F0IMG1-shot.png`",
		"\nfile: shot.png `https://ws.example.test/files/U0ABC/F0IMG1/shot.png`\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("%q is not in %q", want, text)
		}
	}
}

// Only fednet's line for one of the message's files is changed; a person's
// own words are left as they are.
func TestOnlyFednetsFileLinesAreQuoted(t *testing.T) {
	msg := inboundMessage{Files: []inboundFile{{Name: "a.png", URL: "https://ws.example.test/files/U0ABC/F0A/a.png"}}}
	in := "file: a.png https://ws.example.test/files/U0ABC/F0A/a.png\nsee /tmp/b.png\nfile: c.png https://ws.example.test/c.png"
	want := "file: a.png `https://ws.example.test/files/U0ABC/F0A/a.png`\nsee /tmp/b.png\nfile: c.png https://ws.example.test/c.png"
	if got := msg.quoteFileLines(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func boxScreen(transcript, box string) string {
	return transcript + "\n\n" + rule + "\n❯ " + box + "\n" + rule + "\n  ~/dev/example\n"
}

func TestInputBoxTextReadsTheLastBox(t *testing.T) {
	sent := "❯ [FROM: inbox]\n  Message in thread C1/1.0 from U0ABC at 1.5:\n\n● reply"
	for name, c := range map[string]struct {
		screen, box string
		ok          bool
	}{
		"empty":    {boxScreen(sent, ""), "", true},
		"one line": {boxScreen(sent, "[Image #4][Pasted text #5 +6 lines]"), "[Image #4][Pasted text #5 +6 lines]", true},
		"wrapped": {boxScreen(sent, "[Image #4][Pasted text #5 +6 lines][Image #6][Pasted text #7 +6\n  lines]"),
			"[Image #4][Pasted text #5 +6 lines][Image #6][Pasted text #7 +6\nlines]", true},
		"no box":       {sent + "\n", "", false},
		"menu":         {" Choose the text style\n\n ❯ 1. Dark mode\n   2. Light mode\n", "", false},
		"box unclosed": {sent + "\n" + rule + "\n❯ half drawn\n", "", false},
	} {
		box, ok := InputBoxText(c.screen)
		if box != c.box || ok != c.ok {
			t.Errorf("%s: got %q %v, want %q %v", name, box, ok, c.box, c.ok)
		}
	}
}

func TestLeftoverTraceNamesWhatShowsTheMessage(t *testing.T) {
	text := "[FROM: inbox]\nMessage in thread C1/1.0"
	for box, want := range map[string]string{
		"[Image #4][Pasted text #5 +6 lines]": "an [Image #…] placeholder",
		"[Pasted text #5 +6 lines]":           "a [Pasted text #…] placeholder",
		"[FROM: inbox] Message in thread":     "the message's first line",
		"":                                    "",
		"something a person typed":            "",
	} {
		if got := LeftoverTrace(box, text); got != want {
			t.Errorf("box %q: got %q, want %q", box, got, want)
		}
	}
}

// fakeClaudeHerdr puts on PATH a herdr for one Claude Code agent `a`: its
// status is the file `status`, its screen the file `screen` (and
// `screen-typed` once the message is typed), `agent prompt` refuses a
// blocked agent, else types and reports agent_prompt_stalled (setting the status from `prompt-status`
// when that file exists), `send-keys` makes the agent working, `agent
// wait` matches working or blocked. Every call is logged in `log`.
func fakeClaudeHerdr(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/bash
d=` + dir + `
[ "$1" = --session ] && shift 2
echo "$*" >> "$d/log"
case "$1 $2" in
"status server") echo '{"running":true}' ;;
"agent get")
  [ -e "$d/gone" ] && { echo '{"id":"1","error":{"code":"agent_not_found","message":"no agent"}}'; exit 1; }
  echo '{"id":"1","result":{"agent":{"agent":"claude","agent_status":"'"$(cat "$d/status")"'","pane_id":""}}}' ;;
"agent read")
  if [ -e "$d/typed" ] && [ -e "$d/screen-typed" ]; then cat "$d/screen-typed"; else cat "$d/screen"; fi ;;
"agent prompt")
  [ "$(cat "$d/status")" = blocked ] && { echo '{"id":"1","error":{"code":"agent_blocked","message":"blocked"}}' >&2; exit 1; }
  touch "$d/typed"
  [ -e "$d/prompt-status" ] && cp "$d/prompt-status" "$d/status"
  echo '{"id":"1","error":{"code":"agent_prompt_stalled","message":"no observed working state"}}' >&2; exit 1 ;;
"agent send-keys") echo working > "$d/status"; echo '{"id":"1","result":{"type":"ok"}}' ;;
"agent wait")
  case "$(cat "$d/status")" in
  working|blocked) echo '{"id":"1","result":{"agent":{}}}' ;;
  *) echo '{"id":"1","error":{"code":"timeout","message":"timed out"}}' >&2; exit 1 ;;
  esac ;;
*) echo '{"id":"1","error":{"code":"unknown","message":"not faked"}}'; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "herdr"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	t.Setenv("HOME", t.TempDir())
	return dir
}

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func keysPressed(t *testing.T, dir string) int {
	t.Helper()
	log, err := os.ReadFile(filepath.Join(dir, "log"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(log), "agent send-keys a enter")
}

// The image's Enter dropped (2026-10-30): the message waits in the input box
// after herdr's stall, and Deliver presses Enter once instead of leaving it
// there for the retry to type a second copy beside it.
func TestDeliverSubmitsTheMessageLeftInTheInputBox(t *testing.T) {
	dir := fakeClaudeHerdr(t)
	write(t, dir, map[string]string{"status": "done", "screen": boxScreen("● earlier reply", ""),
		"screen-typed": boxScreen("● earlier reply", "[Image #4][Pasted text #5 +6 lines]")})
	code, err := Deliver(herdr.New(nil), "a", "[FROM: inbox]\nlook at /x/shot.png\n")
	if err != nil || code != exit.Ok {
		t.Fatalf("Deliver: %v, exit %d", err, code)
	}
	if n := keysPressed(t, dir); n != 1 {
		t.Errorf("Enter pressed %d times, want once", n)
	}
}

func TestDeliverPressesNothingWithoutTheMessageInTheBox(t *testing.T) {
	for name, files := range map[string]map[string]string{
		// The box held the same before the message was typed.
		"unchanged": {"status": "idle", "screen": boxScreen("", "[Pasted text #1 +3 lines]")},
		// Something else, no trace of the message.
		"other text": {"status": "idle", "screen": boxScreen("", ""), "screen-typed": boxScreen("", "a draft")},
		// Empty: the message went somewhere, not into the box.
		"empty": {"status": "idle", "screen": boxScreen("", "")},
		// A screen that waits for a person's answer.
		"blocked": {"status": "idle", "prompt-status": "blocked", "screen": boxScreen("", ""),
			"screen-typed": boxScreen("", "[Pasted text #1 +3 lines]")},
	} {
		dir := fakeClaudeHerdr(t)
		write(t, dir, files)
		code, err := Deliver(herdr.New(nil), "a", "[FROM: inbox]\nhello\n")
		if err != nil || code != exit.Unknown {
			t.Errorf("%s: %v, exit %d, want 2", name, err, code)
		}
		if n := keysPressed(t, dir); n != 0 {
			t.Errorf("%s: Enter pressed %d times", name, n)
		}
	}
}

// typedLedger is a ledger with a live thread agent `a` for thread C1/1.0.
func typedLedger(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.OpenAt(filepath.Join(t.TempDir(), "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.Exec("INSERT INTO agents (name, role, thread, state, started_at) VALUES ('a', 'thread', 'C1/1.0', " +
		"'active', 0)"); err != nil {
		t.Fatal(err)
	}
	return conn
}

// A message fednet hands over again after exit 2 is not typed again
// (2026-10-30: one message typed into an agent eight times). Enter is
// pressed only while the input box still holds what it held right after
// the message was typed; anything else in it is left alone.
func TestARetryOfATypedMessageTypesNothing(t *testing.T) {
	msg := inboxMessage(eventOf(t, filesEvent), "", "/s")
	left := "[Image #4][Pasted text #5 +6 lines]"
	for name, c := range map[string]struct {
		files map[string]string
		box   any
		code  exit.Code
		typed bool
		keys  int
	}{
		"still in the box": {map[string]string{"status": "done", "screen": boxScreen("", left)}, left, exit.Ok, true, 1},
		"more typed after it": {map[string]string{"status": "idle", "screen": boxScreen("", left+"[Pasted text #6 +2 lines]")},
			left, exit.Ok, true, 1},
		"box empty":       {map[string]string{"status": "idle", "screen": boxScreen("", "")}, left, exit.Ok, true, 0},
		"working, queued": {map[string]string{"status": "working", "screen": boxScreen("", "")}, left, exit.Ok, true, 0},
		// A person's draft, or another message: not this one's to submit.
		"something else": {map[string]string{"status": "idle", "screen": boxScreen("", "a draft [Pasted text #9 +2 lines]")},
			left, exit.Ok, true, 0},
		"box not read after typing": {map[string]string{"status": "idle", "screen": boxScreen("", left)}, nil,
			exit.Unknown, true, 0},
		"blocked": {map[string]string{"status": "blocked", "screen": boxScreen("", left)}, left, exit.Blocked, true, 0},
		"gone":    {map[string]string{"status": "idle", "gone": "", "screen": ""}, left, exit.Ok, false, 0},
	} {
		dir := fakeClaudeHerdr(t)
		write(t, dir, c.files)
		conn := typedLedger(t)
		if _, err := conn.Exec("INSERT INTO inbox (msg_id, thread, state, received_at, box) VALUES ('m1', 'C1/1.0', "+
			"'typed', 0, ?1)", c.box); err != nil {
			t.Fatal(err)
		}
		code, typed, err := submitTyped(herdr.New(nil), conn, "main", "m1", msg)
		if err != nil || code != c.code || typed != c.typed {
			t.Errorf("%s: %v, exit %d, typed %v; want exit %d, typed %v", name, err, code, typed, c.code, c.typed)
		}
		if n := keysPressed(t, dir); n != c.keys {
			t.Errorf("%s: Enter pressed %d times, want %d", name, n, c.keys)
		}
		if log, _ := os.ReadFile(filepath.Join(dir, "log")); strings.Contains(string(log), "agent prompt") {
			t.Errorf("%s: the message was typed again", name)
		}
	}
}

// What the input box held right after a message was typed with no clear
// signal is recorded with it, for its retry.
func TestATypedMessageKeepsWhatTheBoxHeld(t *testing.T) {
	dir := fakeClaudeHerdr(t)
	write(t, dir, map[string]string{"status": "idle", "screen": boxScreen("", "[Pasted text #3 +4 lines]")})
	conn := typedLedger(t)
	if _, err := conn.Exec("INSERT INTO inbox (msg_id, thread, state, received_at) VALUES ('m1', 'C1/1.0', 'reserved', 0)"); err != nil {
		t.Fatal(err)
	}
	if err := setTyped(herdr.New(nil), conn, "m1", "C1/1.0"); err != nil {
		t.Fatal(err)
	}
	var row string
	if err := conn.QueryRow("SELECT state || ' ' || box FROM inbox WHERE msg_id = 'm1'").Scan(&row); err != nil ||
		row != "typed [Pasted text #3 +4 lines]" {
		t.Errorf("row %q (%v)", row, err)
	}
}

// A thread agent whose first message gave no clear signal has the message
// typed in: its row is active, so the thread's next message reaches it
// without the prompt (2026-10-30: each later message came with the whole
// prompt again).
func TestATypedFirstMessageMakesTheRowActive(t *testing.T) {
	conn := typedLedger(t)
	if _, err := conn.Exec("UPDATE agents SET state = 'starting'"); err != nil {
		t.Fatal(err)
	}
	code, err := typedFirst(conn, "a")
	if err != nil || code != exit.Unknown {
		t.Fatalf("typedFirst: %v, exit %d", err, code)
	}
	if _, state, _ := liveThreadAgent(conn, "C1/1.0"); state != "active" {
		t.Errorf("row %q, want active", state)
	}
}

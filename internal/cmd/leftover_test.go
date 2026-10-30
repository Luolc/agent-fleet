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

// A file's path in the message fleet types must not be read as an image to
// attach: Claude Code would drop the Enter herdr presses while it reads the
// image, and the message would wait in the input box (2026-10-30).
func TestTheFilesSectionHasNoPartClaudeCodeTakesForAnImage(t *testing.T) {
	text, err := WithHeader(inboxSender, inboxMessage(eventOf(t, filesEvent), "", "/s").body())
	if err != nil {
		t.Fatal(err)
	}
	if parts := claudeImageParts(text); len(parts) > 0 {
		t.Errorf("parts read as images: %q", parts)
	}
	if !strings.Contains(text, "path `/var/cache/fednet/F0IMG1-shot.png`") {
		t.Errorf("the path is not given: %q", text)
	}
	if parts := claudeImageParts("see " + "/var/cache/fednet/F0IMG1-shot.png"); len(parts) != 1 {
		t.Errorf("the check misses a path at the end of a line: %q", parts)
	}
}

func boxScreen(transcript, box string) string {
	return transcript + "\n\n" + rule + "\n❯ " + box + "\n" + rule + "\n  ~/x-repo/general · Opus\n"
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
// `screen-typed` once the message is typed), `agent prompt` types and
// reports agent_prompt_stalled (setting the status from `prompt-status`
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
// (2026-10-30: one message typed into an agent eight times).
func TestARetryOfATypedMessageTypesNothing(t *testing.T) {
	msg := inboxMessage(eventOf(t, filesEvent), "", "/s")
	for name, c := range map[string]struct {
		files map[string]string
		code  exit.Code
		typed bool
		keys  int
	}{
		"left in the box": {map[string]string{"status": "done",
			"screen": boxScreen("", "[Image #4][Pasted text #5 +6 lines]")}, exit.Ok, true, 1},
		"box empty":       {map[string]string{"status": "idle", "screen": boxScreen("", "")}, exit.Ok, true, 0},
		"working, queued": {map[string]string{"status": "working", "screen": boxScreen("", "")}, exit.Ok, true, 0},
		"blocked":         {map[string]string{"status": "blocked", "screen": boxScreen("", "")}, exit.Blocked, true, 0},
		"something else":  {map[string]string{"status": "idle", "screen": boxScreen("", "a draft")}, exit.Unknown, true, 0},
		"gone":            {map[string]string{"status": "idle", "gone": "", "screen": ""}, exit.Ok, false, 0},
	} {
		dir := fakeClaudeHerdr(t)
		write(t, dir, c.files)
		code, typed, err := submitTyped(herdr.New(nil), typedLedger(t), "main", msg)
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

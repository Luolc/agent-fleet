// `send` to a Claude Code session against the watch fakes: herdr gives the
// agent's pane, and the session's transcript is under the hermetic HOME.
package main

import (
	"strings"
	"testing"
)

// pausedRecords are what Claude Code records of a turn it paused on its
// `Session paused` dialog: a refused message, then the retry's reply
// asking for a tool; the turn's next refusal is not recorded.
func pausedRecords() []string {
	return []string{
		user("q-0", "2030-01-02T03:00:00.000Z"),
		assistant("q-1", "2030-01-02T03:00:01.000Z", "msg-q1", "claude-test-large", "refusal"),
		assistant("q-2", "2030-01-02T03:00:02.000Z", "msg-q2", "claude-test-large", "tool_use"),
	}
}

// sendWorld is the watch world with the Claude Code agent x-lead in pane
// p1, herdr finding it `status`, its session's transcript of `records`.
func sendWorld(t *testing.T, status string, records ...string) *watchWorld {
	t.Helper()
	w := newWatchWorld(t)
	w.herdrList("x-lead " + status + " 1")
	w.file("status-x-lead", status)
	w.file("pane-x-lead", "p1")
	w.claudeSession("p1", 4101, sessionThread, records...)
	return w
}

// A session paused on the dialog, which herdr may read as idle, done or
// blocked, gets nothing typed: exit 8, saying to press no key, with no
// screen and nothing of the conversation.
func TestSendTypesNothingIntoASessionPausedOnItsDialog(t *testing.T) {
	for _, status := range []string{"idle", "done", "blocked"} {
		w := sendWorld(t, status, pausedRecords()...)
		out := w.run("hello\n", []string{"send", "x-lead"}, "FLEET_AGENT=x-worker")
		if out.code != 8 || len(w.prompts()) != 0 || out.stdout != "" {
			t.Fatalf("%s: %+v, prompts %q", status, out, w.prompts())
		}
		hasAll(t, "stderr", out.stderr, "x-lead is taken to be paused on Claude Code's `Session paused` dialog",
			"Nothing was typed", "Press no key", "`fleet worker end`")
		if strings.Contains(out.stderr, refusedText) {
			t.Errorf("the conversation's text was passed on: %s", out.stderr)
		}
	}
}

// A session waiting at its prompt after a stop with no fallback, one
// whose refused turn went on to its end, one at work, and one whose
// session fleet cannot find, get the message as before.
func TestSendDeliversToASessionNotPausedOnItsDialog(t *testing.T) {
	atPrompt := []string{
		assistant("t-1", "2030-01-02T04:00:00.000Z", "msg-t1", "claude-test-large", "end_turn"),
		assistant("t-2", "2030-01-02T04:00:01.000Z", "msg-t2", "claude-test-large", "refusal"),
		noFallback("t-3", "2030-01-02T04:00:01.500Z", "claude-test-large"),
		assistant("t-4", "2030-01-02T04:00:01.600Z", "msg-t3", "<synthetic>", "refusal"),
	}
	recovered := append(pausedRecords(),
		assistant("q-3", "2030-01-02T03:00:03.000Z", "msg-q3", "claude-test-large", "end_turn"))
	for _, c := range []struct {
		what    string
		status  string
		records []string
	}{
		{"at its prompt", "idle", atPrompt},
		{"refused once, its turn ended", "idle", recovered},
		{"paused records, working", "working", pausedRecords()},
	} {
		w := sendWorld(t, c.status, c.records...)
		out := w.run("hello\n", []string{"send", "x-lead"}, "FLEET_AGENT=x-worker")
		if out.code != 0 || len(w.promptsTo("x-lead")) != 1 {
			t.Errorf("%s: %+v, prompts %q", c.what, out, w.prompts())
		}
	}
	w := sendWorld(t, "idle", pausedRecords()...)
	w.file("procinfo-p1", "")
	if out := w.run("hello\n", []string{"send", "x-lead"}, "FLEET_AGENT=x-worker"); out.code != 0 ||
		len(w.promptsTo("x-lead")) != 1 {
		t.Errorf("no session found: %+v, prompts %q", out, w.prompts())
	}
}

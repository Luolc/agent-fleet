// Integration tests for the status emoji fleet keeps on a thread's root
// message, against the thread fakes of thread_test.go and the watch fakes
// of watch_test.go. Hermetic, see main_test.go.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// marked is what the fake fednet was asked to react with since the last
// call, one `+name` or `-name` each, and forgets it.
func marked(t *testing.T, path string) string {
	t.Helper()
	data, _ := os.ReadFile(path)
	_ = os.Remove(path)
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		sign := "+"
		if strings.Contains(line, " -remove ") {
			sign = "-"
		}
		if !strings.Contains(line, "react -socket ") {
			t.Fatalf("not a react: %q", line)
		}
		got = append(got, fields[len(fields)-2]+" "+sign+fields[len(fields)-1])
	}
	return strings.Join(got, ", ")
}

// The state mark follows the session: busy while the agent runs, asking while
// its card waits, busy again once the card is taken back, done once the
// session ended; a message that changes nothing reacts with nothing.
// eyes go on with the first message.
func TestTheThreadsMarkFollowsItsSession(t *testing.T) {
	w := threadWorld(t, "")
	reacts := filepath.Join(w.dir, "reacts")
	busy := threadKey + " +hourglass_flowing_sand, " + threadKey + " -question, " + threadKey + " -white_check_mark"
	if out := w.inbox(w.event("m1", "first", "")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if got := marked(t, reacts); got != busy+", "+threadKey+" +eyes" {
		t.Errorf("a new session: %q, want %q and eyes", got, busy)
	}
	if out := w.inbox(w.event("m2", "second", "")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if got := marked(t, reacts); got != "" {
		t.Errorf("a message to the live agent: %q, want nothing", got)
	}
	if out := w.asThreadAgent("TH-5", "ask-human", "--card", task(w, "q.json", `{"question": "Which bucket?"}`)); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	asking := threadKey + " +question, " + threadKey + " -hourglass_flowing_sand, " + threadKey + " -white_check_mark"
	if got := marked(t, reacts); got != asking {
		t.Errorf("a card asked: %q, want %q", got, asking)
	}
	if out := w.asThreadAgent("TH-5", "ask-human", "--close", "1"); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if got := marked(t, reacts); got != busy {
		t.Errorf("the card taken back: %q, want %q", got, busy)
	}
	if err := os.WriteFile(filepath.Join(w.dir, "has-thread-c0123-1700000000-123"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if out := w.asThreadAgent("TH-5", "thread", "end", "--summary-file", task(w, "s.md", "Done.\n")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	done := threadKey + " +white_check_mark, " + threadKey + " -question, " + threadKey + " -hourglass_flowing_sand"
	if got := marked(t, reacts); got != done {
		t.Errorf("the session ended: %q, want %q", got, done)
	}
}

// A mark fednet fails to put is a warning: the command does what it does
// anyway, and nothing records the mark, so the next change tries again.
func TestAFailedMarkOnlyWarns(t *testing.T) {
	w := threadWorld(t, "")
	if err := os.WriteFile(filepath.Join(w.dir, "react-down"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out := w.inbox(w.event("m1", "first", ""))
	if out.code != 0 || threadAgentRow(w) != "TH-5 active" ||
		!strings.Contains(out.stderr, "warning: the emoji on thread "+threadKey+" were not updated") ||
		!strings.Contains(out.stderr, "missing_scope") {
		t.Fatalf("%+v", out)
	}
	if got := stepsOf(w); strings.Contains(got, "hourglass") {
		t.Errorf("steps = %q", got)
	}
	_ = os.Remove(filepath.Join(w.dir, "react-down"))
	_ = os.Remove(filepath.Join(w.dir, "reacts"))
	if out := w.inbox(w.event("m2", "second", "")); out.code != 0 || out.stderr != "" {
		t.Fatalf("%+v", out)
	}
	if got := marked(t, filepath.Join(w.dir, "reacts")); !strings.HasPrefix(got, threadKey+" +hourglass_flowing_sand") {
		t.Errorf("the retry: %q", got)
	}
}

// watch puts the marks the commands left stale: each thread's own, newest
// thread first, and none on a thread whose recorded marks are current.
func TestWatchCatchesUpTheMarks(t *testing.T) {
	w := watchThreadWorld(t)
	w.exec("INSERT INTO threads (thread, slug, created_at) VALUES ('C1/1.0', 'c1', 0), ('C2/2.0', 'c2', 0), "+
		"('C3/3.0', 'c3', 0), ('C4/4.0', 'c4', 0)",
		"INSERT INTO questions (job, thread, asked_by, text, state, asked_at, card, card_id) "+
			"VALUES ('', 'C1/1.0', 'thread-c1', '{}', 'pending', "+fmt.Sprint(t0)+", 1, 'card-1')",
		"INSERT INTO jobs (job, lead_cwd, home_thread, state, started_at) VALUES ('item-1', '/c', 'C2/2.0', 'open', 0)",
		"INSERT INTO steps (key, step, done_at) VALUES ('mark:C4/4.0', 'white_check_mark', 0), ('look:C4/4.0', 'on', 0)")
	w.lastMessage("C1/1.0", t0)
	w.lastMessage("C2/2.0", t0)
	w.watchAt(0, 0)
	got := marked(t, filepath.Join(w.dir, "react.log"))
	if got != "C4/4.0 +eyes, C3/3.0 +white_check_mark, C3/3.0 -question, C3/3.0 -hourglass_flowing_sand, "+
		"C2/2.0 +hourglass_flowing_sand, C2/2.0 -question, C2/2.0 -white_check_mark, "+
		"C1/1.0 +question, C1/1.0 -hourglass_flowing_sand, C1/1.0 -white_check_mark" {
		t.Errorf("reacts = %q", got)
	}
	w.watchAt(time.Minute, 0)
	if got := marked(t, filepath.Join(w.dir, "react.log")); got != "" {
		t.Errorf("a second run: %q, want nothing", got)
	}
}

// A person's package on the root takes the eyes off, once however often
// fednet hands it over; another reaction changes nothing, and the next
// message puts the eyes back. No agent hears of a reaction.
func TestArchivingTakesTheEyesOffUntilTheNextMessage(t *testing.T) {
	w := threadWorld(t, "")
	reacts := filepath.Join(w.dir, "reacts")
	if out := w.inbox(w.event("m1", "first", "")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	_ = os.Remove(reacts)
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	reaction := func(msgID, name string) result {
		t.Helper()
		return w.inbox(task(w, msgID+".json", `{"msg_id":"`+msgID+`","payload":{"type":"reaction","thread":"`+threadKey+
			`","user":"U0ABC","user_name":"example","reaction":"`+name+`"}}`))
	}
	for _, id := range []string{"r1", "r2"} {
		if out := reaction(id, "package"); out.code != 0 {
			t.Fatalf("%+v", out)
		}
	}
	if got := marked(t, reacts); got != threadKey+" -eyes" {
		t.Errorf("archived twice: %q, want the eyes off once", got)
	}
	if out := reaction("r3", "eyes"); out.code != 0 || !strings.Contains(out.stdout, "ignored r3") {
		t.Errorf("%+v", out)
	}
	if got := marked(t, reacts); got != "" {
		t.Errorf("another reaction: %q, want nothing", got)
	}
	if got := w.calls(); got != "" {
		t.Errorf("calls = %q, want none", got)
	}
	if out := w.inbox(w.event("m2", "again", "")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if got := marked(t, reacts); got != threadKey+" +eyes" {
		t.Errorf("the next message: %q, want the eyes on", got)
	}
	// A thread the scope does not know is left alone.
	other := w.inbox(task(w, "r4.json", `{"msg_id":"r4","payload":{"type":"reaction","thread":"C0999/1.1","user":"U0ABC",`+
		`"reaction":"package"}}`))
	if other.code != 0 || !strings.Contains(other.stdout, "does not know thread C0999/1.1") || marked(t, reacts) != "" {
		t.Errorf("%+v", other)
	}
}

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
// session ended, and the same around a press on a card; a message that
// changes nothing reacts with nothing. eyes go on with the first message.
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
	if out := w.asThreadAgent("TH-5", "ask-human", "--card", task(w, "q2.json", `{"question": "Which month?"}`)); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if got := marked(t, reacts); got != asking {
		t.Errorf("another card asked: %q, want %q", got, asking)
	}
	if out := w.inbox(w.answerEvent("a1", `{"question_id":"main:2","card_id":"card-2","outcome":"answered",`+
		`"choice":"October","note":"","snooze_until":null,"by":"U0ABC","at":"2026-10-16T10:00:00-07:00"}`)); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if got := marked(t, reacts); got != busy {
		t.Errorf("the card answered: %q, want %q", got, busy)
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

// watch puts the marks the commands left stale, and on a thread fleet
// never marked (one from before the marks) its state mark and eyes: each
// thread's own, newest thread first, and none on a thread whose recorded
// marks are current.
func TestWatchCatchesUpTheMarks(t *testing.T) {
	w := watchThreadWorld(t)
	w.exec("INSERT INTO threads (thread, slug, created_at) VALUES ('C1/1.0', 'c1', 0), ('C2/2.0', 'c2', 0), "+
		"('C3/3.0', 'c3', 0), ('C4/4.0', 'c4', 0)",
		"INSERT INTO questions (job, thread, asked_by, text, state, asked_at, card, card_id) "+
			"VALUES ('', 'C1/1.0', 'thread-c1', '{}', 'pending', "+fmt.Sprint(t0)+", 1, 'card-1')",
		"INSERT INTO jobs (job, lead_cwd, home_thread, state, started_at) VALUES ('item-1', '/c', 'C2/2.0', 'open', 0)",
		"INSERT INTO steps (key, step, done_at) VALUES ('mark:C4/4.0', 'white_check_mark', "+fmt.Sprint(t0)+"), "+
			"('mark-put:C4/4.0', 'put', "+fmt.Sprint(t0)+"), ('look:C4/4.0', 'on', 0)")
	w.lastMessage("C1/1.0", t0)
	w.lastMessage("C2/2.0", t0)
	w.watchAt(0, 0)
	got := marked(t, filepath.Join(w.dir, "react.log"))
	if got != "C4/4.0 +eyes, C3/3.0 +white_check_mark, C3/3.0 -question, C3/3.0 -hourglass_flowing_sand, C3/3.0 +eyes, "+
		"C2/2.0 +hourglass_flowing_sand, C2/2.0 -question, C2/2.0 -white_check_mark, C2/2.0 +eyes, "+
		"C1/1.0 +question, C1/1.0 -hourglass_flowing_sand, C1/1.0 -white_check_mark, C1/1.0 +eyes" {
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

// withFednet puts the thread fakes' fednet next to the world's fake herdr
// and gives the scope `name` its socket.
func withFednet(w *world, name string) {
	w.t.Helper()
	if err := os.WriteFile(filepath.Join(w.dir, "fake-herdr", "fednet"), []byte(threadFednet), 0o755); err != nil {
		w.t.Fatal(err)
	}
	w.scopeConfig(name, `{"fednet": {"socket": "/run/fednet.sock"}}`)
}

// The commands that start and end a job mark its home thread: `job start`
// busy, `job end` by the lead and the reclaim of `job end --force` done
// once nothing else runs there.
func TestStartingAndEndingAJobMarkItsHomeThread(t *testing.T) {
	w := newWorld(t)
	withFednet(w, scope)
	w.configure(withLinear)
	w.useStartHerdr()
	w.fakeAtbCreating("")
	reacts := filepath.Join(w.dir, "reacts")
	conn := w.ledger()
	for _, stmt := range []string{
		"INSERT INTO threads (thread, slug, created_at) VALUES ('" + threadKey + "', 'c0123', 0)",
		"INSERT INTO steps (key, step, done_at) VALUES ('mark:" + threadKey + "', 'white_check_mark', 0), ('look-put:" + threadKey + "', 'on', 0)",
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	conn.Close()
	out := w.run("", []string{"job", "start", "item-7", "--repo", "example-dataset", "--parent-issue", "EX-10", "--task-file",
		task(w, "task.md", "Import the A table\n")},
		"FLEET_AGENT=thread-c0123-1700000000-123", "FLEET_ROLE=thread", "FLEET_SCOPE="+scope, "FLEET_THREAD="+threadKey,
		"FLEET_ISSUE=TH-5", linearKey)
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	busy := threadKey + " +hourglass_flowing_sand, " + threadKey + " -question, " + threadKey + " -white_check_mark"
	if got := marked(t, reacts); got != busy {
		t.Errorf("job start: %q, want %q", got, busy)
	}
	done := threadKey + " +white_check_mark, " + threadKey + " -question, " + threadKey + " -hourglass_flowing_sand"
	w.closeHerdr("")
	if out := w.asThread("job", "end", "item-7", "--force"); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if got := marked(t, reacts); got != done {
		t.Errorf("job end --force: %q, want %q", got, done)
	}
	// Ended by its lead, the conclusion delivered by an earlier run, so
	// this ending goes on from there.
	openJobWithLead(w, "")
	conn = w.ledger()
	for _, stmt := range []string{
		"UPDATE jobs SET home_thread = '" + threadKey + "' WHERE job = 'item-1'",
		"INSERT INTO steps (key, step, done_at) SELECT 'job-end:' || id, 'conclude', 0 FROM jobs WHERE job = 'item-1'",
		"UPDATE steps SET step = 'hourglass_flowing_sand' WHERE key = 'mark:" + threadKey + "'",
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	conn.Close()
	w.jobEndHerdr()
	if out := w.endJob("--report-file", task(w, "report.md", "done\n")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if got := marked(t, reacts); got != done {
		t.Errorf("job end: %q, want %q", got, done)
	}
}

// A home thread ask-thread opens for a job is marked busy, and to look at.
func TestAHomeThreadAskThreadOpensIsMarked(t *testing.T) {
	w := threadWorld(t, "")
	openSingleRepoJob(w)
	task(w, "routes.json", `{"routes":[{"channel":"C0789","name":"repo-example-dataset"}]}`)
	if out := w.askThread("--card", task(w, "q.md", "Revert the commit?\n")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	k := "C0789/1700000099.000"
	want := k + " +hourglass_flowing_sand, " + k + " -question, " + k + " -white_check_mark, " + k + " +eyes"
	if got := marked(t, filepath.Join(w.dir, "reacts")); got != want {
		t.Errorf("reacts = %q, want %q", got, want)
	}
}

// watch puts a state mark again, the other two taken off, markCheck after
// it last did: every 10 minutes while the thread is not done, and once
// after it became done, so a mark a race left beside the recorded one
// (busy left on a thread whose session just ended) is taken off.
func TestWatchPutsTheStateMarkAgainToTakeOffWhatARaceLeft(t *testing.T) {
	w := watchThreadWorld(t)
	at := fmt.Sprint(t0)
	w.exec("INSERT INTO threads (thread, slug, created_at) VALUES ('C5/5.0', 'c5', 0), ('C6/6.0', 'c6', 0)",
		"INSERT INTO jobs (job, lead_cwd, home_thread, state, started_at) VALUES ('item-1', '/c', 'C6/6.0', 'open', 0)",
		"INSERT INTO steps (key, step, done_at) VALUES ('mark:C5/5.0', 'white_check_mark', "+at+"), "+
			"('mark-put:C5/5.0', 'put', "+at+"), ('mark:C6/6.0', 'hourglass_flowing_sand', "+at+"), ('mark-put:C6/6.0', 'put', "+at+"), "+
			"('look-put:C5/5.0', 'on', 0), ('look-put:C6/6.0', 'on', 0)")
	w.lastMessage("C6/6.0", t0)
	log := filepath.Join(w.dir, "react.log")
	w.watchAt(time.Minute, 0)
	if got := marked(t, log); got != "" {
		t.Errorf("within 10 minutes: %q, want nothing", got)
	}
	w.watchAt(10*time.Minute, 0)
	both := "C6/6.0 +hourglass_flowing_sand, C6/6.0 -question, C6/6.0 -white_check_mark, " +
		"C5/5.0 +white_check_mark, C5/5.0 -question, C5/5.0 -hourglass_flowing_sand"
	if got := marked(t, log); got != both {
		t.Errorf("after 10 minutes: %q, want %q", got, both)
	}
	w.watchAt(20*time.Minute, 0)
	if got := marked(t, log); got != "C6/6.0 +hourglass_flowing_sand, C6/6.0 -question, C6/6.0 -white_check_mark" {
		t.Errorf("after 20 minutes: %q, want the open thread only", got)
	}
}

// watch marks at most 10 threads a run, newest first, so the threads of a
// ledger from before the marks are caught up over runs.
func TestWatchMarksAtMostTenThreadsARun(t *testing.T) {
	w := watchThreadWorld(t)
	at := fmt.Sprint(t0)
	// Their state marks are current; only eyes are missing, one call each.
	for i := 1; i <= 12; i++ {
		k := fmt.Sprintf("C%d/%d.0", i, i)
		w.exec("INSERT INTO threads (thread, slug, created_at) VALUES ('"+k+"', 'c', 0)",
			"INSERT INTO steps (key, step, done_at) VALUES ('mark:"+k+"', 'white_check_mark', "+at+"), ('mark-put:"+k+"', 'put', "+at+")")
	}
	log := filepath.Join(w.dir, "react.log")
	w.watchAt(0, 0)
	if got := marked(t, log); got != "C12/12.0 +eyes, C11/11.0 +eyes, C10/10.0 +eyes, C9/9.0 +eyes, C8/8.0 +eyes, "+
		"C7/7.0 +eyes, C6/6.0 +eyes, C5/5.0 +eyes, C4/4.0 +eyes, C3/3.0 +eyes" {
		t.Errorf("first run: %q", got)
	}
	w.watchAt(time.Minute, 0)
	if got := marked(t, log); got != "C2/2.0 +eyes, C1/1.0 +eyes" {
		t.Errorf("second run: %q", got)
	}
}

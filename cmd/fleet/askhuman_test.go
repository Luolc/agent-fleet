// Integration tests for `ask-human` and the answered marking in `inbox`,
// against the thread fakes of thread_test.go. Hermetic, see main_test.go.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openJobWithHomeThread puts job item-1 (cross-repo, Linear off) in the
// default ledger as open with its lead live and threadKey as its home
// thread when `home` is set.
func openJobWithHomeThread(w *world, home bool) {
	w.t.Helper()
	conn := w.defaultLedger()
	defer conn.Close()
	thread := ""
	if home {
		thread = threadKey
	}
	for _, stmt := range []string{
		"INSERT INTO jobs (job, lead_cwd, home_thread, state, started_at) VALUES ('item-1', '/c', ?1, 'open', 0)",
		"INSERT INTO agents (name, role, job, parent, state, started_at) VALUES ('item-1-lead', 'lead', 'item-1', 'thread-x', 'active', 0)",
	} {
		if _, err := conn.Exec(stmt, thread); err != nil {
			w.t.Fatal(err)
		}
	}
}

func (w *world) askHuman(args ...string) result {
	w.t.Helper()
	return w.run("", append([]string{"ask-human"}, args...),
		"FLEET_AGENT=item-1-lead", "FLEET_ROLE=lead", "FLEET_PARENT=thread-x", "FLEET_TARGET=default", "FLEET_JOB=item-1", linearKey)
}

// questions are the rows of the questions table: job, thread, asked_by,
// approval, state, one line each.
func questions(w *world) string {
	w.t.Helper()
	conn := w.defaultLedger()
	defer conn.Close()
	rows, err := conn.Query("SELECT job, thread, asked_by, approval, state FROM questions ORDER BY id")
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var job, thread, by, state string
		var approval int
		if err := rows.Scan(&job, &thread, &by, &approval, &state); err != nil {
			w.t.Fatal(err)
		}
		lines = append(lines, strings.Join([]string{job, thread, by, map[int]string{0: "plain", 1: "approval"}[approval], state}, " "))
	}
	return strings.Join(lines, "\n")
}

func TestAskHumanReachesTheLiveThreadAgentAndIsAnsweredByTheNextMessage(t *testing.T) {
	w := threadWorld(t, "")
	openJobWithHomeThread(w, true)
	if out := w.inbox(w.event("m1", "start the import", "")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	question := task(w, "q.md", "Which month: September or October?\n")
	out := w.askHuman("--file", question)
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if got := strings.TrimSpace(w.calls()); got != "herdr agent get\nherdr agent prompt" {
		t.Errorf("calls = %q", got)
	}
	argv := w.file("argv")
	if !strings.HasPrefix(argv, "agent\nprompt\nthread-c0123-1700000000-123\n[FROM: item-1-lead]\nQuestion from item-1-lead for the people in thread "+
		threadKey+". Post it to the thread with `fednet client post`; when they answer, pass the answer on with `fleet send item-1-lead --file <file>`.\n\n"+
		"Which month: September or October?\n") {
		t.Errorf("argv = %q", argv)
	}
	if got := questions(w); got != "item-1 "+threadKey+" item-1-lead plain pending" {
		t.Errorf("questions = %q", got)
	}
	out = w.askHuman("--file", task(w, "a.md", "May I delete the old table?\n"), "--approval")
	if out.code != 1 || !strings.Contains(out.stderr, "approval cards are not supported yet") {
		t.Errorf("approval: %+v", out)
	}
	out = w.inbox(w.event("m2", "October", ""))
	if out.code != 0 || !strings.Contains(out.stdout, "1 pending question(s) in thread "+threadKey+" answered") {
		t.Errorf("answer: %+v", out)
	}
	if got := questions(w); got != "item-1 "+threadKey+" item-1-lead plain answered" {
		t.Errorf("questions = %q", got)
	}
}

func TestAskHumanStartsAThreadAgentWhenTheHomeThreadHasNone(t *testing.T) {
	w := threadWorld(t, "")
	openJobWithHomeThread(w, true)
	// The thread is known with a ticket; its agent ended.
	if out := w.inbox(w.event("m1", "start the import", "ctx")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	conn := w.defaultLedger()
	if _, err := conn.Exec("UPDATE agents SET state = 'ended' WHERE role = 'thread'"); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	out := w.askHuman("--file", task(w, "q.md", "Which month?\n"))
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	calls := w.calls()
	if !strings.Contains(calls, "atb linear claim TH-5 --agent thread-c0123-1700000000-123") ||
		!strings.Contains(calls, "herdr agent start") || !strings.Contains(calls, "herdr agent prompt") {
		t.Errorf("calls = %q", calls)
	}
	if got := w.file("comment-TH-5"); got != "Session 2 started\n\nTriggered by a question from item-1-lead.\n" {
		t.Errorf("comment = %q", got)
	}
	prompt := w.file("argv")
	for _, want := range []string{"[FROM: item-1-lead]\nYou are a thread agent", "## Earlier sessions on this thread",
		"## The message\n\nQuestion from item-1-lead for the people in thread " + threadKey, "Which month?\n"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt = %q, want %q in it", prompt, want)
		}
	}
	if got := questions(w); got != "item-1 "+threadKey+" item-1-lead plain pending" {
		t.Errorf("questions = %q", got)
	}
}

// A start from a lead's question, killed by the fake at `pane rename`, is
// finished by the next ask-human with the question still headed by the
// lead, not by inbox.
func TestAskHumanFinishesAStartItsEarlierRunWasKilledIn(t *testing.T) {
	w := threadWorld(t, "")
	openJobWithHomeThread(w, true)
	if out := w.inbox(w.event("m1", "start the import", "ctx")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	conn := w.defaultLedger()
	if _, err := conn.Exec("UPDATE agents SET state = 'ended' WHERE role = 'thread'"); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	for _, marker := range []string{"threads-workspace", "kill-at-rename"} {
		if err := os.WriteFile(filepath.Join(w.dir, marker), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Remove(filepath.Join(w.dir, "argv"))
	out := w.askHuman("--file", task(w, "q.md", "Which month? 0xQ1\n"))
	if out.code == 0 || w.file("argv") != "" {
		t.Fatalf("the killed run finished: %+v, argv %q", out, w.file("argv"))
	}
	if got := threadAgentRow(w); got != "TH-5 starting" {
		t.Fatalf("row after the kill = %q", got)
	}
	out = w.askHuman("--file", task(w, "q.md", "Which month? 0xQ1\n"))
	if out.code != 0 || !strings.Contains(out.stderr, "still starting from an earlier run") {
		t.Fatalf("retry: %+v", out)
	}
	prompt := w.file("argv")
	for _, want := range []string{"[FROM: item-1-lead]\nYou are a thread agent", "Question from item-1-lead for the people in thread " + threadKey, "0xQ1"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("retry prompt = %q, want %q in it", prompt, want)
		}
	}
	if strings.HasPrefix(prompt, "agent\nprompt\nthread-c0123-1700000000-123\n[FROM: inbox]") {
		t.Errorf("retry prompt headed by inbox: %q", prompt)
	}
	if got := threadAgentRow(w); got != "TH-5 active" {
		t.Errorf("row after the retry = %q", got)
	}
	if got := questions(w); got != "item-1 "+threadKey+" item-1-lead plain pending\nitem-1 "+threadKey+" item-1-lead plain pending" {
		t.Errorf("questions = %q", got)
	}
}

func TestAskHumanRefusalsAndLinearDown(t *testing.T) {
	w := threadWorld(t, "")
	question := task(w, "q.md", "Which month?\n")
	out := w.askHuman("--file", question)
	if out.code != 1 || !strings.Contains(out.stderr, "not open") {
		t.Errorf("no job: %+v", out)
	}
	openJobWithHomeThread(w, false)
	out = w.askHuman("--file", question)
	if out.code != 1 || !strings.Contains(out.stderr, "no home thread") {
		t.Errorf("no home thread: %+v", out)
	}
	for _, c := range []struct {
		label string
		out   result
	}{
		{"a worker", w.asAgent("item-1-a", "worker", "item-1-lead", "item-1", "ask-human", "--file", question)},
		{"an empty file", w.askHuman("--file", task(w, "e.md", " \n"))},
		{"a missing file", w.askHuman("--file", filepath.Join(w.dir, "no.md"))},
		{"no --file", w.askHuman()},
		{"a thread agent without FLEET_THREAD", w.asThread("ask-human", "--file", question)},
	} {
		if c.out.code != 1 {
			t.Errorf("%s: %+v", c.label, c.out)
		}
	}
	if got := questions(w); got != "" {
		t.Errorf("refusals recorded questions: %q", got)
	}
	// A thread agent records its own question and posts it itself.
	out = w.asThreadAgent("TH-5", "ask-human", "--file", question)
	if out.code != 0 || !strings.Contains(out.stdout, "recorded a pending question") || w.calls() != "" {
		t.Errorf("thread agent: %+v, calls %q", out, w.calls())
	}
	if got := questions(w); got != " "+threadKey+" thread-c0123-1700000000-123 plain pending" {
		t.Errorf("questions = %q", got)
	}

	// A lead whose home thread needs a new agent while Linear is down:
	// exit 5, the question pending, nothing posted by fleet.
	w2 := threadWorld(t, "claim")
	openJobWithHomeThread(w2, true)
	conn := w2.defaultLedger()
	if _, err := conn.Exec("INSERT INTO threads (thread, slug, channel, ticket, ticket_url, sessions, created_at) "+
		"VALUES (?1, 'c0123-1700000000-123', 'C0123', 'TH-5', 'https://linear.example.test/TH-5', 1, 0)", threadKey); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	out = w2.askHuman("--file", task(w2, "q.md", "Which month?\n"))
	if out.code != 5 || !strings.Contains(out.stderr, "Linear is unavailable") {
		t.Errorf("Linear down: %+v", out)
	}
	if strings.Contains(w2.calls(), "fednet") || strings.Contains(w2.calls(), "agent start") {
		t.Errorf("calls = %q", w2.calls())
	}
	if got := questions(w2); got != "item-1 "+threadKey+" item-1-lead plain pending" {
		t.Errorf("questions = %q", got)
	}
}

func TestJobEndTakesTheConclusionToTheHomeThread(t *testing.T) {
	w := threadWorld(t, "")
	openJobWithHomeThread(w, true)
	if out := w.inbox(w.event("m1", "start the import", "")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if err := os.WriteFile(filepath.Join(w.dir, "has-thread-c0123-1700000000-123"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	report := task(w, "report.md", "All rows imported.\n")
	out := w.run("", []string{"job", "end", "--report-file", report},
		"FLEET_AGENT=item-1-lead", "FLEET_ROLE=lead", "FLEET_PARENT=thread-x", "FLEET_TARGET=default", "FLEET_JOB=item-1")
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if !strings.Contains(out.stdout, "Job item-1 ended: done.") {
		t.Errorf("stdout = %q", out.stdout)
	}
	argv := w.file("argv")
	if !strings.HasPrefix(argv, "agent\nprompt\nthread-c0123-1700000000-123\n[FROM: item-1-lead]\nConclusion of a job from item-1-lead for the people in thread "+
		threadKey+". Post it to the thread with `fednet client post`; nothing is waiting for an answer.\n\nJob item-1 ended: done.\n") {
		t.Errorf("argv = %q", argv)
	}
	if got := questions(w); got != "" {
		t.Errorf("a conclusion was recorded as a question: %q", got)
	}
	calls := w.calls()
	if strings.Index(calls, "herdr agent prompt") > strings.Index(calls, "herdr workspace close") {
		t.Errorf("the workspace was closed before the conclusion was delivered: %q", calls)
	}
	// A job without a home thread only prints.
	w2 := threadWorld(t, "")
	openJobWithHomeThread(w2, false)
	out = w2.run("", []string{"job", "end", "--report-file", task(w2, "r.md", "done\n")},
		"FLEET_AGENT=item-1-lead", "FLEET_ROLE=lead", "FLEET_PARENT=thread-x", "FLEET_TARGET=default", "FLEET_JOB=item-1")
	if out.code != 0 || !strings.Contains(out.stdout, "Job item-1 ended: done.") || strings.Contains(w2.calls(), "agent prompt") {
		t.Errorf("no home thread: %+v, calls %q", out, w2.calls())
	}
}

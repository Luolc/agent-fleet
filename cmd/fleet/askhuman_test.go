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
		"FLEET_AGENT=item-1-lead", "FLEET_ROLE=lead", "FLEET_PARENT=thread-x", "FLEET_SCOPE=main", "FLEET_JOB=item-1", linearKey)
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
	if got := strings.TrimSpace(w.calls()); got != "fednet client post -socket /run/fednet.sock -thread "+threadKey+
		" -- Which month: September or October?\nherdr status server\nherdr agent get\nherdr agent prompt" {
		t.Errorf("calls = %q", got)
	}
	argv := w.file("argv")
	if !strings.HasPrefix(argv, "agent\nprompt\nthread-c0123-1700000000-123\n[FROM: item-1-lead]\nQuestion from item-1-lead for the people in thread "+
		threadKey+", already posted there by fleet. Your prompt says what to do under \"Messages from fleet\"; at the least, "+
		"when the people answer, pass the answer on with `fleet send item-1-lead --file <file>`.\n\n"+
		"Which month: September or October?\n") {
		t.Errorf("argv = %q", argv)
	}
	if got := questions(w); got != "item-1 "+threadKey+" item-1-lead plain pending" {
		t.Errorf("questions = %q", got)
	}
	// The same question again while pending: done already, nothing repeated.
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	if out := w.askHuman("--file", question); out.code != 0 || w.calls() != "" ||
		!strings.Contains(out.stdout, "already posted to thread "+threadKey+" and delivered to its agent; nothing done") {
		t.Errorf("again: %+v, calls %q", out, w.calls())
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
		"## The message\n\nQuestion from item-1-lead for the people in thread " + threadKey, "Which month?\n",
		"A lead's question waits for a person only when the rules the lead works under need a person's yes for it;",
		"tell the lead with `fleet send` to decide it itself, naming the rule",
		"the question stays pending until a person next writes here",
		"tell each open lead `fleet status` lists with PARENT thread-c0123-1700000000-123 what changed."} {
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
	if got := questions(w); got != "item-1 "+threadKey+" item-1-lead plain pending" {
		t.Errorf("questions = %q", got)
	}
	if got := strings.Count(w.calls(), "fednet client post"); got != 1 {
		t.Errorf("posted %d times, calls %q", got, w.calls())
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
	// A thread agent's own question is recorded and posted to its thread.
	out = w.asThreadAgent("TH-5", "ask-human", "--file", question)
	if out.code != 0 || !strings.Contains(out.stdout, "posted a pending question") ||
		w.calls() != "fednet client post -socket /run/fednet.sock -thread "+threadKey+" -- Which month?\n" {
		t.Errorf("thread agent: %+v, calls %q", out, w.calls())
	}
	if got := questions(w); got != " "+threadKey+" thread-c0123-1700000000-123 plain pending" {
		t.Errorf("questions = %q", got)
	}
	// Only the end of the question is trimmed; asking again does nothing.
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	indented := task(w, "i.md", "    SELECT 1;\nIs this right?\n\n")
	if out := w.asThreadAgent("TH-5", "ask-human", "--file", indented); out.code != 0 ||
		w.calls() != "fednet client post -socket /run/fednet.sock -thread "+threadKey+" --     SELECT 1;\nIs this right?\n" {
		t.Errorf("indented: %+v, calls %q", out, w.calls())
	}
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	if out := w.asThreadAgent("TH-5", "ask-human", "--file", indented); out.code != 0 || w.calls() != "" ||
		!strings.Contains(out.stdout, "already posted to thread "+threadKey+"; nothing done") {
		t.Errorf("indented again: %+v, calls %q", out, w.calls())
	}

	// A lead whose home thread needs a new agent while Linear is down: the
	// agent starts without its ticket and gets the question, the thread is
	// told.
	w2 := threadWorld(t, "claim")
	openJobWithHomeThread(w2, true)
	conn := w2.defaultLedger()
	if _, err := conn.Exec("INSERT INTO threads (thread, slug, channel, ticket, ticket_url, mapping, cwd, sessions, created_at) "+
		"VALUES (?1, 'c0123-1700000000-123', 'C0123', 'TH-5', 'https://linear.example.test/TH-5', 'repo-example-dataset', ?2, 1, 0)",
		threadKey, filepath.Join(w2.dir, "home", "dev", "example-dataset")); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	out = w2.askHuman("--file", task(w2, "q.md", "Which month?\n"))
	if out.code != 0 {
		t.Errorf("Linear down: %+v", out)
	}
	calls := w2.calls()
	if !strings.Contains(calls, "-- Which month?") || !strings.Contains(calls, "agent start") ||
		!strings.Contains(calls, "-- Linear is unavailable (atb linear claim TH-5: exit 4); this session runs without its ticket.") {
		t.Errorf("calls = %q", calls)
	}
	if prompt := w2.file("argv"); !strings.Contains(prompt, "FLEET_ISSUE= (empty: this session runs without its thread ticket") ||
		!strings.Contains(prompt, "Question from item-1-lead") {
		t.Errorf("prompt = %q", prompt)
	}
	if got := questions(w2); got != "item-1 "+threadKey+" item-1-lead plain pending" {
		t.Errorf("questions = %q", got)
	}
}

// A post that fails leaves the question recorded and undelivered; asking
// again posts it once and delivers it, and a third run repeats nothing.
func TestAskHumanContinuesAfterAFailedPost(t *testing.T) {
	w := threadWorld(t, "")
	openJobWithHomeThread(w, true)
	if out := w.inbox(w.event("m1", "start the import", "")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if err := os.WriteFile(filepath.Join(w.dir, "fednet-down"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	_ = os.Remove(filepath.Join(w.dir, "argv"))
	question := task(w, "q.md", "Which month? 0xQ2\n")
	out := w.askHuman("--file", question)
	if out.code != 5 || out.stderr != "fleet: fednet client post failed (exit status: 4); the question is recorded, run ask-human again to post it\n" {
		t.Errorf("failed post: %+v", out)
	}
	if strings.Contains(w.calls(), "agent prompt") {
		t.Errorf("delivered after a failed post: %q", w.calls())
	}
	_ = os.Remove(filepath.Join(w.dir, "fednet-down"))
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	if out := w.askHuman("--file", question); out.code != 0 {
		t.Fatalf("retry: %+v", out)
	}
	if got := strings.TrimSpace(w.calls()); got != "fednet client post -socket /run/fednet.sock -thread "+threadKey+
		" -- Which month? 0xQ2\nherdr status server\nherdr agent get\nherdr agent prompt" {
		t.Errorf("retry calls = %q", got)
	}
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	if out := w.askHuman("--file", question); out.code != 0 || w.calls() != "" {
		t.Errorf("third run: %+v, calls %q", out, w.calls())
	}
	if got := questions(w); got != "item-1 "+threadKey+" item-1-lead plain pending" {
		t.Errorf("questions = %q", got)
	}
	// Without a socket nothing is recorded.
	w.scopeConfig("main", `{"linear": {"team": "TH"}}`)
	out = w.askHuman("--file", task(w, "q2.md", "Another?\n"))
	if out.code != 1 || !strings.Contains(out.stderr, "fednet.socket is not configured") {
		t.Errorf("no socket: %+v", out)
	}
	if got := questions(w); got != "item-1 "+threadKey+" item-1-lead plain pending" {
		t.Errorf("questions after no socket = %q", got)
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
		"FLEET_AGENT=item-1-lead", "FLEET_ROLE=lead", "FLEET_PARENT=thread-x", "FLEET_SCOPE=main", "FLEET_JOB=item-1")
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if !strings.Contains(out.stdout, "Job item-1 ended: done.") {
		t.Errorf("stdout = %q", out.stdout)
	}
	argv := w.file("argv")
	if !strings.HasPrefix(argv, "agent\nprompt\nthread-c0123-1700000000-123\n[FROM: item-1-lead]\nConclusion of a job from item-1-lead for the people in thread "+
		threadKey+". Your prompt says what to do under \"Messages from fleet\"; at the least, post it to the thread with "+
		"`fleet thread post`.\n\nJob item-1 ended: done.\n") {
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
		"FLEET_AGENT=item-1-lead", "FLEET_ROLE=lead", "FLEET_PARENT=thread-x", "FLEET_SCOPE=main", "FLEET_JOB=item-1")
	if out.code != 0 || !strings.Contains(out.stdout, "Job item-1 ended: done.") || strings.Contains(w2.calls(), "agent prompt") {
		t.Errorf("no home thread: %+v, calls %q", out, w2.calls())
	}
}

// openSingleRepoJob puts job item-1 (repo example-dataset, run in its
// checkout, Linear off) in the default ledger as open with no home thread
// and its lead live, as a start by a caller without FLEET_ROLE leaves it.
func openSingleRepoJob(w *world) {
	w.t.Helper()
	conn := w.defaultLedger()
	defer conn.Close()
	checkout := filepath.Join(w.dir, "home", "dev", "example-dataset")
	for _, stmt := range []string{
		"INSERT INTO jobs (job, repo, lead_cwd, state, started_at) VALUES ('item-1', 'example-dataset', ?1, 'open', 0)",
		"INSERT INTO agents (name, role, job, parent, state, started_at) VALUES ('item-1-lead', 'lead', 'item-1', 'fleet', 'active', 0)",
	} {
		if _, err := conn.Exec(stmt, checkout); err != nil {
			w.t.Fatal(err)
		}
	}
}

// homeThread is the home thread of job item-1 and the mapping and cwd of
// that thread's row, space-separated.
func homeThread(w *world) string {
	w.t.Helper()
	conn := w.defaultLedger()
	defer conn.Close()
	var thread, mapping, cwd string
	if err := conn.QueryRow("SELECT j.home_thread, COALESCE(t.mapping, ''), COALESCE(t.cwd, '') FROM jobs j "+
		"LEFT JOIN threads t ON t.thread = j.home_thread WHERE j.job = 'item-1'").Scan(&thread, &mapping, &cwd); err != nil {
		w.t.Fatal(err)
	}
	return strings.TrimSpace(strings.Join([]string{thread, mapping, cwd}, " "))
}

func TestAskHumanOpensAHomeThreadInTheReposChannel(t *testing.T) {
	w := threadWorld(t, "")
	openSingleRepoJob(w)
	question := task(w, "q.md", "Revert the red commit or fix it?\n")
	// No thread of the repo's channel or the general one, and fednet's
	// routes (read once) name neither: fleet does not know their ids.
	out := w.askHuman("--file", question)
	if out.code != 1 || !strings.Contains(out.stderr, "no thread of #repo-example-dataset or #x-repo-general has reached this scope") ||
		w.calls() != "fednet client routes -socket /run/fednet.sock -json\n" || questions(w) != "" || homeThread(w) != "" {
		t.Fatalf("unknown channels: %+v, calls %q", out, w.calls())
	}
	// A message in the repo's channel makes its id known.
	if out := w.inbox(w.event("m1", "hello", "")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if err := os.WriteFile(filepath.Join(w.dir, "fednet-down"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	out = w.askHuman("--file", question)
	if out.code != 5 || out.stderr != "fleet: fednet client open-thread failed (exit status: 4): post: hub unreachable; "+
		"nothing is recorded, run ask-human again\n" || questions(w) != "" || homeThread(w) != "" {
		t.Fatalf("failed open: %+v", out)
	}
	_ = os.Remove(filepath.Join(w.dir, "fednet-down"))
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	out = w.askHuman("--file", question)
	if out.code != 0 || !strings.Contains(out.stdout, "opened thread C0123/1700000099.000 as the home thread of job item-1") {
		t.Fatalf("%+v", out)
	}
	calls := strings.Split(w.calls(), "\n")
	if calls[0] != "fednet client open-thread -socket /run/fednet.sock -channel C0123 -- Questions from job item-1 in "+
		"example-dataset, which started without a thread; fleet posts them below." ||
		calls[1] != "fednet client post -socket /run/fednet.sock -thread C0123/1700000099.000 -- Revert the red commit or fix it?" {
		t.Errorf("calls = %q", calls)
	}
	checkout := filepath.Join(w.dir, "home", "dev", "example-dataset")
	if got := homeThread(w); got != "C0123/1700000099.000 repo-example-dataset "+checkout {
		t.Errorf("home thread = %q", got)
	}
	if got := questions(w); got != "item-1 C0123/1700000099.000 item-1-lead plain pending" {
		t.Errorf("questions = %q", got)
	}
	if argv := w.file("argv"); !strings.Contains(argv, "[FROM: item-1-lead]\nYou are a thread agent") ||
		!strings.Contains(argv, "Question from item-1-lead for the people in thread C0123/1700000099.000") {
		t.Errorf("argv = %q", argv)
	}
	if got := w.file("description"); !strings.HasPrefix(got, "channel: C0123\nthread: C0123/1700000099.000\n"+
		"first message, a question from item-1-lead:\n\nRevert the red commit or fix it?") {
		t.Errorf("ticket description = %q", got)
	}
	// Asked again: the home thread is there, nothing is opened or posted.
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	if out := w.askHuman("--file", question); out.code != 0 || w.calls() != "" {
		t.Errorf("again: %+v, calls %q", out, w.calls())
	}
}

func TestAskHumanFallsBackToTheGeneralChannel(t *testing.T) {
	w := threadWorld(t, "")
	openSingleRepoJob(w)
	general := dir(w, filepath.Join("home", "x-repo", "general"))
	if out := w.inbox(w.eventIn("m1", "hello", "", `"channel_name":"x-repo-general"`)); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	// A direct message has the general mapping too, but no thread opens there.
	conn := w.defaultLedger()
	if _, err := conn.Exec("INSERT INTO threads (thread, slug, channel, mapping, cwd, created_at) " +
		"VALUES ('D0456/1700000050.000', 'd0456', 'D0456', 'x-repo-general', '', 0)"); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	// A fednet whose routes carry no names, as before it had them.
	task(w, "routes.json", `{"routes":[{"channel":"C0999","machine":"workstation"},{"channel":"dm","machine":"workstation"}]}`)
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	out := w.askHuman("--file", task(w, "q.md", "main of example-dataset is red: revert?\n"))
	if calls := strings.Split(w.calls(), "\n"); out.code != 0 || calls[0] != "fednet client routes -socket /run/fednet.sock -json" ||
		!strings.HasPrefix(calls[1], "fednet client open-thread -socket /run/fednet.sock -channel C0123 --") {
		t.Fatalf("%+v, calls %q", out, w.calls())
	}
	if got := homeThread(w); got != "C0123/1700000099.000 x-repo-general "+general {
		t.Errorf("home thread = %q", got)
	}
}

func TestAskHumanTakesTheReposChannelFromFednetsRoutes(t *testing.T) {
	w := threadWorld(t, "")
	openSingleRepoJob(w)
	// No thread of either channel has reached the scope; the routes name
	// the repo's channel, after a direct message's id with that name.
	task(w, "routes.json", `{"routes":[{"channel":"dm","machine":"workstation"},`+
		`{"channel":"D0456","name":"repo-example-dataset"},`+
		`{"channel":"C0789","machine":"workstation","scope":"main","name":"repo-example-dataset"},`+
		`{"channel":"C0123","name":"x-repo-general"}]}`)
	out := w.askHuman("--file", task(w, "q.md", "Revert the red commit or fix it?\n"))
	if out.code != 0 || !strings.Contains(out.stdout, "opened thread C0789/1700000099.000 as the home thread of job item-1") {
		t.Fatalf("%+v, calls %q", out, w.calls())
	}
	calls := strings.Split(w.calls(), "\n")
	if calls[0] != "fednet client routes -socket /run/fednet.sock -json" ||
		!strings.HasPrefix(calls[1], "fednet client open-thread -socket /run/fednet.sock -channel C0789 --") {
		t.Errorf("calls = %q", calls)
	}
	checkout := filepath.Join(w.dir, "home", "dev", "example-dataset")
	if got := homeThread(w); got != "C0789/1700000099.000 repo-example-dataset "+checkout {
		t.Errorf("home thread = %q", got)
	}
	// The thread is in the ledger now: the next open takes the id from
	// there and reads no routes. (The fake opens every thread with the
	// same key, so the first one's is changed.)
	conn := w.defaultLedger()
	for _, stmt := range []string{"UPDATE jobs SET home_thread = '' WHERE job = 'item-1'",
		"UPDATE threads SET thread = 'C0789/1700000001.000' WHERE thread = 'C0789/1700000099.000'"} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	conn.Close()
	_ = os.Remove(filepath.Join(w.dir, "routes.json"))
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	out = w.askHuman("--file", task(w, "q2.md", "And the other commit?\n"))
	if out.code != 0 || !strings.HasPrefix(w.calls(), "fednet client open-thread -socket /run/fednet.sock -channel C0789 --") {
		t.Fatalf("again: %+v, calls %q", out, w.calls())
	}
}

func TestAskHumanTakesTheGeneralChannelFromFednetsRoutes(t *testing.T) {
	general := ""
	for name, routes := range map[string]string{
		// The routes name the general channel, not the repo's.
		"named": `{"routes":[{"channel":"C0999","name":"repo-other"},{"channel":"C0123","name":"x-repo-general"}]}`,
		// Routes fednet cannot give name no channel; the ledger's id of the
		// general channel is taken.
		"unreadable": "not json",
	} {
		t.Run(name, func(t *testing.T) {
			w := threadWorld(t, "")
			openSingleRepoJob(w)
			general = dir(w, filepath.Join("home", "x-repo", "general"))
			if name == "unreadable" {
				if out := w.inbox(w.eventIn("m1", "hello", "", `"channel_name":"x-repo-general"`)); out.code != 0 {
					t.Fatalf("%+v", out)
				}
			}
			task(w, "routes.json", routes)
			_ = os.Remove(filepath.Join(w.dir, "calls"))
			out := w.askHuman("--file", task(w, "q.md", "main of example-dataset is red: revert?\n"))
			if out.code != 0 || !strings.Contains(w.calls(), "fednet client open-thread -socket /run/fednet.sock -channel C0123 --") {
				t.Fatalf("%+v, calls %q", out, w.calls())
			}
			if got := homeThread(w); got != "C0123/1700000099.000 x-repo-general "+general {
				t.Errorf("home thread = %q", got)
			}
			if name == "unreadable" && !strings.Contains(out.stderr, "fleet: fednet client routes: not a JSON reply") {
				t.Errorf("stderr = %q", out.stderr)
			}
		})
	}
}

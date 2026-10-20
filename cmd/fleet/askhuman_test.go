// Integration tests for `ask-human`, `ask-thread` and the answers `inbox` takes,
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

func (w *world) askThread(args ...string) result {
	w.t.Helper()
	return w.run("", append([]string{"ask-thread"}, args...),
		"FLEET_AGENT=item-1-lead", "FLEET_ROLE=lead", "FLEET_PARENT=thread-x", "FLEET_SCOPE=main", "FLEET_JOB=item-1", linearKey)
}

// questions are the rows of the questions table: job, thread, asked_by,
// card or plain, state, one line each.
func questions(w *world) string {
	w.t.Helper()
	conn := w.defaultLedger()
	defer conn.Close()
	rows, err := conn.Query("SELECT job, thread, asked_by, card, state FROM questions ORDER BY id")
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var job, thread, by, state string
		var card int
		if err := rows.Scan(&job, &thread, &by, &card, &state); err != nil {
			w.t.Fatal(err)
		}
		lines = append(lines, strings.Join([]string{job, thread, by, map[int]string{0: "plain", 1: "card"}[card], state}, " "))
	}
	return strings.Join(lines, "\n")
}

// A lead's question reaches the live thread agent; a person's message in
// the thread does not answer it.
func TestAskThreadReachesTheLiveThreadAgent(t *testing.T) {
	w := threadWorld(t, "")
	openJobWithHomeThread(w, true)
	if out := w.inbox(w.event("m1", "start the import", "")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	question := task(w, "q.md", "Which month: September or October?\n")
	out := w.askThread("--card", question)
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	// fleet posts nothing: the thread agent asks the people.
	if got := strings.TrimSpace(w.calls()); got != "herdr status server\nherdr agent get\nherdr agent prompt" {
		t.Errorf("calls = %q", got)
	}
	if !strings.Contains(out.stdout, "gave question #1 to the agent of thread "+threadKey) {
		t.Errorf("stdout = %q", out.stdout)
	}
	argv := w.file("argv")
	if !strings.HasPrefix(argv, "agent\nprompt\nthread-c0123-1700000000-123\n[FROM: item-1-lead]\n[QUESTION #1 from item-1-lead]\n"+
		"Yours to settle, in thread "+threadKey+"; fleet has posted nothing. The user-level skill `fleet` "+
		"(`references/thread.md`) says what to do with it; at the least, answer it with `fleet send item-1-lead --file <file>`, or ask the people "+
		"with `fleet ask-human --card <file> --for 1` (this card or one of your own), whose answers reach item-1-lead "+
		"too.\n\nWhich month: September or October?\n") {
		t.Errorf("argv = %q", argv)
	}
	if got := questions(w); got != "item-1 "+threadKey+" item-1-lead card pending" {
		t.Errorf("questions = %q", got)
	}
	// The same question again while pending: done already, nothing repeated.
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	if out := w.askThread("--card", question); out.code != 0 || w.calls() != "" ||
		!strings.Contains(out.stdout, "already delivered to the agent of thread "+threadKey+"; nothing done") {
		t.Errorf("again: %+v, calls %q", out, w.calls())
	}
	out = w.inbox(w.event("m2", "October", ""))
	if out.code != 0 || strings.Contains(out.stdout, "answered") {
		t.Errorf("message: %+v", out)
	}
	if got := questions(w); got != "item-1 "+threadKey+" item-1-lead card pending" {
		t.Errorf("questions = %q", got)
	}
}

// The thread agent's message to the lead settles the lead's questions in
// its thread; its own stay pending until a person writes there.
func TestAskHumanIsAnsweredByTheThreadAgentsMessageToTheLead(t *testing.T) {
	w := threadWorld(t, "")
	openJobWithHomeThread(w, true)
	if out := w.inbox(w.event("m1", "start the import", "")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if out := w.askThread("--card", task(w, "q.md", "Which month?\n")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if out := w.asThreadAgent("TH-5", "ask-human", "--card", task(w, "o.json", `{"question": "Which bucket?"}`)); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	out := w.asThreadAgent("TH-5", "send", "item-1-lead", "--file", task(w, "a.md", "October, as the people said.\n"))
	if out.code != 0 || !strings.Contains(out.stdout, "1 pending question(s) of item-1-lead in thread "+threadKey+" answered") {
		t.Errorf("send: %+v", out)
	}
	if got := questions(w); got != "item-1 "+threadKey+" item-1-lead card answered\n "+threadKey+
		" thread-c0123-1700000000-123 card pending" {
		t.Errorf("questions = %q", got)
	}
}

func TestAskThreadStartsAThreadAgentWhenTheHomeThreadHasNone(t *testing.T) {
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
	out := w.askThread("--card", task(w, "q.md", "Which month?\n"))
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
		"## The message\n\n[QUESTION #1 from item-1-lead]\nYours to settle, in thread " + threadKey, "Which month?\n",
		"A lead's question (`[QUESTION #<id> from <lead>]`, a card's JSON) is yours to settle: fleet has recorded it as pending and posted nothing",
		"`fleet send <lead> --file <file>`, which settles it"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt = %q, want %q in it", prompt, want)
		}
	}
	if got := questions(w); got != "item-1 "+threadKey+" item-1-lead card pending" {
		t.Errorf("questions = %q", got)
	}
	if strings.Contains(calls, "fednet client post") {
		t.Errorf("fleet posted the lead's question: %q", calls)
	}
}

// A start from a lead's question, killed by the fake at `pane rename`, is
// finished by the next ask-thread with the question still headed by the
// lead, not by inbox.
func TestAskThreadFinishesAStartItsEarlierRunWasKilledIn(t *testing.T) {
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
	out := w.askThread("--card", task(w, "q.md", "Which month? 0xQ1\n"))
	if out.code == 0 || w.file("argv") != "" {
		t.Fatalf("the killed run finished: %+v, argv %q", out, w.file("argv"))
	}
	if got := threadAgentRow(w); got != "TH-5 starting" {
		t.Fatalf("row after the kill = %q", got)
	}
	out = w.askThread("--card", task(w, "q.md", "Which month? 0xQ1\n"))
	if out.code != 0 || !strings.Contains(out.stderr, "still starting from an earlier run") {
		t.Fatalf("retry: %+v", out)
	}
	prompt := w.file("argv")
	for _, want := range []string{"[FROM: item-1-lead]\nYou are a thread agent", "[QUESTION #1 from item-1-lead]\nYours to settle, in thread " + threadKey, "0xQ1"} {
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
	if got := questions(w); got != "item-1 "+threadKey+" item-1-lead card pending" {
		t.Errorf("questions = %q", got)
	}
	if got := strings.Count(w.calls(), "fednet client post"); got != 0 {
		t.Errorf("posted %d times, calls %q", got, w.calls())
	}
}

func TestAskThreadRefusalsAndLinearDown(t *testing.T) {
	w := threadWorld(t, "")
	question := task(w, "q.md", "Which month?\n")
	out := w.askThread("--card", question)
	if out.code != 1 || !strings.Contains(out.stderr, "not open") {
		t.Errorf("no job: %+v", out)
	}
	openJobWithHomeThread(w, false)
	out = w.askThread("--card", question)
	if out.code != 1 || !strings.Contains(out.stderr, "no home thread") {
		t.Errorf("no home thread: %+v", out)
	}
	for _, c := range []struct {
		label string
		out   result
	}{
		{"a worker", w.asAgent("item-1-a", "worker", "item-1-lead", "item-1", "ask-human", "--file", question)},
		{"an empty file", w.askThread("--card", task(w, "e.md", " \n"))},
		{"a missing file", w.askThread("--card", filepath.Join(w.dir, "no.md"))},
		{"no --card", w.askThread()},
	} {
		if c.out.code != 1 {
			t.Errorf("%s: %+v", c.label, c.out)
		}
	}
	if got := questions(w); got != "" {
		t.Errorf("refusals recorded questions: %q", got)
	}
	// A thread agent asks with ask-human, not ask-thread.
	if out := w.asThreadAgent("TH-5", "ask-thread", "--card", question); out.code != 1 ||
		!strings.Contains(out.stderr, "fleet ask-human --card") {
		t.Errorf("thread agent: %+v", out)
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
	out = w2.askThread("--card", task(w2, "q.md", "Which month?\n"))
	if out.code != 0 {
		t.Errorf("Linear down: %+v", out)
	}
	calls := w2.calls()
	if strings.Contains(calls, "-- Which month?") || !strings.Contains(calls, "agent start") ||
		!strings.Contains(calls, "-- Linear is unavailable (atb linear claim TH-5: exit 4); this session runs without its ticket.") {
		t.Errorf("calls = %q", calls)
	}
	if prompt := w2.file("argv"); !strings.Contains(prompt, "FLEET_ISSUE= (empty: this session runs without its thread ticket") ||
		!strings.Contains(prompt, "[QUESTION #1 from item-1-lead]") || !strings.Contains(prompt, "Which month?") {
		t.Errorf("prompt = %q", prompt)
	}
	if got := questions(w2); got != "item-1 "+threadKey+" item-1-lead card pending" {
		t.Errorf("questions = %q", got)
	}
}

// A thread agent's card fednet cannot post leaves its question pending;
// asking again posts it once, and a third run repeats nothing.
func TestAskHumanContinuesAfterAFailedAsk(t *testing.T) {
	w := threadWorld(t, "")
	if out := w.inbox(w.event("m1", "start the import", "")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if err := os.WriteFile(filepath.Join(w.dir, "fednet-down"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	card := task(w, "q.json", `{"question": "Which month? 0xQ2"}`)
	out := w.asThreadAgent("TH-5", "ask-human", "--card", card)
	if out.code != 4 || out.stderr != "post: hub unreachable\nfleet: question #1 is recorded, run ask-human again to post it\n" {
		t.Errorf("failed ask: %+v", out)
	}
	_ = os.Remove(filepath.Join(w.dir, "fednet-down"))
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	out = w.asThreadAgent("TH-5", "ask-human", "--card", card)
	if out.code != 0 || out.stdout != "asked question #1 in thread "+threadKey+": https://slack.example.test/card-1\n" {
		t.Fatalf("retry: %+v", out)
	}
	if got := w.calls(); got != "fednet client ask -socket /run/fednet.sock -thread "+threadKey+" -ref main:1 -card "+card+" -json\n" {
		t.Errorf("retry calls = %q", got)
	}
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	if out := w.asThreadAgent("TH-5", "ask-human", "--card", card); out.code != 0 || w.calls() != "" ||
		!strings.Contains(out.stdout, "question #1 was already asked in thread "+threadKey+"; nothing done") {
		t.Errorf("third run: %+v, calls %q", out, w.calls())
	}
	if got := questions(w); got != " "+threadKey+" thread-c0123-1700000000-123 card pending" {
		t.Errorf("questions = %q", got)
	}
	// A card fednet refuses: nothing posted, the question closed.
	if err := os.WriteFile(filepath.Join(w.dir, "card-refused"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out = w.asThreadAgent("TH-5", "ask-human", "--card", task(w, "big.json", `{"question": "Too many?"}`))
	if out.code != 2 || !strings.Contains(out.stderr, "options: 6, over the limit of 5") ||
		!strings.Contains(out.stderr, "nothing was posted and question #2 is closed") {
		t.Errorf("refused: %+v", out)
	}
	if got := questions(w); got != " "+threadKey+" thread-c0123-1700000000-123 card pending\n "+threadKey+
		" thread-c0123-1700000000-123 card closed" {
		t.Errorf("questions after the refusal = %q", got)
	}
	// Without a socket nothing is recorded.
	w.scopeConfig("main", `{"linear": {"team": "TH"}}`)
	out = w.asThreadAgent("TH-5", "ask-human", "--card", task(w, "q2.json", `{"question": "Another?"}`))
	if out.code != 1 || !strings.Contains(out.stderr, "fednet.socket is not configured") {
		t.Errorf("no socket: %+v", out)
	}
	if got := strings.Count(questions(w), "\n"); got != 1 {
		t.Errorf("questions after no socket = %q", questions(w))
	}
}

func TestAskHumanRefusals(t *testing.T) {
	w := threadWorld(t, "")
	openJobWithHomeThread(w, true)
	card := task(w, "q.json", `{"question": "Which month?"}`)
	for _, c := range []struct {
		label, want string
		out         result
	}{
		{"a lead", "fleet ask-thread --card", w.run("", []string{"ask-human", "--card", card}, "FLEET_AGENT=item-1-lead",
			"FLEET_ROLE=lead", "FLEET_PARENT=thread-x", "FLEET_SCOPE=main", "FLEET_JOB=item-1")},
		{"a worker", "fleet send item-1-lead", w.asAgent("item-1-a", "worker", "item-1-lead", "item-1", "ask-human", "--card", card)},
		{"--file with --card", "exactly one of --card", w.asThreadAgent("TH-5", "ask-human", "--file", card, "--card", card)},
		{"--approval", "approval cards are not supported yet", w.asThreadAgent("TH-5", "ask-human", "--card", card, "--approval")},
		{"an empty card", "the card file is empty", w.asThreadAgent("TH-5", "ask-human", "--card", task(w, "e.json", " \n"))},
		{"neither --card nor --close", "exactly one of --card", w.asThreadAgent("TH-5", "ask-human")},
		{"--for without --card", "goes with --card", w.asThreadAgent("TH-5", "ask-human", "--close", "1", "--for", "1")},
		{"a --for that is no lead's question", "question #7 is not a lead's question pending",
			w.asThreadAgent("TH-5", "ask-human", "--card", card, "--for", "7")},
		{"a --close that is no card of the caller's", "question #7 is not a card of yours",
			w.asThreadAgent("TH-5", "ask-human", "--close", "7")},
		{"a thread agent without FLEET_THREAD", "FLEET_THREAD is not set", w.asThread("ask-human", "--card", card)},
	} {
		if c.out.code != 1 || !strings.Contains(c.out.stderr, c.want) {
			t.Errorf("%s: %+v", c.label, c.out)
		}
	}
	if got := questions(w); got != "" {
		t.Errorf("refusals recorded questions: %q", got)
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
		threadKey+". The user-level skill `fleet` (`references/thread.md`) says what to do with it; at the least, post it to the thread with "+
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

func TestAskThreadOpensAHomeThreadInTheReposChannel(t *testing.T) {
	w := threadWorld(t, "")
	openSingleRepoJob(w)
	question := task(w, "q.md", "Revert the red commit or fix it?\n")
	// No thread of the repo's channel or the general one, and fednet's
	// routes (read once) name neither: fleet does not know their ids.
	out := w.askThread("--card", question)
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
	out = w.askThread("--card", question)
	if out.code != 5 || out.stderr != "fleet: fednet client open-thread failed (exit status: 4): post: hub unreachable; "+
		"nothing is recorded, run ask-thread again\n" || questions(w) != "" || homeThread(w) != "" {
		t.Fatalf("failed open: %+v", out)
	}
	_ = os.Remove(filepath.Join(w.dir, "fednet-down"))
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	out = w.askThread("--card", question)
	if out.code != 0 || !strings.Contains(out.stdout, "opened thread C0123/1700000099.000 as the home thread of job item-1") {
		t.Fatalf("%+v", out)
	}
	// The thread opens with fleet's line, not the question, and nothing
	// more is posted: the thread agent asks.
	calls := strings.Split(w.calls(), "\n")
	if calls[0] != "fednet client open-thread -socket /run/fednet.sock -channel C0123 -- Thread of job item-1 in "+
		"example-dataset, which started without one." || strings.Contains(w.calls(), "fednet client post") {
		t.Errorf("calls = %q", calls)
	}
	checkout := filepath.Join(w.dir, "home", "dev", "example-dataset")
	if got := homeThread(w); got != "C0123/1700000099.000 repo-example-dataset "+checkout {
		t.Errorf("home thread = %q", got)
	}
	if got := questions(w); got != "item-1 C0123/1700000099.000 item-1-lead card pending" {
		t.Errorf("questions = %q", got)
	}
	if argv := w.file("argv"); !strings.Contains(argv, "[FROM: item-1-lead]\nYou are a thread agent") ||
		!strings.Contains(argv, "[QUESTION #1 from item-1-lead]\nYours to settle, in thread C0123/1700000099.000") {
		t.Errorf("argv = %q", argv)
	}
	if got := w.file("description"); !strings.HasPrefix(got, "channel: C0123\nthread: C0123/1700000099.000\n"+
		"first message, a question from item-1-lead:\n\nRevert the red commit or fix it?") {
		t.Errorf("ticket description = %q", got)
	}
	// Asked again: the home thread is there, nothing is opened or delivered.
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	if out := w.askThread("--card", question); out.code != 0 || w.calls() != "" {
		t.Errorf("again: %+v, calls %q", out, w.calls())
	}
}

func TestAskThreadFallsBackToTheGeneralChannel(t *testing.T) {
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
	out := w.askThread("--card", task(w, "q.md", "main of example-dataset is red: revert?\n"))
	if calls := strings.Split(w.calls(), "\n"); out.code != 0 || calls[0] != "fednet client routes -socket /run/fednet.sock -json" ||
		!strings.HasPrefix(calls[1], "fednet client open-thread -socket /run/fednet.sock -channel C0123 --") {
		t.Fatalf("%+v, calls %q", out, w.calls())
	}
	if got := homeThread(w); got != "C0123/1700000099.000 x-repo-general "+general {
		t.Errorf("home thread = %q", got)
	}
}

func TestAskThreadTakesTheReposChannelFromFednetsRoutes(t *testing.T) {
	w := threadWorld(t, "")
	openSingleRepoJob(w)
	// No thread of either channel has reached the scope; the routes name
	// the repo's channel, after a direct message's id with that name.
	task(w, "routes.json", `{"routes":[{"channel":"dm","machine":"workstation"},`+
		`{"channel":"D0456","name":"repo-example-dataset"},`+
		`{"channel":"C0789","machine":"workstation","scope":"main","name":"repo-example-dataset"},`+
		`{"channel":"C0123","name":"x-repo-general"}]}`)
	out := w.askThread("--card", task(w, "q.md", "Revert the red commit or fix it?\n"))
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
	out = w.askThread("--card", task(w, "q2.md", "And the other commit?\n"))
	if out.code != 0 || !strings.HasPrefix(w.calls(), "fednet client open-thread -socket /run/fednet.sock -channel C0789 --") {
		t.Fatalf("again: %+v, calls %q", out, w.calls())
	}
}

func TestAskThreadTakesTheGeneralChannelFromFednetsRoutes(t *testing.T) {
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
			out := w.askThread("--card", task(w, "q.md", "main of example-dataset is red: revert?\n"))
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

// answerEvent writes the event fednet's hook gives for a press on a card.
func (w *world) answerEvent(msgID, answer string) string {
	w.t.Helper()
	return task(w, msgID+".json", `{"msg_id":"`+msgID+`","payload":{"type":"answer","answer":`+answer+`}}`)
}

// question is the state, outcome and snooze_until of question `id`.
func question(w *world, id int) string {
	w.t.Helper()
	conn := w.defaultLedger()
	defer conn.Close()
	var state, outcome, until string
	if err := conn.QueryRow("SELECT state, outcome, COALESCE(snooze_until, '') FROM questions WHERE id = ?1", id).
		Scan(&state, &outcome, &until); err != nil {
		w.t.Fatal(err)
	}
	return strings.TrimSpace(strings.Join([]string{state, outcome, until}, " "))
}

// A lead's question the thread agent asks the people for: the card's
// presses reach the thread agent and the lead, each under its own id;
// the thread agent's messages to the lead and the people's messages in
// the thread answer neither.
func TestAnAnswerReachesTheThreadAgentAndTheLead(t *testing.T) {
	w := threadWorld(t, "")
	openJobWithHomeThread(w, true)
	if out := w.inbox(w.event("m1", "start the import", "")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	card := task(w, "q.json", `{"question": "Which month?", "options": [{"short": "September"}, {"short": "October"}]}`)
	if out := w.askThread("--card", card); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	out := w.asThreadAgent("TH-5", "ask-human", "--card", card, "--for", "1")
	if out.code != 0 || out.stdout != "asked question #2 in thread "+threadKey+": https://slack.example.test/card-2\n" ||
		w.calls() != "fednet client ask -socket /run/fednet.sock -thread "+threadKey+" -ref main:2 -card "+card+" -json\n" {
		t.Fatalf("%+v, calls %q", out, w.calls())
	}
	out = w.asThreadAgent("TH-5", "send", "item-1-lead", "--file", task(w, "n.md", "Asking the people now.\n"))
	if out.code != 0 || strings.Contains(out.stdout, "answered") {
		t.Errorf("send: %+v", out)
	}
	if out := w.inbox(w.event("m2", "hmm, let me think", "")); out.code != 0 || strings.Contains(out.stdout, "answered") {
		t.Errorf("message: %+v", out)
	}
	if got := questions(w); got != "item-1 "+threadKey+" item-1-lead card pending\n "+threadKey+
		" thread-c0123-1700000000-123 card pending" {
		t.Errorf("questions = %q", got)
	}

	_ = os.Remove(filepath.Join(w.dir, "prompts"))
	snoozed := w.answerEvent("a1", `{"question_id":"main:2","card_id":"card-2","outcome":"snoozed","choice":null,`+
		`"note":"after lunch <3","snooze_until":"2026-10-16T12:00:00-07:00","by":"U0ABC","at":"2026-10-16T10:00:00-07:00"}`)
	out = w.inbox(snoozed)
	if out.code != 0 || !strings.Contains(out.stdout, "question #2: snoozed, given to the agent of thread "+threadKey) {
		t.Fatalf("snoozed: %+v", out)
	}
	prompts := w.file("prompts")
	for _, want := range []string{
		"thread-c0123-1700000000-123\n[FROM: inbox]\n[ANSWER #2]\nA press on the card of your question #2 in thread " + threadKey + ".",
		`{"question_id":2,"card_id":"card-2","outcome":"snoozed","choice":null,"note":"after lunch <3",` +
			`"snooze_until":"2026-10-16T12:00:00-07:00","by":"U0ABC","at":"2026-10-16T10:00:00-07:00"}`,
		"item-1-lead\n[FROM: inbox]\n[ANSWER #1]\nA press on the card your home thread's agent posted in thread " + threadKey +
			" for your question #1.",
		`{"question_id":1,"card_id":"card-2","outcome":"snoozed"`,
	} {
		if !strings.Contains(prompts, want) {
			t.Errorf("prompts = %q, want %q in it", prompts, want)
		}
	}
	if got := question(w, 2); got != "pending snoozed 1792177200" {
		t.Errorf("question 2 = %q", got)
	}
	// fednet running the hook again: nothing twice.
	_ = os.Remove(filepath.Join(w.dir, "prompts"))
	if out := w.inbox(snoozed); out.code != 0 || !strings.Contains(out.stdout, "message a1 was already delivered") || w.file("prompts") != "" {
		t.Errorf("again: %+v, prompts %q", out, w.file("prompts"))
	}
	// Not fleet's card, or not this question's: ignored.
	for _, a := range []string{`{"question_id":"main:2","card_id":"card-7","outcome":"answered","at":"x"}`,
		`{"question_id":"q-12","card_id":"card-2","outcome":"answered","at":"x"}`} {
		if out := w.inbox(w.answerEvent("a9", a)); out.code != 0 || !strings.Contains(out.stdout, "ignored a9") || w.file("prompts") != "" {
			t.Errorf("%s: %+v", a, out)
		}
	}
	out = w.inbox(w.answerEvent("a2", `{"question_id":"main:2","card_id":"card-2","outcome":"answered",`+
		`"choice":{"index":1,"short":"October"},"by":"U0ABC","at":"2026-10-16T12:05:00-07:00"}`))
	if out.code != 0 || !strings.Contains(w.file("prompts"), `{"question_id":1,"card_id":"card-2","outcome":"answered","choice":{"index":1,"short":"October"}`) {
		t.Errorf("answered: %+v, prompts %q", out, w.file("prompts"))
	}
	if got := questions(w); got != "item-1 "+threadKey+" item-1-lead card answered\n "+threadKey+
		" thread-c0123-1700000000-123 card answered" {
		t.Errorf("questions = %q", got)
	}
	if got := question(w, 2); got != "answered answered" {
		t.Errorf("question 2 = %q", got)
	}
}

// A card taken back is closed; a press that still reaches it is recorded
// and given to nobody.
func TestAskHumanCloseTakesBackACard(t *testing.T) {
	w := threadWorld(t, "")
	if out := w.inbox(w.event("m1", "start", "")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if out := w.asThreadAgent("TH-5", "ask-human", "--card", task(w, "q.json", `{"question": "Which month?"}`)); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	out := w.asThreadAgent("TH-5", "ask-human", "--close", "1", "--note", "moot now")
	if out.code != 0 || out.stdout != "took back question #1 in thread "+threadKey+"\n" ||
		w.calls() != "fednet client ask-close -socket /run/fednet.sock -note moot now -- card-1\n" {
		t.Errorf("%+v, calls %q", out, w.calls())
	}
	if got := question(w, 1); got != "closed withdrawn" {
		t.Errorf("question 1 = %q", got)
	}
	_ = os.Remove(filepath.Join(w.dir, "prompts"))
	out = w.inbox(w.answerEvent("a1", `{"question_id":"main:1","card_id":"card-1","outcome":"answered","choice":null,"note":"x","at":"y"}`))
	if out.code != 0 || !strings.Contains(out.stdout, "question #1 is closed: its answered is recorded and given to no agent") ||
		w.file("prompts") != "" {
		t.Errorf("%+v, prompts %q", out, w.file("prompts"))
	}
	if got := question(w, 1); got != "closed answered" {
		t.Errorf("question 1 = %q", got)
	}
}

// An agent started on an older prompt still asks with --file: a lead's
// text goes to the thread agent as ask-thread takes it, a thread agent's
// is posted as a card with no options.
func TestAskHumanFileFromAnOlderPrompt(t *testing.T) {
	w := threadWorld(t, "")
	openJobWithHomeThread(w, true)
	if out := w.inbox(w.event("m1", "start the import", "")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	out := w.run("", []string{"ask-human", "--file", task(w, "q.md", "Which month?\n")}, "FLEET_AGENT=item-1-lead",
		"FLEET_ROLE=lead", "FLEET_PARENT=thread-x", "FLEET_SCOPE=main", "FLEET_JOB=item-1", linearKey)
	if out.code != 0 || !strings.Contains(out.stdout, "gave question #1 to the agent of thread "+threadKey) ||
		!strings.Contains(w.file("argv"), "[FROM: item-1-lead]\n[QUESTION #1 from item-1-lead]\n") ||
		!strings.HasSuffix(strings.Split(w.file("argv"), "\n--wait")[0], "\n\nWhich month?\n") {
		t.Errorf("lead: %+v, argv %q", out, w.file("argv"))
	}
	if got := questions(w); got != "item-1 "+threadKey+" item-1-lead plain pending" {
		t.Errorf("questions = %q", got)
	}
	for _, c := range []struct{ text, card string }{
		{"Which bucket? <b>\nThe new one or the old one.\n",
			`{"question":"Which bucket? <b>","context":"Which bucket? <b>\nThe new one or the old one."}`},
		{strings.Repeat("长", 141) + "\nmore\n",
			`{"question":"agent 有一个问题，详见下方","context":"` + strings.Repeat("长", 141) + `\nmore"}`},
	} {
		out := w.asThreadAgent("TH-5", "ask-human", "--file", task(w, "t.md", c.text))
		if out.code != 0 || !strings.Contains(out.stdout, "asked question #") {
			t.Errorf("thread agent: %+v", out)
		}
		if got := strings.TrimSpace(w.file("card.json")); got != c.card {
			t.Errorf("card = %s, want %s", got, c.card)
		}
	}
	// Only a press answers it.
	if out := w.inbox(w.event("m2", "the new one", "")); out.code != 0 || strings.Contains(out.stdout, "answered") {
		t.Errorf("message: %+v", out)
	}
	if got := questions(w); got != "item-1 "+threadKey+" item-1-lead plain pending\n "+threadKey+
		" thread-c0123-1700000000-123 card pending\n "+threadKey+" thread-c0123-1700000000-123 card pending" {
		t.Errorf("questions = %q", got)
	}
}

// A lead's question still pending when its job ended is the thread's: a
// person's message answers it, unless a card asks the people for it.
func TestAMessageAnswersTheQuestionsOfAnEndedJob(t *testing.T) {
	w := threadWorld(t, "")
	openJobWithHomeThread(w, true)
	if out := w.inbox(w.event("m1", "start the import", "")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	for _, f := range []string{"q1.json", "q2.json"} {
		if out := w.askThread("--card", task(w, f, `{"question": "`+f+`"}`)); out.code != 0 {
			t.Fatalf("%+v", out)
		}
	}
	if out := w.asThreadAgent("TH-5", "ask-human", "--card", task(w, "c.json", `{"question": "q2?"}`), "--for", "2"); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	conn := w.defaultLedger()
	if _, err := conn.Exec("UPDATE jobs SET state = 'ended', ended_at = 1"); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	out := w.inbox(w.event("m2", "September", ""))
	if out.code != 0 || !strings.Contains(out.stdout, "1 pending question(s) in thread "+threadKey+" answered") {
		t.Errorf("%+v", out)
	}
	if got := questions(w); got != "item-1 "+threadKey+" item-1-lead card answered\nitem-1 "+threadKey+
		" item-1-lead card pending\n "+threadKey+" thread-c0123-1700000000-123 card pending" {
		t.Errorf("questions = %q", got)
	}
}

// A snooze whose time fleet cannot read is recorded without one and
// delivered, so fednet does not run the hook again for it.
func TestASnoozeWithoutAReadableTimeIsDelivered(t *testing.T) {
	w := threadWorld(t, "")
	if out := w.inbox(w.event("m1", "start", "")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if out := w.asThreadAgent("TH-5", "ask-human", "--card", task(w, "q.json", `{"question": "Which month?"}`)); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	out := w.inbox(w.answerEvent("a1", `{"question_id":"main:1","card_id":"card-1","outcome":"snoozed","choice":null,`+
		`"snooze_until":"tomorrow","by":"U0ABC","at":"x"}`))
	if out.code != 0 || !strings.Contains(out.stderr, `snooze_until that is not RFC 3339 ("tomorrow")`) ||
		!strings.Contains(w.file("argv"), "[ANSWER #1]") {
		t.Errorf("%+v", out)
	}
	if got := question(w, 1); got != "pending snoozed" {
		t.Errorf("question 1 = %q", got)
	}
}

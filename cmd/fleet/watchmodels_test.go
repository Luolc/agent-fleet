// The watch `model-changes` against the watch fakes, with made-up Claude
// Code sessions: a session file per process and a transcript per session
// under the hermetic HOME.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// refusedText is in every transcript's conversation text; no report may
// carry it.
const refusedText = "0xREFUSEDTEXT"

// claudeSession puts a Claude Code session in `pane`: its process `pid`
// in the pane's foreground, the session file Claude Code writes for it,
// and its transcript, of `records`.
func (w *watchWorld) claudeSession(pane string, pid int, session string, records ...string) {
	w.t.Helper()
	w.file("procinfo-"+pane, fmt.Sprintf(`{"result":{"process_info":{"shell_pid":1,"foreground_processes":`+
		`[{"pid":%d,"name":"claude"}]}}}`, pid))
	claude := filepath.Join(w.dir, "home", ".claude")
	w.write(filepath.Join(claude, "sessions", fmt.Sprintf("%d.json", pid)),
		fmt.Sprintf(`{"pid":%d,"sessionId":"%s","cwd":"/work/example","startedAt":1}`, pid, session))
	w.write(filepath.Join(claude, "projects", "-work-example", session+".jsonl"), strings.Join(records, ""))
}

// appendRecords adds `records` to the transcript of `session`.
func (w *watchWorld) appendRecords(session string, records ...string) {
	w.t.Helper()
	path := filepath.Join(w.dir, "home", ".claude", "projects", "-work-example", session+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		w.t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(strings.Join(records, "")); err != nil {
		w.t.Fatal(err)
	}
}

func (w *watchWorld) write(path, body string) {
	w.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

// record is one transcript line as Claude Code writes it.
func record(fields map[string]any) string {
	data, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return string(data) + "\n"
}

// assistant is an assistant message's record: one content block of the
// message `id`, answered by `model`.
func assistant(uuid, at, id, model, stop string) string {
	return record(map[string]any{"type": "assistant", "uuid": uuid, "timestamp": at, "isSidechain": false,
		"message": map[string]any{"id": id, "model": model, "role": "assistant", "stop_reason": stop,
			"content": []any{map[string]any{"type": "text", "text": "about " + refusedText}}}})
}

func user(uuid, at string) string {
	return record(map[string]any{"type": "user", "uuid": uuid, "timestamp": at,
		"message": map[string]any{"role": "user", "content": "please " + refusedText}})
}

func fallback(uuid, at, from, to string) string {
	return record(map[string]any{"type": "system", "subtype": "model_refusal_fallback", "uuid": uuid, "timestamp": at,
		"originalModel": from, "fallbackModel": to, "direction": "retry", "scope": "session", "trigger": "refusal",
		"content": "refused: " + refusedText, "apiRefusalExplanation": refusedText, "apiRefusalCategory": refusedText})
}

func noFallback(uuid, at, from string) string {
	return record(map[string]any{"type": "system", "subtype": "model_refusal_no_fallback", "uuid": uuid,
		"timestamp": at, "originalModel": from, "content": "refused: " + refusedText,
		"apiRefusalExplanation": refusedText})
}

// The sessions of the test: a worker downgraded, a thread agent stopped by
// the safety classifier with the fallback off, a worker that ran on one
// model throughout, a Pi agent, whose session is never read, and the new
// session a takeover starts.
const (
	sessionWorker = "00000000-0000-4000-8000-000000000001"
	sessionThread = "00000000-0000-4000-8000-000000000002"
	sessionSteady = "00000000-0000-4000-8000-000000000003"
	sessionPi     = "00000000-0000-4000-8000-000000000004"
	sessionNew    = "00000000-0000-4000-8000-000000000005"
)

// The model and effort the ledger records for the starts: neither model
// of the transcripts, so a start that took its model from a transcript
// shows.
const (
	startModel  = "claude-test-configured"
	startEffort = "high"
)

// modelWorld is a thread C1/1.0 with its agent thread-c1, home thread of
// the open job j (repo example), whose lead j-lead has the workers j-w
// (downgraded after a refusal, idle), j-ok (on one model throughout) and
// j-pi (Pi) in the workspace w-j; j-w has a task file, a directory, a work
// order and two files of its own in the job's scratch directory.
func modelWorld(t *testing.T) *watchWorld {
	t.Helper()
	w := watchThreadWorld(t)
	work := filepath.Join(w.dir, "work")
	task := filepath.Join(w.dir, "tasks", "j-w.md")
	scratch := filepath.Join(w.dir, "home", "scratch", "example")
	w.write(task, "# Fix the example parser\n\nMake the parser accept empty input.\n")
	w.write(filepath.Join(scratch, "report-j-j-w-2030-01-02.md"), "notes")
	w.write(filepath.Join(scratch, "report-j-other-2030-01-02.md"), "notes")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	w.file("workspaces.json", `{"result":{"workspaces":[{"workspace_id":"w-j","label":"j"}]}}`)
	w.exec("INSERT INTO agents (name, role, thread, pane_id, state, started_at, kind) VALUES "+
		"('thread-c1', 'thread', 'C1/1.0', 'p1', 'active', 0, 'claude')",
		"INSERT INTO jobs (job, repo, lead_cwd, home_thread, state, started_at) VALUES "+
			"('j', 'example', '"+filepath.Join(w.dir, "dev", "example")+"', 'C1/1.0', 'open', 0)",
		"INSERT INTO agents (name, role, job, parent, pane_id, state, started_at, kind, model, effort) VALUES "+
			"('j-lead', 'lead', 'j', 'thread-c1', 'p2', 'active', 0, 'claude', '"+startModel+"', '"+startEffort+"'), "+
			"('j-ok', 'worker', 'j', 'j-lead', 'p4', 'active', 0, 'claude', '"+startModel+"', '"+startEffort+"'), "+
			"('j-pi', 'worker', 'j', 'j-lead', 'p5', 'active', 0, 'pi', 'example/pi-model', NULL)",
		"INSERT INTO agents (name, role, job, parent, pane_id, state, started_at, kind, model, effort, cwd, task, "+
			"issue, issue_url) VALUES ('j-w', 'worker', 'j', 'j-lead', 'p3', 'active', 0, 'claude', '"+startModel+"', '"+
			startEffort+"', '"+work+"', '"+task+"', 'LL-9', 'https://linear.example.test/issue/LL-9')")
	w.herdrList("thread-c1 idle 1", "j-lead idle 1", "j-w idle 1", "j-ok working 1", "j-pi working 1 pi")
	for _, name := range []string{"thread-c1", "j-lead", "j-w", "j-ok", "j-pi"} {
		w.screen(name, claude("⏺ "+name, "1s", "2hr 59m"))
	}
	w.lastMessage("C1/1.0", t0)
	w.claudeSession("p3", 4103, sessionWorker,
		user("u-1", "2030-01-02T03:00:00.000Z"),
		assistant("a-1", "2030-01-02T03:00:01.000Z", "msg-1", "claude-test-large", "tool_use"),
		assistant("a-2", "2030-01-02T03:00:02.000Z", "msg-2", "claude-test-large", "refusal"),
		assistant("a-3", "2030-01-02T03:00:02.500Z", "msg-2", "claude-test-large", "refusal"),
		fallback("f-1", "2030-01-02T03:00:03.000Z", "claude-test-large", "claude-test-small"),
		assistant("a-4", "2030-01-02T03:00:04.000Z", "msg-3", "claude-test-small", "tool_use"))
	w.claudeSession("p1", 4101, sessionThread,
		assistant("t-1", "2030-01-02T04:00:00.000Z", "msg-t1", "claude-test-large", "end_turn"),
		assistant("t-2", "2030-01-02T04:00:01.000Z", "msg-t2", "claude-test-large", "refusal"),
		noFallback("t-3", "2030-01-02T04:00:01.500Z", "claude-test-large"),
		assistant("t-4", "2030-01-02T04:00:01.600Z", "msg-t3", "<synthetic>", "refusal"))
	w.claudeSession("p4", 4104, sessionSteady,
		user("s-1", "2030-01-02T05:00:00.000Z"),
		assistant("s-2", "2030-01-02T05:00:01.000Z", "msg-s1", "claude-test-large", "tool_use"),
		assistant("s-3", "2030-01-02T05:00:02.000Z", "msg-s2", "<synthetic>", "stop_sequence"),
		record(map[string]any{"type": "assistant", "uuid": "s-4", "isSidechain": true,
			"message": map[string]any{"id": "msg-s3", "model": "claude-test-tiny"}}),
		assistant("s-5", "2030-01-02T05:00:03.000Z", "msg-s4", "claude-test-large", "end_turn"))
	w.claudeSession("p5", 4105, sessionPi,
		fallback("p-1", "2030-01-02T06:00:00.000Z", "claude-test-large", "claude-test-small"))
	return w
}

// posts are the texts fleet posted to threads, each as `<thread> <text>`.
func (w *watchWorld) posts() []string {
	var posts []string
	for _, entry := range strings.Split(w.raw("fednet.log"), "client post ") {
		if thread, text, ok := strings.Cut(entry, " -- "); ok && strings.Contains(thread, "-thread ") {
			posts = append(posts, thread[strings.Index(thread, "-thread ")+len("-thread "):]+" "+text)
		}
	}
	return posts
}

func (w *watchWorld) raw(name string) string {
	data, _ := os.ReadFile(filepath.Join(w.dir, name))
	return string(data)
}

// promptsTo are the texts delivered to `name`.
func (w *watchWorld) promptsTo(name string) []string {
	var texts []string
	for _, p := range w.prompts() {
		if p[0] == name {
			texts = append(texts, p[1])
		}
	}
	return texts
}

// noCommandTyped fails the test when any text delivered to an agent is a
// slash command, or starts a line with one.
func (w *watchWorld) noCommandTyped() {
	w.t.Helper()
	for _, p := range w.prompts() {
		for _, line := range strings.Split(p[1], "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "/") {
				w.t.Errorf("a command was typed into %s: %q", p[0], line)
			}
		}
	}
}

// hasAll fails the test for each of `want` missing from `text`.
func hasAll(t *testing.T, what, text string, want ...string) {
	t.Helper()
	for _, s := range want {
		if !strings.Contains(text, s) {
			t.Errorf("%q missing from %s:\n%s", s, what, text)
		}
	}
}

// A downgraded worker gets a new session of the same name, on the model
// and effort its start recorded, in its directory, with its work order,
// task and a takeover note, and the old session's tab is closed; a thread
// agent stopped on its model is told once to go on; nothing of the
// conversation reaches anything fleet writes, and nobody is told twice.
func TestTheModelWatchTakesOverWithoutTheConversation(t *testing.T) {
	w := modelWorld(t)
	out := w.watchAt(time.Minute, 0)
	for _, text := range []string{out.stdout, out.stderr, w.raw("prompts"), w.raw("fednet.log"), w.raw("tab-argv"),
		w.raw("starts")} {
		if strings.Contains(text, refusedText) {
			t.Errorf("the conversation's text was passed on:\n%s", text)
		}
	}
	w.noCommandTyped()

	// The worker's new session.
	starts := strings.Split(strings.TrimSpace(w.raw("starts")), "\n")
	if len(starts) != 1 || !strings.HasPrefix(starts[0], "agent start j-w --kind claude --pane pn1 ") ||
		!strings.HasSuffix(starts[0], " --model "+startModel+" --effort "+startEffort) {
		t.Errorf("starts %q", starts)
	}
	hasAll(t, "the new tab", w.raw("tab-argv"), "--workspace w-j --label w ", "--cwd "+filepath.Join(w.dir, "work"),
		"FLEET_AGENT=j-w", "FLEET_ROLE=worker", "FLEET_PARENT=j-lead", "FLEET_JOB=j", "FLEET_ISSUE=LL-9")
	if closed := strings.Fields(w.raw("closed")); strings.Join(closed, " ") != "p3" {
		t.Errorf("closed %q, want the old session's pane p3 alone", closed)
	}
	if got := w.query("SELECT group_concat(id || ' ' || pane_id || ' ' || state || ' ' || takeovers || ' ' || issue, ',') " +
		"FROM agents WHERE name = 'j-w'"); got != "5 pn1 active 1 LL-9" {
		t.Errorf("j-w's rows: %s", got)
	}
	first := w.promptsTo("j-w")
	if len(first) != 1 {
		t.Fatalf("j-w got %q", first)
	}
	hasAll(t, "j-w's first message", first[0], "[FROM: watch]\nYou are a worker run by fleet: j-w",
		"## Note from fleet\n\nThis session takes over from an earlier session of j-w, which fleet closed: in it, "+
			"the session now runs on claude-test-small; it started on claude-test-large.",
		"2030-01-02T03:00:03.000Z model_refusal_fallback: claude-test-large -> claude-test-small (record f-1)",
		"the same name, task, work order (LL-9), directory and parent (j-lead)",
		"your directory "+filepath.Join(w.dir, "work")+" (read its branch, HEAD and `git status` first)",
		"~/scratch/example/report-j-j-w-2030-01-02.md",
		"Do not redo the step that was stopped, neither as it was nor in other words",
		"hand that step to your lead j-lead",
		"## Your task\n\nWork order: https://linear.example.test/issue/LL-9\n\n# Fix the example parser")
	for _, leak := range []string{"report-j-other", sessionWorker, ".jsonl"} {
		if strings.Contains(first[0], leak) {
			t.Errorf("%q in j-w's first message:\n%s", leak, first[0])
		}
	}

	// The thread agent is told to go on, and the lead about its worker.
	nudge := w.promptsTo("thread-c1")
	if len(nudge) != 1 {
		t.Fatalf("thread-c1 got %q", nudge)
	}
	hasAll(t, "the message to thread-c1", nudge[0], "[FROM: watch]",
		"the session is stopped: its latest message, on claude-test-large, was stopped by a safety classifier",
		"2030-01-02T04:00:01.500Z model_refusal_no_fallback: stopped on claude-test-large, no fallback (record t-3)",
		"Go on with the thread's work", "(the open jobs reporting to this thread: j)",
		"Do not redo the step that was stopped, neither as it was nor in other words")
	if got := w.query("SELECT takeovers || ' ' || pane_id FROM agents WHERE name = 'thread-c1'"); got != "1 p1" {
		t.Errorf("thread-c1: %s", got)
	}
	told := w.promptsTo("j-lead")
	if len(told) != 1 {
		t.Fatalf("j-lead got %q", told)
	}
	hasAll(t, "the message to j-lead", told[0], "[FROM: watch]",
		"the Claude Code session of j-w (worker, job j) has 3 model event(s)",
		"2030-01-02T03:00:02.000Z stopped by a safety classifier (stop_reason refusal) on claude-test-large (record a-2)",
		"2030-01-02T03:00:04.000Z model_change: claude-test-large -> claude-test-small, the session started on "+
			"claude-test-large (record a-4)",
		"fleet replaced the session of j-w (pane p3 closed, the new session in pane pn1) because the session now "+
			"runs on claude-test-small",
		"What fleet did about it is said above.")
	for _, name := range []string{"j-ok", "j-pi"} {
		if got := w.promptsTo(name); len(got) != 0 || strings.Contains(out.stdout, "model event: "+name) {
			t.Errorf("%s: %q\n%s", name, got, out.stdout)
		}
	}
	posts := w.posts()
	if len(posts) != 2 {
		t.Fatalf("posts %q", posts)
	}
	hasAll(t, "the thread agent's post", posts[0], "C1/1.0 fleet watch：thread-c1 的 Claude Code 会话出现了 2 个模型事件",
		"2030-01-02T04:00:01.000Z 被 safety classifier 拦停 (`stop_reason: refusal`)，模型 `claude-test-large`，记录 `t-2`",
		"fleet 以 watch 的名义给 `thread-c1` 发了一次「接着干」")
	hasAll(t, "the worker's post", posts[1], "C1/1.0 fleet watch：j-w 的 Claude Code 会话出现了 3 个模型事件",
		"降级 (`model_refusal_fallback`)：`claude-test-large` → `claude-test-small`，记录 `f-1`",
		"fleet 就地换了 `j-w` 的会话 (pane p3 closed, the new session in pane pn1)，原因：会话现在用的是 `claude-test-small`")

	// The next run finds nothing new and does nothing.
	before, logged := len(w.prompts()), len(w.posts())
	if out := w.watchAt(2*time.Minute, 0); strings.Contains(out.stdout, "model event") {
		t.Fatalf("%+v", out)
	}
	if len(w.prompts()) != before || len(w.posts()) != logged || len(strings.Fields(w.raw("closed"))) != 1 ||
		strings.Count(w.raw("starts"), "agent start") != 1 {
		t.Errorf("acted again: %q %q", w.prompts()[before:], w.posts()[logged:])
	}
}

// A second stop of an agent fleet took over once is left to the people:
// no new session, no second message to go on, the parent and the thread
// told that a person decides; a thread agent stopped again after it
// replied counts as stopped again.
func TestTheModelWatchLeavesASecondEventToThePeople(t *testing.T) {
	w := modelWorld(t)
	w.watchAt(time.Minute, 0)
	before, logged := len(w.prompts()), len(w.posts())
	w.claudeSession("pn1", 4106, sessionNew,
		assistant("n-1", "2030-01-02T08:00:00.000Z", "msg-n1", "claude-test-large", "refusal"))
	w.appendRecords(sessionThread, assistant("t-5", "2030-01-02T08:00:01.000Z", "msg-t5", "claude-test-large", "end_turn"),
		assistant("t-6", "2030-01-02T08:00:02.000Z", "msg-t6", "claude-test-large", "refusal"))
	w.watchAt(2*time.Minute, 0)
	w.noCommandTyped()
	if starts := strings.Count(w.raw("starts"), "agent start"); starts != 1 || len(strings.Fields(w.raw("closed"))) != 1 {
		t.Errorf("%d starts, closed %q", starts, w.raw("closed"))
	}
	var targets []string
	for _, p := range w.prompts()[before:] {
		targets = append(targets, p[0])
	}
	if strings.Join(targets, " ") != "j-lead" {
		t.Fatalf("told %q", targets)
	}
	hasAll(t, "the message to j-lead", w.prompts()[before][1], "(record n-1)",
		"it handled j-w once already (a new session, or a message to go on), so a person decides what to do now")
	posts := w.posts()[logged:]
	if len(posts) != 2 {
		t.Fatalf("posts %q", posts)
	}
	hasAll(t, "the thread agent's post", posts[0], "记录 `t-6`", "`thread-c1` 已经被自动处理过一次", "需要人来定怎么办")
	hasAll(t, "the worker's post", posts[1], "记录 `n-1`", "`j-w` 已经被自动处理过一次", "需要人来定怎么办")
}

// threadKeyC1 is modelWorld's thread, and threadC1 the name its agents
// get from it.
const (
	threadKeyC1 = "C1/1.0"
	threadC1    = "thread-c1-1-0"
)

// threadTakeoverWorld is modelWorld with the thread agent named as a
// start names it, a thread ticket TH-7 its session claimed, a Linear team,
// and the `threads` workspace with its `shell` tab, so a thread's next
// session can start.
func threadTakeoverWorld(t *testing.T) *watchWorld {
	t.Helper()
	w := modelWorld(t)
	checkout := filepath.Join(w.dir, "home", "dev", "example")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	w.write(filepath.Join(w.dir, "home", ".config", "fleet", scope+".json"),
		`{"linear": {"team": "TH"}, "fednet": {"socket": "/run/example/fednet.sock"}}`)
	w.file("workspaces.json", `{"result":{"workspaces":[{"workspace_id":"w-j","label":"j"},`+
		`{"workspace_id":"w-t","label":"threads"}]}}`)
	w.file("tabs.json", `{"result":{"tabs":[{"label":"shell","tab_id":"tab-shell"}]}}`)
	w.file("issues.json", `{"issue":{"comments":{"nodes":[]}}}`)
	w.exec("UPDATE agents SET name = '"+threadC1+"', issue = 'TH-7', issue_url = 'https://linear.example.test/TH-7' "+
		"WHERE name = 'thread-c1'",
		"UPDATE agents SET parent = '"+threadC1+"' WHERE name = 'j-lead'",
		"INSERT INTO threads (thread, slug, channel, mapping, cwd, sessions, ticket, ticket_url, created_at) VALUES "+
			"('"+threadKeyC1+"', 'c1-1-0', 'C1', 'repo-example', '"+checkout+"', 1, 'TH-7', "+
			"'https://linear.example.test/TH-7', 0)")
	w.herdrList(threadC1+" idle 1", "j-lead idle 1", "j-w working 1", "j-ok working 1", "j-pi working 1 pi")
	w.screen(threadC1, claude("⏺ "+threadC1, "1s", "2hr 59m"))
	w.claudeSession("p1", 4101, sessionThread,
		assistant("t-1", "2030-01-02T04:00:00.000Z", "msg-t1", "claude-test-large", "refusal"),
		fallback("t-2", "2030-01-02T04:00:01.000Z", "claude-test-large", "claude-test-small"),
		assistant("t-3", "2030-01-02T04:00:02.000Z", "msg-t3", "claude-test-small", "end_turn"))
	return w
}

// A thread agent moved to another model gets the thread's next session:
// the old one ended with fleet's own summary on the ticket and its tab
// closed, the new one started as `inbox` starts one, claiming the same
// ticket, on the model its start recorded (none), with the takeover note
// as its message; no command is typed, nothing of the conversation passed
// on. A second move on the thread is left to the people.
func TestTheModelWatchStartsANewSessionForAThreadAgentMovedToAnotherModel(t *testing.T) {
	w := threadTakeoverWorld(t)
	out := w.watchAt(time.Minute, 0, linearKey)
	w.noCommandTyped()
	for _, text := range []string{out.stdout, out.stderr, w.raw("prompts"), w.raw("fednet.log"), w.raw("atb.log"),
		w.raw("starts")} {
		if strings.Contains(text, refusedText) {
			t.Errorf("the conversation's text was passed on:\n%s", text)
		}
	}
	if got := w.query("SELECT group_concat(state || ' ' || pane_id || ' ' || issue, ',') FROM (SELECT * FROM agents " +
		"WHERE name = '" + threadC1 + "' ORDER BY id)"); got != "ended p1 TH-7,active pn1 TH-7" {
		t.Errorf("%s's rows: %s", threadC1, got)
	}
	if closed := strings.Fields(w.raw("closed")); strings.Join(closed, " ") != "p1" {
		t.Errorf("closed %q", closed)
	}
	starts := strings.TrimSpace(w.raw("starts"))
	if !strings.HasPrefix(starts, "agent start "+threadC1+" --kind claude --pane pn1 ") || strings.Contains(starts, "--model") ||
		strings.Contains(starts, "--effort") {
		t.Errorf("starts %q", starts)
	}
	atb := w.log("atb.log")
	hasAll(t, "atb's calls", atb, "linear comment TH-7 --body-file F", "linear release TH-7 --agent "+threadC1,
		"linear claim TH-7 --agent "+threadC1)
	if got := w.query("SELECT sessions || ' ' || takeovers FROM threads"); got != "2 1" {
		t.Errorf("the thread: %s", got)
	}
	first := w.promptsTo(threadC1)
	if len(first) != 1 {
		t.Fatalf("%s got %q", threadC1, first)
	}
	hasAll(t, "the new session's first message", first[0], "[FROM: watch]",
		"## The message\n\nfleet watch, watch `model-changes`: this session takes over from the session before it on "+
			"this thread, which fleet ended: in it, the session now runs on claude-test-small; it started on "+
			"claude-test-large.",
		"(record t-2)", "Do not redo the step that was stopped, neither as it was nor in other words",
		"ask the people in the thread about that step")
	for _, leak := range []string{sessionThread, ".jsonl"} {
		if strings.Contains(first[0], leak) {
			t.Errorf("%q in the first message", leak)
		}
	}
	hasAll(t, "the closing line", w.raw("fednet.log"), "fleet 结束了这次会话并另起了一个新会话")
	hasAll(t, "the post", strings.Join(w.posts(), "\n"), "fleet 就地换了 `"+threadC1+"` 的会话 (the old session ended, "+
		"a new session started for the thread)")

	// The new session moved too: left to the people.
	before, starts1 := len(w.prompts()), strings.Count(w.raw("starts"), "agent start")
	w.claudeSession("pn1", 4107, sessionNew,
		assistant("n-1", "2030-01-02T09:00:00.000Z", "msg-n1", "claude-test-large", "end_turn"),
		assistant("n-2", "2030-01-02T09:00:01.000Z", "msg-n2", "claude-test-small", "end_turn"))
	w.watchAt(2*time.Minute, 0, linearKey)
	if strings.Count(w.raw("starts"), "agent start") != starts1 || len(w.prompts()) != before ||
		len(strings.Fields(w.raw("closed"))) != 1 {
		t.Errorf("acted again: %q", w.prompts()[before:])
	}
	posts := w.posts()
	hasAll(t, "the last post", posts[len(posts)-1], "记录 `n-2`", "已经被自动处理过一次", "需要人来定怎么办")
}

// A session still working is left to finish its turn: its events are
// reported, but nothing is done until herdr finds it idle.
func TestTheModelWatchWaitsForABusySession(t *testing.T) {
	w := modelWorld(t)
	w.herdrList("thread-c1 working 1", "j-lead idle 1", "j-w working 1", "j-ok working 1", "j-pi working 1 pi")
	w.watchAt(time.Minute, 0)
	if len(w.promptsTo("j-w")) != 0 || len(w.promptsTo("thread-c1")) != 0 || w.raw("tab-argv") != "" {
		t.Fatalf("acted on a busy session: %q %q", w.prompts(), w.raw("tab-argv"))
	}
	hasAll(t, "the message to j-lead", w.promptsTo("j-lead")[0], "has 3 model event(s)", "fleet did nothing about these")
	w.herdrList("thread-c1 idle 1", "j-lead idle 1", "j-w done 1", "j-ok working 1", "j-pi working 1 pi")
	w.watchAt(2*time.Minute, 0)
	if len(w.promptsTo("j-w")) != 1 || len(w.promptsTo("thread-c1")) != 1 || len(w.posts()) != 4 {
		t.Fatalf("once idle: %q, posts %q", w.prompts(), w.posts())
	}
	hasAll(t, "the second message to j-lead", w.promptsTo("j-lead")[1], "about the Claude Code session of j-w",
		"fleet replaced the session of j-w")
}

// What counts is the session as it is now, read to its end: one moved back
// to the model it started on, or stopped and then replying again, has
// nothing done, its events only reported; a variant of the same model is
// no change; a session read before the upgrade is not acted on for what
// was reported then.
func TestTheModelWatchActsOnlyOnTheSessionAsItIsNow(t *testing.T) {
	w := modelWorld(t)
	w.claudeSession("p3", 4103, sessionWorker,
		assistant("a-1", "2030-01-02T03:00:01.000Z", "msg-1", "claude-test-large", "refusal"),
		fallback("f-1", "2030-01-02T03:00:03.000Z", "claude-test-large[1m]", "claude-test-small"),
		assistant("a-2", "2030-01-02T03:00:04.000Z", "msg-2", "claude-test-small", "end_turn"),
		assistant("a-3", "2030-01-03T03:00:00.000Z", "msg-3", "claude-test-large", "end_turn"))
	w.claudeSession("p1", 4101, sessionThread,
		assistant("t-1", "2030-01-02T04:00:01.000Z", "msg-t1", "claude-test-large", "refusal"),
		assistant("t-2", "2030-01-02T04:00:02.000Z", "msg-t2", "claude-test-large", "end_turn"))
	w.claudeSession("p4", 4104, sessionSteady,
		assistant("s-1", "2030-01-02T05:00:01.000Z", "msg-s1", "claude-test-large[1m]", "end_turn"),
		assistant("s-2", "2030-01-02T05:00:02.000Z", "msg-s2", "claude-test-large", "end_turn"))
	refused := assistant("l-1", "2030-01-02T06:00:01.000Z", "msg-l1", "claude-test-large", "refusal")
	w.claudeSession("p2", 4102, "00000000-0000-4000-8000-000000000006", refused)
	w.exec(fmt.Sprintf("INSERT INTO transcripts (agent, session, path, read_to, start_model, last_model, stopped, "+
		"decided_at) VALUES (2, '00000000-0000-4000-8000-000000000006', '%s', %d, 'claude-test-large', "+
		"'claude-test-large', 1, 0)",
		filepath.Join(w.dir, "home", ".claude", "projects", "-work-example", "00000000-0000-4000-8000-000000000006.jsonl"),
		len(refused)))
	out := w.watchAt(time.Minute, 0)
	if w.raw("tab-argv") != "" || len(w.promptsTo("j-w")) != 0 || len(w.promptsTo("thread-c1")) != 0 ||
		len(w.promptsTo("thread-c1")) != 0 || w.query("SELECT count(*) FROM model_actions") != "0" {
		t.Fatalf("acted: %q %q", w.prompts(), w.raw("tab-argv"))
	}
	if strings.Contains(out.stdout, "model event: j-ok") {
		t.Errorf("a variant reported as a change:\n%s", out.stdout)
	}
	hasAll(t, "the message to j-lead", w.promptsTo("j-lead")[0], "has 4 model event(s)",
		"model_change: claude-test-small -> claude-test-large", "fleet did nothing about these events")
}

// A transcript this run did not read to its end has nothing done: its
// events so far are reported as not read to the end, and the next run
// reads on and acts.
func TestTheModelWatchActsOnlyOnATranscriptReadToItsEnd(t *testing.T) {
	w := modelWorld(t)
	filler := user("u-x", "2030-01-02T03:00:00.000Z")
	var lines []string
	for n := 0; n < 3<<20; n += len(filler) {
		lines = append(lines, filler)
	}
	records := append([]string{assistant("a-1", "2030-01-02T03:00:01.000Z", "msg-1", "claude-test-large", "refusal")}, lines...)
	records = append(records, fallback("f-1", "2030-01-02T03:00:03.000Z", "claude-test-large", "claude-test-small"),
		assistant("a-2", "2030-01-02T03:00:04.000Z", "msg-2", "claude-test-small", "end_turn"))
	w.claudeSession("p3", 4103, sessionWorker, records...)
	out := w.watchAt(time.Minute, 0, "FLEET_TEST_READ_BUDGET=0s")
	if w.raw("tab-argv") != "" || len(w.promptsTo("j-w")) != 0 {
		t.Fatalf("acted on a transcript not read to its end: %q", w.raw("tab-argv"))
	}
	hasAll(t, "watch's output", out.stdout, "model watch: the transcript of j-w (worker) was not read to its end this run")
	hasAll(t, "the message to j-lead", w.promptsTo("j-lead")[0], "has 1 model event(s)", "(record a-1)",
		"This run did not read its transcript to the end, so fleet did nothing about it yet")
	hasAll(t, "the post", w.posts()[1], "(未读完)")
	w.watchAt(2*time.Minute, 0)
	if len(w.promptsTo("j-w")) != 1 || !strings.Contains(w.raw("starts"), "agent start j-w") {
		t.Errorf("not taken over once read: %q", w.prompts())
	}
}

// A row started before fleet recorded the model and effort is not taken
// over: the parent and the thread are told that a person decides, and the
// one takeover is used up.
func TestTheModelWatchDoesNotGuessAModelTheLedgerLacks(t *testing.T) {
	w := modelWorld(t)
	w.exec("UPDATE agents SET kind = '', model = NULL, effort = NULL WHERE name = 'j-w'")
	w.watchAt(time.Minute, 0)
	if w.raw("tab-argv") != "" || w.raw("starts") != "" || len(w.promptsTo("j-w")) != 0 {
		t.Fatalf("taken over: %q %q", w.raw("tab-argv"), w.raw("starts"))
	}
	hasAll(t, "the message to j-lead", w.promptsTo("j-lead")[0], "fleet could not take j-w over (the session now runs "+
		"on claude-test-small; it started on claude-test-large): the ledger has no record of the model and effort it "+
		"was started with", "fleet will not handle this agent again")
	if got := w.query("SELECT takeovers || ' ' || state FROM agents WHERE name = 'j-w'"); got != "1 active" {
		t.Errorf("j-w: %s", got)
	}
}

// A tab that cannot be opened leaves the old session running and its row
// as it was; the failure is reported and not tried again.
func TestTheModelWatchKeepsTheOldSessionWhenNoTabOpens(t *testing.T) {
	w := modelWorld(t)
	w.file("tab-down", "x")
	w.watchAt(time.Minute, 0)
	if w.raw("closed") != "" || w.raw("starts") != "" {
		t.Errorf("closed %q, starts %q", w.raw("closed"), w.raw("starts"))
	}
	if got := w.query("SELECT pane_id || ' ' || state || ' ' || takeovers FROM agents WHERE name = 'j-w'"); got != "p3 active 1" {
		t.Errorf("j-w: %s", got)
	}
	hasAll(t, "the message to j-lead", w.promptsTo("j-lead")[0],
		"no tab could be opened for a new session, so the old one goes on")
}

// A tell that does not land is tried again next run, without posting the
// same events to the thread a second time.
func TestTheModelWatchTellsABlockedParentNextRun(t *testing.T) {
	w := modelWorld(t)
	w.exec("UPDATE agents SET takeovers = 1")
	w.reply(`{"error":{"code":"agent_blocked","message":"blocked"}}`)
	w.watchAt(time.Minute, 3)
	posts := len(w.posts())
	w.reply(`{"result":{"type":"agent_prompted"}}`)
	before := len(w.prompts())
	w.watchAt(2*time.Minute, 0)
	var again []string
	for _, p := range w.prompts()[before:] {
		if strings.Contains(p[1], "model-changes") {
			again = append(again, p[0])
		}
	}
	if strings.Join(again, " ") != "j-lead" || len(w.posts()) != posts {
		t.Errorf("told %q, posts %d then %d", again, posts, len(w.posts()))
	}
}

// Turned off, the watch reads no transcript.
func TestTheModelWatchCanBeTurnedOff(t *testing.T) {
	w := modelWorld(t)
	w.write(filepath.Join(w.dir, "home", ".config", "fleet", scope+".json"),
		`{"fednet": {"socket": "/run/example/fednet.sock"}, "watches": {"model-changes": {"off": true}}}`)
	out := w.watchAt(time.Minute, 0)
	if strings.Contains(out.stdout, "model event") || len(w.posts()) != 0 ||
		w.query("SELECT count(*) FROM transcripts") != "0" || len(w.prompts()) != 0 {
		t.Errorf("%+v, posts %q", out, w.posts())
	}
}

// A post that failed is tried again next run, without telling the parent
// again.
func TestTheModelWatchPostsAgainNextRunAfterAFailedPost(t *testing.T) {
	w := modelWorld(t)
	w.file("post-down", "x")
	w.watchAt(time.Minute, 5)
	tried, toldOnce := len(w.posts()), len(w.promptsTo("j-lead"))
	if tried != 2 || toldOnce != 1 {
		t.Fatalf("first run: %d posts tried, %d tells", tried, toldOnce)
	}
	w.file("post-down", "")
	w.watchAt(2*time.Minute, 0)
	posts := w.posts()[tried:]
	if len(posts) != 2 || !strings.Contains(posts[0], "thread-c1 的 Claude Code 会话出现了 2 个模型事件") ||
		!strings.Contains(posts[1], "j-w 的 Claude Code 会话出现了 3 个模型事件") || len(w.promptsTo("j-lead")) != toldOnce {
		t.Errorf("second run: posts %q, %d tells", posts, len(w.promptsTo("j-lead")))
	}
}

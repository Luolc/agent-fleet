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
// model throughout, and a Pi agent, whose session is never read.
const (
	sessionWorker = "00000000-0000-4000-8000-000000000001"
	sessionThread = "00000000-0000-4000-8000-000000000002"
	sessionSteady = "00000000-0000-4000-8000-000000000003"
	sessionPi     = "00000000-0000-4000-8000-000000000004"
)

func modelWorld(t *testing.T) *watchWorld {
	t.Helper()
	w := watchThreadWorld(t)
	w.exec("INSERT INTO agents (name, role, thread, pane_id, state, started_at) VALUES "+
		"('thread-c1', 'thread', 'C1/1.0', 'p1', 'active', 0)",
		"INSERT INTO jobs (job, lead_cwd, home_thread, state, started_at) VALUES ('j', '/c', 'C1/1.0', 'open', 0)",
		"INSERT INTO agents (name, role, job, parent, pane_id, state, started_at) VALUES "+
			"('j-lead', 'lead', 'j', 'thread-c1', 'p2', 'active', 0), ('j-w', 'worker', 'j', 'j-lead', 'p3', 'active', 0), "+
			"('j-ok', 'worker', 'j', 'j-lead', 'p4', 'active', 0), ('j-pi', 'worker', 'j', 'j-lead', 'p5', 'active', 0)")
	w.herdrList("thread-c1 idle 1", "j-lead idle 1", "j-w working 1", "j-ok working 1", "j-pi working 1 pi")
	for _, name := range []string{"j-lead", "j-w", "j-ok", "j-pi"} {
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

func TestTheModelWatchReportsEachEventOnceWithoutTheConversation(t *testing.T) {
	w := modelWorld(t)
	out := w.watchAt(time.Minute, 0)
	for _, text := range []string{out.stdout, out.stderr, w.raw("prompts"), w.raw("fednet.log")} {
		if strings.Contains(text, refusedText) {
			t.Errorf("the conversation's text was reported:\n%s", text)
		}
	}
	var told []string
	for _, p := range w.prompts() {
		if strings.Contains(p[1], "model-changes") {
			told = append(told, p[0])
			for _, want := range []string{
				"[FROM: watch]",
				"the Claude Code session of j-w (worker, job j) has 3 model event(s)",
				"2030-01-02T03:00:02.000Z stopped by a safety classifier (stop_reason refusal) on claude-test-large (record a-2)",
				"2030-01-02T03:00:03.000Z model_refusal_fallback: claude-test-large -> claude-test-small (record f-1)",
				"2030-01-02T03:00:04.000Z model_change: claude-test-large -> claude-test-small, the session started on " +
					"claude-test-large (record a-4)",
				"fleet only reports this",
			} {
				if !strings.Contains(p[1], want) {
					t.Errorf("%q missing from the message to %s:\n%s", want, p[0], p[1])
				}
			}
		}
	}
	if strings.Join(told, " ") != "j-lead" {
		t.Errorf("told %q, want j-lead alone", told)
	}
	posts := w.posts()
	if len(posts) != 2 {
		t.Fatalf("posts %q", posts)
	}
	for i, want := range [][]string{
		{"C1/1.0 fleet watch：thread-c1 的 Claude Code 会话出现了 2 个模型事件",
			"2030-01-02T04:00:01.000Z 被 safety classifier 拦停 (`stop_reason: refusal`)，模型 `claude-test-large`，记录 `t-2`",
			"2030-01-02T04:00:01.500Z 被拦停，没有降级 (`model_refusal_no_fallback`)，模型 `claude-test-large`，记录 `t-3`"},
		{"C1/1.0 fleet watch：j-w 的 Claude Code 会话出现了 3 个模型事件",
			"降级 (`model_refusal_fallback`)：`claude-test-large` → `claude-test-small`，记录 `f-1`",
			"模型从 `claude-test-large` 换成了 `claude-test-small` (会话开始时是 `claude-test-large`)，记录 `a-4`"},
	} {
		for _, line := range want {
			if !strings.Contains(posts[i], line) {
				t.Errorf("%q missing from post %d:\n%s", line, i, posts[i])
			}
		}
	}
	for _, name := range []string{"j-ok", "j-pi", "j-lead"} {
		if strings.Contains(out.stdout, "model event: "+name) {
			t.Errorf("%s reported:\n%s", name, out.stdout)
		}
	}

	// The next run finds nothing new and reports nothing again.
	before, logged := len(w.prompts()), len(w.posts())
	if out := w.watchAt(2*time.Minute, 0); strings.Contains(out.stdout, "model event") {
		t.Fatalf("%+v", out)
	}
	if len(w.prompts()) != before || len(w.posts()) != logged {
		t.Errorf("reported again: %q %q", w.prompts()[before:], w.posts()[logged:])
	}

	// What the transcript gains after is read from where the last run
	// stopped: only the new change is reported.
	w.appendRecords(sessionWorker, assistant("a-5", "2030-01-02T07:00:00.000Z", "msg-5", "claude-test-large", "end_turn"))
	w.watchAt(3*time.Minute, 0)
	prompts := w.prompts()
	if len(prompts) != before+1 || !strings.Contains(prompts[before][1], "has 1 model event(s)") ||
		!strings.Contains(prompts[before][1], "model_change: claude-test-small -> claude-test-large, the session started on "+
			"claude-test-large (record a-5)") {
		t.Errorf("after the append: %q", prompts[before:])
	}
	if posts := w.posts(); len(posts) != logged+1 || !strings.Contains(posts[logged], "记录 `a-5`") {
		t.Errorf("after the append: %q", posts[logged:])
	}
}

// A tell that does not land is tried again next run, without posting the
// same events to the thread a second time.
func TestTheModelWatchTellsABlockedParentNextRun(t *testing.T) {
	w := modelWorld(t)
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
		w.query("SELECT count(*) FROM transcripts") != "0" {
		t.Errorf("%+v, posts %q", out, w.posts())
	}
}

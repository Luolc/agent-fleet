package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const threadAgent = "thread-c0123-1700000000-123"

// `send` takes a thread key for <to>: the message goes to the thread's
// live thread agent, named by the ledger.
func TestSendTakesAThreadKeyForItsLiveThreadAgent(t *testing.T) {
	w := threadWorld(t, "")
	if out := w.inbox(w.event("m1", "first", "")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	_ = os.Remove(filepath.Join(w.dir, "prompts"))
	out := w.run("tell the thread 0xKEY\n", []string{"send", threadKey}, "FLEET_AGENT=x-worker")
	if out.code != 0 || !strings.Contains(out.stdout, "delivered to "+threadAgent) {
		t.Fatalf("send: %+v", out)
	}
	prompts, _ := os.ReadFile(filepath.Join(w.dir, "prompts"))
	if !strings.Contains(string(prompts), threadAgent) || !strings.Contains(string(prompts), "0xKEY") {
		t.Errorf("prompts = %q", prompts)
	}
}

// A thread key with no live thread agent is exit 4, nothing sent, and the
// message says what to do; an unknown thread says so.
func TestSendToAThreadKeyWithoutALiveAgentIsExit4AndSaysWhatToDo(t *testing.T) {
	w := threadWorld(t, "")
	if out := w.inbox(w.event("m1", "first", "")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	conn := w.defaultLedger()
	defer conn.Close()
	if _, err := conn.Exec("UPDATE agents SET state = 'ended' WHERE role = 'thread'"); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(w.dir, "prompts"))
	out := w.run("hello\n", []string{"send", threadKey}, "FLEET_AGENT=x-worker")
	if out.code != 4 || !strings.Contains(out.stderr, "has no live thread agent") ||
		!strings.Contains(out.stderr, "fleet thread post --deliver --thread "+threadKey) {
		t.Errorf("ended: %+v", out)
	}
	out = w.run("hello\n", []string{"send", "C0999/1700000999.000"}, "FLEET_AGENT=x-worker")
	if out.code != 4 || !strings.Contains(out.stderr, "is not in this scope's ledger") {
		t.Errorf("unknown: %+v", out)
	}
	if _, err := os.Stat(filepath.Join(w.dir, "prompts")); err == nil {
		t.Errorf("a prompt was sent")
	}
}

// `thread post --deliver` to a thread of this machine is refused only
// while that thread has a live agent, and the refusal names the key to
// send to.
func TestDeliverToALocalThreadWithoutALiveAgentPosts(t *testing.T) {
	w := threadWorld(t, "")
	if out := w.inbox(w.event("m1", "first", "")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	body := task(w, "post.md", "Hello.\n")
	out := w.asThreadAgent("TH-5", "thread", "post", "--deliver", "--thread", threadKey, "--body-file", body)
	if out.code != 1 || !strings.Contains(out.stderr, "fleet send "+threadKey) {
		t.Errorf("live: %+v", out)
	}
	// The caller's row moves to another thread, so threadKey has no live agent.
	conn := w.defaultLedger()
	defer conn.Close()
	if _, err := conn.Exec("UPDATE agents SET thread = 'C0B/1700000009.000' WHERE role = 'thread'"); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	out = w.run("", []string{"thread", "post", "--deliver", "--thread", threadKey, "--body-file", body},
		"FLEET_AGENT="+threadAgent, "FLEET_ROLE=thread", "FLEET_SCOPE=main", "FLEET_THREAD=C0B/1700000009.000", "FLEET_ISSUE=TH-5", linearKey)
	if out.code != 0 || !strings.Contains(w.calls(), "fednet client post -socket /run/fednet.sock -thread "+threadKey+" -deliver -- Hello.") {
		t.Errorf("no live agent: %+v calls %q", out, w.calls())
	}
}

// `status --json` and the table show a thread agent's thread key.
func TestStatusShowsAThreadAgentsThreadKey(t *testing.T) {
	w := threadWorld(t, "")
	if out := w.inbox(w.event("m1", "first", "")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	out := w.run("", []string{"status", "--json"})
	if out.code != 0 || !strings.Contains(out.stdout, `"thread": "`+threadKey+`"`) {
		t.Errorf("json: %+v", out)
	}
	out = w.run("", []string{"status"})
	if out.code != 0 || !strings.Contains(out.stdout, "THREAD") || !strings.Contains(out.stdout, threadAgent+"  thread  -    -       "+threadKey) {
		t.Errorf("table: %+v", out)
	}
}

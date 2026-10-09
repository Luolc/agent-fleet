// Integration tests for `inbox`, `thread end`, `thread set-project` and
// `thread relate` against a fake herdr, a fake atb and a fake fednet,
// all logging their calls to <dir>/calls in order. Hermetic, see
// main_test.go; the real herdr runs them in the judge.
package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Luolc/agent-fleet/internal/db"
)

// threadHerdr is a fake herdr for the thread arms. It takes the
// `--session` the hook adds (recorded in <dir>/session), lists a
// `threads` workspace once <dir>/threads-workspace exists, says an agent
// exists when <dir>/has-<name> exists, and lets a start run to the end.
const threadHerdr = `#!/bin/sh
dir="$(dirname "$0")/.."
if [ "$1" = --session ]; then echo "$2" > "$dir/session"; shift 2; fi
echo "herdr $1 $2" >> "$dir/calls"
case "$1 $2" in
  "agent list") echo '{"result":{"agents":[]}}' ;;
  "agent get")
    if [ -e "$dir/has-$3" ]; then
      echo '{"result":{"agent":{"agent_status":"idle","tab_id":"t9","pane_id":"p9"}}}'
    else
      echo '{"error":{"code":"agent_not_found","message":"no agent '"$3"'"}}'
    fi ;;
  "workspace list")
    if [ -e "$dir/threads-workspace" ]; then
      echo '{"result":{"workspaces":[{"workspace_id":"w1","label":"item-1"},{"workspace_id":"w7","label":"threads"}]}}'
    else
      echo '{"result":{"workspaces":[{"workspace_id":"w1","label":"item-1"}]}}'
    fi ;;
  "workspace create")
    printf '%s\n' "$@" > "$dir/workspace-argv"
    echo '{"result":{"root_pane":{"workspace_id":"w7","tab_id":"t7","pane_id":"p7"}}}' ;;
  "tab create")
    printf '%s\n' "$@" > "$dir/tab-argv"
    echo '{"result":{"root_pane":{"workspace_id":"w7","tab_id":"t8","pane_id":"p8"}}}' ;;
  "agent start") : > "$dir/has-$3"; echo '{"result":{}}' ;;
  "pane rename")
    if [ -e "$dir/kill-at-rename" ]; then rm "$dir/kill-at-rename"; kill -9 $PPID; sleep 1; fi
    echo '{"result":{}}' ;;
  "tab rename") echo '{"result":{}}' ;;
  "tab close")
    if [ -e "$dir/tab-close-fails" ]; then rm "$dir/tab-close-fails"; echo '{"error":{"code":"internal","message":"tab busy"}}'; exit 1; fi
    echo "$3" > "$dir/closed-tab"; echo '{"result":{}}' ;;
  "agent read") printf '%s\n' "────────────" "❯ " "────────────" ;;
  "agent prompt")
    printf '%s\n' "$@" > "$dir/argv"
    echo '{"id":"cli:agent:prompt","result":{"type":"agent_prompted"}}' ;;
  *) echo "fake herdr: unexpected command: $*" >&2; exit 2 ;;
esac
`

// threadAtb is a fake atb: every call logged; `create` prints TH-5;
// `query` prints the comments of a ticket, two of them session summaries;
// the subcommand named by failOn exits 4.
func threadAtb(failOn string) string {
	return `#!/bin/sh
dir="$(dirname "$0")/.."
key=unset
[ -n "$LINEAR_API_KEY" ] && key=set
echo "atb $* key=$key" >> "$dir/calls"
if [ "$2" = "` + failOn + `" ]; then
  # The first failure is atb's own (exit 1); after it the issue has no
  # holder, so the retry is exit 4, as atb 0.2.3 answers.
  if [ -e "$dir/failed-once" ]; then echo "error: refused: no holder" >&2; exit 4; fi
  : > "$dir/failed-once"; echo "error: Linear unreachable" >&2; exit 1
fi
case "$2" in
  create) echo '{"identifier":"TH-5","url":"https://linear.example.test/TH-5"}' ;;
  comment) cat "$5" > "$dir/comment-$3" ;;
  query) printf '%s\n' '{"issue":{"comments":{"nodes":[{"body":"claim: thread-x","createdAt":"2026-10-09T01:00:00Z"},{"body":"Session 2 ended\n\nSecond: 0xSUM2","createdAt":"2026-10-09T03:00:00Z"},{"body":"Session 1 ended\n\nFirst: 0xSUM1","createdAt":"2026-10-09T02:00:00Z"}]}}}' ;;
esac
exit 0
`
}

// threadFednet is a fake fednet: every call logged; the post fails while
// <dir>/fednet-down exists.
const threadFednet = `#!/bin/sh
dir="$(dirname "$0")/.."
echo "fednet $*" >> "$dir/calls"
[ -e "$dir/fednet-down" ] && { echo "post: hub unreachable" >&2; exit 4; }
echo m-posted
`

// threadWorld is a world with the three fakes on PATH and the default
// target configured with a Linear team and a fednet socket.
func threadWorld(t *testing.T, atbFailOn string) *world {
	t.Helper()
	w := newWorld(t)
	for name, script := range map[string]string{"herdr": threadHerdr, "atb": threadAtb(atbFailOn), "fednet": threadFednet} {
		if err := os.WriteFile(filepath.Join(w.dir, "fake-herdr", name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w.targetConfig(`{"linear": {"team": "TH"}, "fednet": {"socket": "/run/fednet.sock"}}`)
	return w
}

// targetConfig writes the default target's config file.
func (w *world) targetConfig(body string) {
	w.t.Helper()
	dir := filepath.Join(w.dir, "home", ".config", "fleet")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "default.json"), []byte(body), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

const threadKey = "C0123/1700000000.123"

// event writes an event file for a message in threadKey.
func (w *world) event(msgID, text, context string) string {
	w.t.Helper()
	body := `{"msg_id":"` + msgID + `","payload":{"type":"message","thread":"` + threadKey + `","text":"` + text +
		`","user":"U0ABC","ts":"1700000001.000","context":"` + context + `"}}`
	return task(w, msgID+".json", body)
}

func (w *world) inbox(file string) result {
	w.t.Helper()
	return w.run("", []string{"inbox", file}, linearKey)
}

// defaultLedger opens the default target's ledger, the one `inbox` uses.
func (w *world) defaultLedger() *sql.DB {
	w.t.Helper()
	path, err := db.PathUnder(filepath.Join(w.dir, "home", ".local", "state"), "default")
	if err != nil {
		w.t.Fatal(err)
	}
	conn, err := db.OpenAt(path)
	if err != nil {
		w.t.Fatal(err)
	}
	return conn
}

// threadAgentRow is the issue and state of the newest thread agent row,
// or "none".
func threadAgentRow(w *world) string {
	w.t.Helper()
	conn := w.defaultLedger()
	defer conn.Close()
	var issue, state string
	if err := conn.QueryRow("SELECT issue, state FROM agents WHERE role = 'thread' ORDER BY id DESC LIMIT 1").Scan(&issue, &state); err != nil {
		return "none"
	}
	return issue + " " + state
}

// threadRow is the ticket, sessions and context of the thread, or "none".
func threadRow(w *world) string {
	w.t.Helper()
	conn := w.defaultLedger()
	defer conn.Close()
	var ticket, context string
	var sessions int64
	if err := conn.QueryRow("SELECT ticket, sessions, context FROM threads WHERE thread = ?1", threadKey).Scan(
		&ticket, &sessions, &context); err != nil {
		return "none"
	}
	return ticket + " " + strings.Repeat("s", int(sessions)) + " " + context
}

// inboxRow is the state of the message, or "none".
func inboxRow(w *world, msgID string) string {
	w.t.Helper()
	conn := w.defaultLedger()
	defer conn.Close()
	var state string
	if err := conn.QueryRow("SELECT state FROM inbox WHERE msg_id = ?1", msgID).Scan(&state); err != nil {
		return "none"
	}
	return state
}

func (w *world) file(name string) string {
	data, _ := os.ReadFile(filepath.Join(w.dir, name))
	return string(data)
}

func TestInboxStartsAThreadAgentForANewThreadWithItsTicket(t *testing.T) {
	w := threadWorld(t, "")
	out := w.inbox(w.event("m1", "Please import the A table", "The data channel"))
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	want := []string{
		"atb linear create --team TH --label thread --title Please import the A table --description-file " +
			"DESC --json key=set",
		"atb linear claim TH-5 --agent thread-c0123-1700000000-123 --source " + threadKey +
			" --scope default: thread " + threadKey + " key=set",
		"herdr workspace list",
		"herdr workspace create",
		"herdr tab rename",
		"herdr agent start",
		"herdr pane rename",
		"herdr agent read",
		"herdr agent get",
		"herdr agent prompt",
	}
	got := strings.Split(strings.TrimSpace(w.calls()), "\n")
	for i := range got {
		if strings.HasPrefix(got[i], "atb linear create") {
			head, _, _ := strings.Cut(got[i], "--description-file ")
			got[i] = head + "--description-file DESC --json key=set"
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls = %q, want %q", got, want)
	}
	if got := w.file("session"); strings.TrimSpace(got) != "default" {
		t.Errorf("session = %q, want the target's name", got)
	}
	cwd := filepath.Join(w.dir, "home", "cross-repo", "threads")
	argv := w.file("workspace-argv")
	for _, want := range []string{"--label\nthreads\n", "--cwd\n" + cwd + "\n", "--env\nFLEET_AGENT=thread-c0123-1700000000-123\n",
		"--env\nFLEET_ROLE=thread\n", "--env\nFLEET_TARGET=default\n", "--env\nFLEET_ISSUE=TH-5\n",
		"--env\nFLEET_THREAD=" + threadKey + "\n"} {
		if !strings.Contains(argv, want) {
			t.Errorf("workspace create argv = %q, want %q in it", argv, want)
		}
	}
	if !dirExists(cwd) {
		t.Errorf("%s was not made", cwd)
	}
	prompt := w.file("argv")
	for _, want := range []string{"[FROM: inbox]\nYou are a thread agent", "FLEET_ISSUE=TH-5 (your thread ticket, https://linear.example.test/TH-5)",
		"fednet client post -socket /run/fednet.sock -thread " + threadKey, "## Channel context\n\nThe data channel\n",
		"## The message\n\nMessage in thread " + threadKey + " from U0ABC at 1700000001.000:\n\nPlease import the A table\n"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt = %q, want %q in it", prompt, want)
		}
	}
	if strings.Contains(prompt, "Earlier sessions") {
		t.Error("a new thread got earlier sessions")
	}
	if got := threadAgentRow(w); got != "TH-5 active" {
		t.Errorf("row = %q", got)
	}
	if got := threadRow(w); got != "TH-5 s The data channel" {
		t.Errorf("thread row = %q", got)
	}
	if got := inboxRow(w, "m1"); got != "delivered" {
		t.Errorf("inbox row = %q", got)
	}

	// The same message again does nothing: no herdr, no atb.
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	out = w.inbox(w.event("m1", "Please import the A table", ""))
	if out.code != 0 || w.calls() != "" || !strings.Contains(out.stdout, "already delivered") {
		t.Errorf("rerun: %+v, calls %q", out, w.calls())
	}

	// A second message goes to the live agent as `send` would.
	if err := os.WriteFile(filepath.Join(w.dir, "has-thread-c0123-1700000000-123"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out = w.inbox(w.event("m2", "And the B table", ""))
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if got := strings.TrimSpace(w.calls()); got != "herdr agent get\nherdr agent prompt" {
		t.Errorf("calls = %q", got)
	}
	argv = w.file("argv")
	if !strings.HasPrefix(argv, "agent\nprompt\nthread-c0123-1700000000-123\n[FROM: inbox]\nMessage in thread "+threadKey+
		" from U0ABC at 1700000001.000:\n\nAnd the B table\n") || strings.Contains(argv, "You are a thread agent") {
		t.Errorf("argv = %q", argv)
	}
	if got := inboxRow(w, "m2"); got != "delivered" {
		t.Errorf("inbox row = %q", got)
	}
}

func TestInboxReopensAKnownThreadWithItsSummariesWhenTheAgentIsGone(t *testing.T) {
	w := threadWorld(t, "")
	if out := w.inbox(w.event("m1", "first", "ctx")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	// The agent ended its session (row ended), the workspace stays.
	conn := w.defaultLedger()
	if _, err := conn.Exec("UPDATE agents SET state = 'ended' WHERE role = 'thread'"); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if err := os.WriteFile(filepath.Join(w.dir, "threads-workspace"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	out := w.inbox(w.event("m2", "are we done?", ""))
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	want := []string{
		"atb linear claim TH-5 --agent thread-c0123-1700000000-123 --source " + threadKey +
			" --scope default: thread " + threadKey + " key=set",
		"atb linear comment TH-5 --body-file BODY key=set",
		`atb linear query { issue(id: "TH-5") { comments { nodes { body createdAt } } } } key=set`,
		"herdr workspace list",
		"herdr tab create",
		"herdr agent start",
		"herdr pane rename",
		"herdr agent read",
		"herdr agent get",
		"herdr agent prompt",
	}
	got := strings.Split(strings.TrimSpace(w.calls()), "\n")
	for i := range got {
		if strings.HasPrefix(got[i], "atb linear comment") {
			got[i] = "atb linear comment TH-5 --body-file BODY key=set"
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls = %q, want %q", got, want)
	}
	if got := w.file("comment-TH-5"); got != "Session 2 started\n\nTriggered by a message from U0ABC at 1700000001.000.\n" {
		t.Errorf("comment = %q", got)
	}
	if !strings.Contains(w.file("tab-argv"), "--workspace\nw7\n--label\nc0123-1700000000-123\n") {
		t.Errorf("tab argv = %q", w.file("tab-argv"))
	}
	prompt := w.file("argv")
	for _, want := range []string{"## Channel context\n\nctx\n",
		"## Earlier sessions on this thread\n\nSession 1 ended\n\nFirst: 0xSUM1\n\nSession 2 ended\n\nSecond: 0xSUM2\n",
		"## The message\n\nMessage in thread " + threadKey + " from U0ABC at 1700000001.000:\n\nare we done?\n"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt = %q, want %q in it", prompt, want)
		}
	}
	if got := threadRow(w); got != "TH-5 ss ctx" {
		t.Errorf("thread row = %q", got)
	}

	// A live row whose agent herdr does not know is ended and a new
	// session started; the old row stays ended.
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	_ = os.Remove(filepath.Join(w.dir, "has-thread-c0123-1700000000-123"))
	out = w.inbox(w.event("m3", "still there?", ""))
	if out.code != 0 || !strings.Contains(out.stderr, "gone from herdr") {
		t.Fatalf("%+v", out)
	}
	if got := strings.TrimSpace(w.calls()); !strings.HasPrefix(got, "herdr agent get\natb linear claim") {
		t.Errorf("calls = %q", got)
	}
	conn = w.defaultLedger()
	defer conn.Close()
	var live, ended int64
	if err := conn.QueryRow("SELECT count(*) FROM agents WHERE role = 'thread' AND state != 'ended'").Scan(&live); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow("SELECT count(*) FROM agents WHERE role = 'thread' AND state = 'ended'").Scan(&ended); err != nil {
		t.Fatal(err)
	}
	if live != 1 || ended != 2 {
		t.Errorf("live %d, ended %d", live, ended)
	}
}

func TestInboxDropsTheMessageAndTellsTheThreadWhenLinearIsUnavailable(t *testing.T) {
	w := threadWorld(t, "create")
	out := w.inbox(w.event("m1", "hello", ""))
	if out.code != 0 || !strings.Contains(out.stderr, "Linear is unavailable") {
		t.Fatalf("%+v", out)
	}
	got := strings.Split(strings.TrimSpace(w.calls()), "\n")
	if len(got) != 2 || !strings.HasPrefix(got[0], "atb linear create") ||
		got[1] != "fednet client post -socket /run/fednet.sock -thread "+threadKey+" -- Linear is unavailable right now, "+
			"so no agent was started for this thread; please try again later." {
		t.Errorf("calls = %q", got)
	}
	if got := threadAgentRow(w); got != " ended" {
		t.Errorf("row = %q, want ended", got)
	}
	if got, want := threadRow(w), " "; !strings.HasPrefix(got, want) || strings.Contains(got, "s") {
		t.Errorf("thread row = %q, want no ticket and no session", got)
	}
	if got := inboxRow(w, "m1"); got != "dropped" {
		t.Errorf("inbox row = %q", got)
	}
	// Dropped stays dropped on a rerun.
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	if out := w.inbox(w.event("m1", "hello", "")); out.code != 0 || w.calls() != "" {
		t.Errorf("rerun: %+v, calls %q", out, w.calls())
	}
}

func TestInboxRunsWithoutLinearAndIgnoresOtherPayloads(t *testing.T) {
	w := threadWorld(t, "")
	w.targetConfig(`{}`)
	out := w.inbox(w.event("m1", "hello", ""))
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if got := w.calls(); strings.Contains(got, "atb") {
		t.Errorf("atb was called: %q", got)
	}
	if !strings.Contains(w.file("argv"), "FLEET_ISSUE= (empty: thread tickets are off for this target)") ||
		!strings.Contains(w.file("argv"), "the fednet socket is not configured") {
		t.Errorf("prompt = %q", w.file("argv"))
	}
	if got := threadRow(w); got != " s " {
		t.Errorf("thread row = %q", got)
	}

	_ = os.Remove(filepath.Join(w.dir, "calls"))
	approval := task(w, "a.json", `{"msg_id":"m2","payload":{"type":"approval","outcome":"approved"}}`)
	out = w.inbox(approval)
	if out.code != 0 || w.calls() != "" || !strings.Contains(out.stdout, "ignored m2") {
		t.Errorf("approval: %+v, calls %q", out, w.calls())
	}
	if got := inboxRow(w, "m2"); got != "none" {
		t.Errorf("an ignored payload got a row: %q", got)
	}
	for _, bad := range []string{`not json`, `{"msg_id":"","payload":{"type":"message"}}`, `{"msg_id":"m3","payload":{"type":"message"}}`} {
		if out := w.inbox(task(w, "bad.json", bad)); out.code != 1 {
			t.Errorf("%s: %+v", bad, out)
		}
	}
	if out := w.inbox(filepath.Join(w.dir, "missing.json")); out.code != 1 {
		t.Errorf("missing file: %+v", out)
	}
	out = w.inbox(task(w, "t.json", `{"msg_id":"m4","payload":{"type":"message","thread":"C1/1.1","text":"x","target":"a/b"}}`))
	if out.code != 1 || !strings.Contains(out.stderr, "directory name") {
		t.Errorf("bad target: %+v", out)
	}
}

func TestInboxRunsAThreadAgentOfAnotherTargetInItsCheckout(t *testing.T) {
	w := threadWorld(t, "")
	w.configure(`{}`)
	ev := task(w, "e.json", `{"msg_id":"m1","payload":{"type":"message","thread":"C1/1.1","text":"x","target":"example-dataset"}}`)
	out := w.inbox(ev)
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	cwd := filepath.Join(w.dir, "home", "dev", "example-dataset")
	if !strings.Contains(w.file("workspace-argv"), "--cwd\n"+cwd+"\n--env\nFLEET_AGENT=thread-c1-1-1\n") ||
		!strings.Contains(w.file("workspace-argv"), "--env\nFLEET_TARGET=example-dataset\n") {
		t.Errorf("workspace argv = %q", w.file("workspace-argv"))
	}
	if got := strings.TrimSpace(w.file("session")); got != "example-dataset" {
		t.Errorf("session = %q", got)
	}
	ev = task(w, "e2.json", `{"msg_id":"m2","payload":{"type":"message","thread":"C1/1.1","text":"x","target":"no-such"}}`)
	if out := w.inbox(ev); out.code != 5 || !strings.Contains(out.stderr, "no checkout") {
		t.Errorf("missing checkout: %+v", out)
	}
}

// asThreadAgent runs the binary as the thread agent of threadKey, with
// the ticket `issue`.
func (w *world) asThreadAgent(issue string, args ...string) result {
	w.t.Helper()
	return w.run("", args, "FLEET_AGENT=thread-c0123-1700000000-123", "FLEET_ROLE=thread", "FLEET_TARGET=default",
		"FLEET_THREAD="+threadKey, "FLEET_ISSUE="+issue, linearKey)
}

func TestThreadEndWritesTheSummaryReleasesEndsTheRowAndClosesTheTabLast(t *testing.T) {
	w := threadWorld(t, "")
	if out := w.inbox(w.event("m1", "first", "")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if err := os.WriteFile(filepath.Join(w.dir, "has-thread-c0123-1700000000-123"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	summary := task(w, "summary.md", "Started job item-1 for the import.\n")
	out := w.asThreadAgent("TH-5", "thread", "end", "--summary-file", summary)
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	got := strings.Split(strings.TrimSpace(w.calls()), "\n")
	got[0] = strings.SplitN(got[0], " --body-file", 2)[0]
	want := []string{"atb linear comment TH-5",
		"atb linear release TH-5 --agent thread-c0123-1700000000-123 --reason done --done key=set",
		"herdr agent get", "herdr tab close"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls = %q, want %q", got, want)
	}
	if got := w.file("comment-TH-5"); got != "Session 1 ended\n\nStarted job item-1 for the import.\n" {
		t.Errorf("comment = %q", got)
	}
	if got := strings.TrimSpace(w.file("closed-tab")); got != "t9" {
		t.Errorf("closed tab = %q", got)
	}
	if got := threadAgentRow(w); got != "TH-5 ended" {
		t.Errorf("row = %q", got)
	}
	if !strings.Contains(out.stdout, "ended session 1 of thread "+threadKey) {
		t.Errorf("stdout = %q", out.stdout)
	}
}

func TestThreadEndRefusalsAndAtbFailureChangeNothing(t *testing.T) {
	w := threadWorld(t, "release")
	if out := w.inbox(w.event("m1", "first", "")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	summary := task(w, "summary.md", "done\n")
	for _, c := range []struct {
		label string
		out   result
	}{
		{"a lead", w.asAgent("item-1-lead", "lead", "thread-1", "item-1", "thread", "end", "--summary-file", summary)},
		{"a thread agent without FLEET_THREAD", w.asThread("thread", "end", "--summary-file", summary)},
		{"an empty summary", w.asThreadAgent("TH-5", "thread", "end", "--summary-file", task(w, "empty.md", "\n"))},
		{"a missing summary", w.asThreadAgent("TH-5", "thread", "end", "--summary-file", filepath.Join(w.dir, "no.md"))},
		{"no summary flag", w.asThreadAgent("TH-5", "thread", "end")},
	} {
		if c.out.code != 1 {
			t.Errorf("%s: %+v", c.label, c.out)
		}
	}
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	out := w.asThreadAgent("TH-5", "thread", "end", "--summary-file", summary)
	if out.code != 5 || !strings.Contains(out.stderr, "atb linear release") {
		t.Errorf("%+v", out)
	}
	if got := threadAgentRow(w); got != "TH-5 active" {
		t.Errorf("row = %q, want active", got)
	}
	if strings.Contains(w.calls(), "herdr") {
		t.Errorf("herdr was called: %q", w.calls())
	}
	// Without a ticket there is no Linear step.
	w2 := threadWorld(t, "")
	w2.targetConfig(`{}`)
	if out := w2.inbox(w2.event("m1", "first", "")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if err := os.WriteFile(filepath.Join(w2.dir, "has-thread-c0123-1700000000-123"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(w2.dir, "calls"))
	if out := w2.asThreadAgent("", "thread", "end", "--summary-file", task(w2, "s.md", "bye\n")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if got := strings.TrimSpace(w2.calls()); got != "herdr agent get\nherdr tab close" {
		t.Errorf("calls = %q", got)
	}
}

func TestThreadSetProjectAndRelateActOnTheCallersTicket(t *testing.T) {
	w := threadWorld(t, "")
	out := w.asThreadAgent("TH-5", "thread", "set-project", "Example project")
	if out.code != 0 || out.stdout != "TH-5 is in project Example project\n" {
		t.Errorf("%+v", out)
	}
	out = w.asThreadAgent("TH-5", "thread", "relate", "EX-10")
	if out.code != 0 || out.stdout != "TH-5 is related to EX-10\n" {
		t.Errorf("%+v", out)
	}
	if got := strings.TrimSpace(w.calls()); got != "atb linear set-project TH-5 --project Example project key=set\n"+
		"atb linear relate TH-5 EX-10 key=set" {
		t.Errorf("calls = %q", got)
	}
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	for _, c := range []struct {
		label string
		out   result
	}{
		{"no ticket", w.asThreadAgent("", "thread", "set-project", "P")},
		{"a lead", w.asAgent("item-1-lead", "lead", "thread-1", "item-1", "thread", "relate", "EX-10")},
		{"a bad issue", w.asThreadAgent("TH-5", "thread", "relate", "ex10")},
		{"an empty project", w.asThreadAgent("TH-5", "thread", "set-project", " ")},
		{"no subcommand", w.asThreadAgent("TH-5", "thread")},
	} {
		if c.out.code != 1 {
			t.Errorf("%s: %+v", c.label, c.out)
		}
	}
	if w.calls() != "" {
		t.Errorf("atb was called: %q", w.calls())
	}
}

func TestJobStartFromAThreadAgentRecordsTheHomeThreadAndRelatesTheTicket(t *testing.T) {
	w := newWorld(t)
	w.configure(withLinear)
	w.useStartHerdr()
	w.fakeAtbCreating("")
	taskFile := task(w, "task.md", "Import the A table\n")
	out := w.run("", []string{"job", "start", "item-7", "--repo", "example-dataset", "--parent-issue", "EX-10", "--task-file", taskFile},
		"FLEET_AGENT=thread-c0123-1700000000-123", "FLEET_ROLE=thread", "FLEET_TARGET="+target,
		"FLEET_THREAD="+threadKey, "FLEET_ISSUE=TH-5", linearKey)
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if !strings.Contains(w.calls(), "atb linear claim EX-12 --agent item-7-lead --source thread-c0123-1700000000-123 "+
		"--scope example-dataset: job item-7 key=set\natb linear relate TH-5 EX-10 key=set\nherdr workspace create") {
		t.Errorf("calls = %q", w.calls())
	}
	conn := w.ledger()
	defer conn.Close()
	var home string
	if err := conn.QueryRow("SELECT home_thread FROM jobs WHERE job = 'item-7'").Scan(&home); err != nil {
		t.Fatal(err)
	}
	if home != threadKey {
		t.Errorf("home_thread = %q", home)
	}
	if strings.Contains(w.file("workspace-argv"), "FLEET_THREAD") {
		t.Error("the lead's pane got FLEET_THREAD")
	}
	out = w.startJob("threads", "--task-file", taskFile)
	if out.code != 1 || !strings.Contains(out.stderr, "reserved") {
		t.Errorf("job named threads: %+v", out)
	}
}

func TestInboxFinishesAStartItsEarlierRunWasKilledIn(t *testing.T) {
	w := threadWorld(t, "")
	if out := w.inbox(w.event("m0", "first", "ctx")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	// The agent ended its session; the next message reopens the thread,
	// and that run is killed by the fake herdr at `pane rename`: the agent
	// was started, the prompt never delivered.
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
	out := w.inbox(w.event("m1", "are we done? 0xMSG1", ""))
	if out.code == 0 || w.file("argv") != "" {
		t.Fatalf("the killed run finished: %+v, argv %q", out, w.file("argv"))
	}
	if got := threadAgentRow(w); got != "TH-5 starting" {
		t.Fatalf("row after the kill = %q", got)
	}
	if got := inboxRow(w, "m1"); got != "reserved" {
		t.Fatalf("inbox row after the kill = %q", got)
	}
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	out = w.inbox(w.event("m1", "are we done? 0xMSG1", ""))
	if out.code != 0 || !strings.Contains(out.stderr, "still starting from an earlier run") {
		t.Fatalf("retry: %+v", out)
	}
	want := "herdr agent get\n" +
		`atb linear query { issue(id: "TH-5") { comments { nodes { body createdAt } } } } key=set` + "\n" +
		"herdr pane rename\nherdr agent read\nherdr agent get\nherdr agent prompt"
	if got := strings.TrimSpace(w.calls()); got != want {
		t.Errorf("retry calls = %q, want %q", got, want)
	}
	prompt := w.file("argv")
	for _, want := range []string{"[FROM: inbox]\nYou are a thread agent", "FLEET_ISSUE=TH-5 (your thread ticket",
		"## Channel context\n\nctx\n", "## Earlier sessions on this thread\n\nSession 1 ended", "0xMSG1"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("retry prompt = %q, want %q in it", prompt, want)
		}
	}
	if got := threadAgentRow(w); got != "TH-5 active" {
		t.Errorf("row after the retry = %q", got)
	}
	if got := inboxRow(w, "m1"); got != "delivered" {
		t.Errorf("inbox row after the retry = %q", got)
	}
	if got := threadRow(w); got != "TH-5 ss ctx" {
		t.Errorf("thread row = %q, want two sessions, not three", got)
	}
}

func TestInboxKeepsTheMessageWhenTheOutageNoticeCannotBePosted(t *testing.T) {
	w := threadWorld(t, "create")
	if err := os.WriteFile(filepath.Join(w.dir, "fednet-down"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out := w.inbox(w.event("m1", "hello", ""))
	if out.code != 5 || !strings.Contains(out.stderr, "the thread was not told") {
		t.Fatalf("%+v", out)
	}
	if got := inboxRow(w, "m1"); got != "reserved" {
		t.Errorf("inbox row = %q, want reserved", got)
	}
	if got := threadAgentRow(w); got != " ended" {
		t.Errorf("row = %q, want ended", got)
	}
	// Linear still down, the post works again: dropped now.
	_ = os.Remove(filepath.Join(w.dir, "fednet-down"))
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	out = w.inbox(w.event("m1", "hello", ""))
	if out.code != 0 || !strings.Contains(w.calls(), "fednet client post") {
		t.Errorf("retry: %+v, calls %q", out, w.calls())
	}
	if got := inboxRow(w, "m1"); got != "dropped" {
		t.Errorf("inbox row = %q, want dropped", got)
	}
	// No socket configured: nothing to post with, kept as well.
	w2 := threadWorld(t, "create")
	w2.targetConfig(`{"linear": {"team": "TH"}}`)
	out = w2.inbox(w2.event("m1", "hello", ""))
	if out.code != 5 || !strings.Contains(out.stderr, "fednet.socket is not configured") {
		t.Errorf("no socket: %+v", out)
	}
	if got := inboxRow(w2, "m1"); got != "reserved" {
		t.Errorf("inbox row = %q, want reserved", got)
	}
}

// stepsOf are the recorded steps of the newest thread agent row's ending.
func stepsOf(w *world) string {
	w.t.Helper()
	conn := w.defaultLedger()
	defer conn.Close()
	var id int64
	if err := conn.QueryRow("SELECT id FROM agents WHERE role = 'thread' ORDER BY id DESC LIMIT 1").Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	rows, err := conn.Query("SELECT step FROM steps WHERE key = ?1 ORDER BY id", "thread-end:"+strconv.FormatInt(id, 10))
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	var steps []string
	for rows.Next() {
		var step string
		if err := rows.Scan(&step); err != nil {
			w.t.Fatal(err)
		}
		steps = append(steps, step)
	}
	return strings.Join(steps, " ")
}

func TestThreadEndResumesAfterAPartialReleaseAndForceFinishesLocally(t *testing.T) {
	w := threadWorld(t, "release")
	if out := w.inbox(w.event("m1", "first", "")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if err := os.WriteFile(filepath.Join(w.dir, "has-thread-c0123-1700000000-123"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	summary := task(w, "summary.md", "bye\n")
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	// The comment is written, the release fails: exit 5, the comment
	// recorded, the row live, the tab open.
	out := w.asThreadAgent("TH-5", "thread", "end", "--summary-file", summary)
	if out.code != 5 || !strings.Contains(out.stderr, "atb linear release") {
		t.Fatalf("first: %+v", out)
	}
	if got := stepsOf(w); got != "comment" {
		t.Errorf("steps = %q", got)
	}
	// The retry skips the comment; the release now answers exit 4 (no
	// holder), which is still a failure.
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	out = w.asThreadAgent("TH-5", "thread", "end", "--summary-file", summary)
	if out.code != 5 {
		t.Fatalf("retry: %+v", out)
	}
	got := strings.TrimSpace(w.calls())
	if strings.Contains(got, "comment") || !strings.HasPrefix(got, "atb linear release TH-5") || strings.Contains(got, "herdr") {
		t.Errorf("retry calls = %q", got)
	}
	if got := threadAgentRow(w); got != "TH-5 active" {
		t.Errorf("row = %q", got)
	}
	// --force: the release is skipped and listed, the row ended, the tab
	// closed.
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	out = w.asThreadAgent("TH-5", "thread", "end", "--summary-file", summary, "--force")
	if out.code != 0 {
		t.Fatalf("force: %+v", out)
	}
	if !strings.Contains(out.stdout, "ended session 1 of thread "+threadKey+"\nLinear steps not done, finish them by hand:\n"+
		"  - atb linear release TH-5 --agent thread-c0123-1700000000-123 --reason done --done\n") ||
		strings.Contains(out.stdout, "comment") {
		t.Errorf("force stdout = %q", out.stdout)
	}
	if got := strings.TrimSpace(w.calls()); !strings.HasPrefix(got, "atb linear release TH-5") || !strings.HasSuffix(got, "herdr agent get\nherdr tab close") {
		t.Errorf("force calls = %q", got)
	}
	if got := threadAgentRow(w); got != "TH-5 ended" {
		t.Errorf("row = %q", got)
	}
	if got := strings.TrimSpace(w.file("closed-tab")); got != "t9" {
		t.Errorf("closed tab = %q", got)
	}
	if got := stepsOf(w); got != "comment end-row" {
		t.Errorf("steps = %q", got)
	}
}

func TestThreadEndRetriesAFailedTabCloseAfterTheRowEnded(t *testing.T) {
	w := threadWorld(t, "")
	if out := w.inbox(w.event("m1", "first", "")); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	for _, marker := range []string{"has-thread-c0123-1700000000-123", "tab-close-fails"} {
		if err := os.WriteFile(filepath.Join(w.dir, marker), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	summary := task(w, "summary.md", "bye\n")
	out := w.asThreadAgent("TH-5", "thread", "end", "--summary-file", summary)
	if out.code != 5 || !strings.Contains(out.stderr, "tab busy") {
		t.Fatalf("first: %+v", out)
	}
	if got := threadAgentRow(w); got != "TH-5 ended" {
		t.Errorf("row = %q", got)
	}
	if got := stepsOf(w); got != "comment release end-row" {
		t.Errorf("steps = %q", got)
	}
	_ = os.Remove(filepath.Join(w.dir, "calls"))
	out = w.asThreadAgent("TH-5", "thread", "end", "--summary-file", summary)
	if out.code != 0 {
		t.Fatalf("retry: %+v", out)
	}
	if got := strings.TrimSpace(w.calls()); got != "herdr agent get\nherdr tab close" {
		t.Errorf("retry calls = %q", got)
	}
	if got := strings.TrimSpace(w.file("closed-tab")); got != "t9" {
		t.Errorf("closed tab = %q", got)
	}
	// Once the tab is gone there is nothing left to do.
	_ = os.Remove(filepath.Join(w.dir, "has-thread-c0123-1700000000-123"))
	if out := w.asThreadAgent("TH-5", "thread", "end", "--summary-file", summary); out.code != 0 {
		t.Errorf("after the tab closed: %+v", out)
	}
}

// `fleet watch` against a fake herdr with a screen per agent, with ledger
// rows inserted directly. Hermetic, see main_test.go.
package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode"

	"github.com/Luolc/agent-fleet/internal/cmd"
	"github.com/Luolc/agent-fleet/internal/db"
)

const rule = "────────────────────────────────────────"

// watchHerdr is the stand-in for herdr in the watch tests, with the world's
// paths baked in. `agent list` prints <dir>/list.json; `agent read <name>`
// prints <dir>/screens/<name> and fails like herdr when there is none;
// `agent prompt` appends the target and the text to <dir>/prompts and
// answers with `reply`; `agent get` finds the agents of list.json; a pane
// has the tab `tab-<pane>`, and `tab close` writes the pane to
// <dir>/closed, after which neither is found.
func watchHerdr(dir, reply string) string {
	return `#!/bin/sh
if [ "$1" = --session ]; then shift 2; fi
dir='` + dir + `'
case "$1 $2" in
  "agent list")
    cat "$dir/list.json"
    ;;
  "agent read")
    if [ -f "$dir/screens/$3" ]; then
      cat "$dir/screens/$3"
    else
      echo '{"error":{"code":"agent_not_found","message":"no such agent"}}' >&2
      exit 1
    fi
    ;;
  "agent prompt")
    printf 'TO %s\n%s\n--END--\n' "$3" "$4" >> "$dir/prompts"
    cat <<'REPLY'
` + reply + `
REPLY
    ;;
  "status server")
    echo '{"running":true}'
    ;;
  "agent get")
    if grep -q "\"name\":\"$3\"" "$dir/list.json"; then
      echo '{"result":{"agent":{"name":"'"$3"'"}}}'
    else
      echo '{"error":{"code":"agent_not_found","message":"no such agent"}}'
    fi
    ;;
  "pane get")
    if grep -qx "$3" "$dir/closed" 2>/dev/null; then
      echo '{"error":{"code":"pane_not_found","message":"gone"}}'
    else
      echo '{"result":{"pane":{"tab_id":"tab-'"$3"'"}}}'
    fi
    ;;
  "tab close")
    echo "${3#tab-}" >> "$dir/closed"
    echo '{"result":{}}'
    ;;
  "tab get")
    if grep -qx "${3#tab-}" "$dir/closed" 2>/dev/null; then
      echo '{"error":{"code":"tab_not_found","message":"gone"}}'
    else
      echo '{"result":{}}'
    fi
    ;;
  "workspace list")
    echo '{"result":{"workspaces":[]}}'
    ;;
  *)
    echo "fake herdr: unexpected command: $*" >&2
    exit 2
    ;;
esac
`
}

// watchWorld is a world with the watch fake and an empty ledger.
type watchWorld struct {
	*world
	ledger *sql.DB
}

func newWatchWorld(t *testing.T) *watchWorld {
	t.Helper()
	w := &watchWorld{world: newWorld(t)}
	if err := os.MkdirAll(filepath.Join(w.dir, "screens"), 0o755); err != nil {
		t.Fatal(err)
	}
	w.reply(`{"result":{"type":"agent_prompted"}}`)
	w.ledger = w.world.ledger()
	t.Cleanup(func() { w.ledger.Close() })
	return w
}

// reply (re)writes the fake herdr so that `agent prompt` answers `reply`.
func (w *watchWorld) reply(reply string) {
	w.t.Helper()
	if err := os.WriteFile(filepath.Join(w.dir, "fake-herdr", "herdr"), []byte(watchHerdr(w.dir, reply)), 0o755); err != nil {
		w.t.Fatal(err)
	}
}

// watchRow is a ledger row; nil columns stay NULL.
type watchRow struct {
	name, role, job, parent, state string
	startedAt                      int64
	lastStatus, lastSeq            any
	lastScreenHash, lastChangeAt   any
}

func (w *watchWorld) insert(r watchRow) {
	w.t.Helper()
	if r.state == "" {
		r.state = "active"
	}
	if _, err := w.ledger.Exec(
		"INSERT INTO agents (name, role, job, parent, state, started_at, last_status, "+
			"last_seq, last_screen_hash, last_change_at) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10)",
		r.name, r.role, r.job, r.parent, r.state, r.startedAt,
		r.lastStatus, r.lastSeq, r.lastScreenHash, r.lastChangeAt); err != nil {
		w.t.Fatal(err)
	}
}

// herdrList sets what `herdr agent list` reports: name, agent_status and
// state_change_seq of each agent.
func (w *watchWorld) herdrList(agents ...string) {
	w.t.Helper()
	var entries []string
	for _, a := range agents {
		var name, status string
		var seq int
		if _, err := fmt.Sscanf(a, "%s %s %d", &name, &status, &seq); err != nil {
			w.t.Fatal(err)
		}
		entries = append(entries, fmt.Sprintf(
			`{"agent":"claude","name":"%s","agent_status":"%s","state_change_seq":%d}`, name, status, seq))
	}
	list := `{"id":"cli:agent:list","result":{"agents":[` + strings.Join(entries, ",") + `]}}`
	if err := os.WriteFile(filepath.Join(w.dir, "list.json"), []byte(list), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

func (w *watchWorld) screen(name, text string) {
	w.t.Helper()
	if err := os.WriteFile(filepath.Join(w.dir, "screens", name), []byte(text), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

// prompts is every `agent prompt` the fake saw, as [target, text].
func (w *watchWorld) prompts() [][2]string {
	log, _ := os.ReadFile(filepath.Join(w.dir, "prompts"))
	var prompts [][2]string
	for _, entry := range strings.Split(string(log), "--END--\n") {
		to, text, ok := strings.Cut(entry, "\n")
		target, isTo := strings.CutPrefix(to, "TO ")
		if ok && isTo {
			prompts = append(prompts, [2]string{target, strings.TrimRightFunc(text, unicode.IsSpace)})
		}
	}
	return prompts
}

func (w *watchWorld) suspects() string {
	w.t.Helper()
	var names sql.NullString
	if err := w.ledger.QueryRow(
		"SELECT group_concat(name, ' ') FROM (SELECT name FROM agents WHERE suspect = 1 ORDER BY name)",
	).Scan(&names); err != nil {
		w.t.Fatal(err)
	}
	return names.String
}

// claude is a Claude screen: a transcript, the spinner with its timer, the
// input box and a footer countdown.
func claude(transcript, timer, reset string) string {
	return transcript + "\n\n✻ Thinking… (" + timer + " · esc to interrupt)\n\n" + rule + "\n❯ \n" + rule +
		"\n  Session: 9% | Reset: " + reset + "\n"
}

func TestWatchTellsALeadOnlyWhenTheSuspectsAmongItsWorkersChange(t *testing.T) {
	w := newWatchWorld(t)
	anHourAgo := db.Now() - 3600
	stuck := claude("⏺ Running the migration", "12m 3s", "2hr 59m")
	// a-lead is stuck itself: a suspect that is only printed.
	w.insert(watchRow{name: "a-lead", role: "lead", job: "a", parent: "thread-1", startedAt: anHourAgo,
		lastStatus: "working", lastSeq: 7, lastScreenHash: cmd.Hash(cmd.FilterClaudeScreen(stuck)), lastChangeAt: anHourAgo})
	// a-w1 is moving: an hour since the last change, but its screen moved.
	w.insert(watchRow{name: "a-w1", role: "worker", job: "a", parent: "a-lead", startedAt: anHourAgo,
		lastStatus: "working", lastSeq: 9, lastScreenHash: cmd.Hash("an older screen"), lastChangeAt: anHourAgo})
	// a-w2 is stuck: the same status, seq and filtered screen as an hour ago.
	w.insert(watchRow{name: "a-w2", role: "worker", job: "a", parent: "a-lead", startedAt: anHourAgo,
		lastStatus: "working", lastSeq: 8, lastScreenHash: cmd.Hash(cmd.FilterClaudeScreen(stuck)), lastChangeAt: anHourAgo})
	// b-w1 is gone from herdr; its lead b-lead is fine.
	w.insert(watchRow{name: "b-lead", role: "lead", job: "b", parent: "thread-1", startedAt: anHourAgo})
	w.insert(watchRow{name: "b-w1", role: "worker", job: "b", parent: "b-lead", startedAt: anHourAgo})
	// Not watched: a thread agent idle for an hour, and an ended row.
	w.insert(watchRow{name: "thread-1", role: "thread", startedAt: anHourAgo,
		lastStatus: "idle", lastSeq: 1, lastScreenHash: cmd.Hash(""), lastChangeAt: anHourAgo})
	w.insert(watchRow{name: "c-lead", role: "lead", job: "c", state: "ended", startedAt: anHourAgo})
	w.herdrList("thread-1 idle 1", "a-lead working 7", "a-w1 working 9", "a-w2 working 8", "b-lead working 2")
	w.screen("thread-1", "")
	w.screen("a-lead", stuck)
	w.screen("a-w1", claude("⏺ Step 1\n⏺ Step 2", "3s", "2hr 59m"))
	w.screen("a-w2", stuck)
	w.screen("b-lead", claude("⏺ Planning", "1s", "2hr 59m"))

	out := w.run("", []string{"watch", "--scope", scope})
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if got := w.suspects(); got != "a-lead a-w2 b-w1" {
		t.Errorf("suspects = %q", got)
	}
	for _, want := range []string{
		"suspect: a-lead (lead, job a, parent thread-1): no change for 1h00m",
		"suspect: a-w2 (worker, job a, parent a-lead): no change for 1h00m",
		"suspect: b-w1 (worker, job b, parent b-lead): gone from herdr",
	} {
		if !strings.Contains(out.stdout, want) {
			t.Errorf("%q missing from stdout:\n%s", want, out.stdout)
		}
	}
	prompts := w.prompts()
	if len(prompts) != 2 {
		t.Fatalf("%q", prompts)
	}
	if prompts[0][0] != "a-lead" || prompts[1][0] != "b-lead" {
		t.Errorf("told %q and %q", prompts[0][0], prompts[1][0])
	}
	for i, want := range [][]string{
		{"[FROM: watch]\n", "a-w2 (worker, job a, parent a-lead): no change for 1h00m", "Running the migration"},
		{"[FROM: watch]\n", "b-w1 (worker, job b, parent b-lead): gone from herdr"},
	} {
		for _, needle := range want {
			if !strings.Contains(prompts[i][1], needle) {
				t.Errorf("%q missing from the notice to %s:\n%s", needle, prompts[i][0], prompts[i][1])
			}
		}
	}
	// The leads' own suspicion and the other lead's workers are never in a
	// lead's notice.
	for _, absent := range []string{"a-lead (lead", "a-w1", "b-w1"} {
		if strings.Contains(prompts[0][1], absent) {
			t.Errorf("%q in the notice to a-lead:\n%s", absent, prompts[0][1])
		}
	}

	// Only the spinner timer and the footer countdown move: still stuck,
	// the sets are unchanged, and no lead is told again.
	w.screen("a-lead", claude("⏺ Running the migration", "27m 40s", "2hr 44m"))
	w.screen("a-w2", claude("⏺ Running the migration", "27m 40s", "2hr 44m"))
	out = w.run("", []string{"watch", "--scope", scope})
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if !strings.Contains(out.stdout, "suspect set unchanged for every lead (3 suspect)") {
		t.Errorf("%s", out.stdout)
	}
	if got := len(w.prompts()); got != 2 {
		t.Errorf("%d prompts", got)
	}

	// b-w1 is back. b-lead's set changes, but herdr refuses the notice as
	// blocked: exit 3, and b-w1's flag is kept so the next run tells
	// b-lead. a-lead's set is unchanged, so it is not told (and not blocked
	// by the fake). The list does not show b-lead blocked, which would
	// start the unblocking (judge/unblock.sh).
	w.herdrList("thread-1 idle 1", "a-lead working 7", "a-w1 working 9", "a-w2 working 8", "b-lead working 3", "b-w1 working 4")
	w.screen("b-w1", claude("⏺ Back", "1s", "2hr 40m"))
	w.reply(`{"error":{"code":"agent_blocked","message":"b"}}`)
	out = w.run("", []string{"watch", "--scope", scope})
	if out.code != 3 {
		t.Fatalf("%+v", out)
	}
	if got := w.suspects(); got != "a-lead a-w2 b-w1" {
		t.Errorf("suspects = %q", got)
	}
	if prompts := w.prompts(); len(prompts) != 3 || prompts[2][0] != "b-lead" {
		t.Errorf("%q", prompts)
	}

	w.reply(`{"result":{"type":"agent_prompted"}}`)
	out = w.run("", []string{"watch", "--scope", scope})
	if out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if got := w.suspects(); got != "a-lead a-w2" {
		t.Errorf("suspects = %q", got)
	}
	prompts = w.prompts()
	if len(prompts) != 4 || prompts[3][0] != "b-lead" {
		t.Fatalf("%q", prompts)
	}
	if !strings.Contains(prompts[3][1], "No longer suspect: b-w1") || !strings.Contains(prompts[3][1], "No worker of yours is a suspect now") {
		t.Errorf("%s", prompts[3][1])
	}
}

func TestWatchGivesAStartingRowItsStartBeforeCallingItGone(t *testing.T) {
	w := newWatchWorld(t)
	now := db.Now()
	// a-w1's spawn reserved its row moments ago and herdr has no agent yet.
	w.insert(watchRow{name: "a-lead", role: "lead", job: "a", parent: "thread-1", startedAt: now - 3600})
	w.insert(watchRow{name: "a-w1", role: "worker", job: "a", parent: "a-lead", state: "starting", startedAt: now - 5})
	w.herdrList("a-lead working 2")
	w.screen("a-lead", claude("⏺ Spawning", "1s", "2hr 59m"))
	out := w.run("", []string{"watch", "--scope", scope})
	if out.code != 0 || strings.Contains(out.stdout, "suspect: a-w1") || len(w.prompts()) != 0 {
		t.Fatalf("%+v %q", out, w.prompts())
	}
	// Ten minutes on, still not in herdr: the start failed or its agent
	// went away, and the lead is told.
	w.exec(fmt.Sprintf("UPDATE agents SET started_at = %d WHERE name = 'a-w1'", now-600))
	out = w.run("", []string{"watch", "--scope", scope})
	if !strings.Contains(out.stdout, "suspect: a-w1 (worker, job a, parent a-lead): gone from herdr") {
		t.Errorf("%+v", out)
	}
	if prompts := w.prompts(); len(prompts) != 1 || prompts[0][0] != "a-lead" || !strings.Contains(prompts[0][1], "a-w1") {
		t.Errorf("%q", prompts)
	}
}

func TestWatchRefusesABadScope(t *testing.T) {
	w := newWatchWorld(t)
	if out := w.run("", []string{"watch", "--scope", "a/b"}); out.code != 1 {
		t.Errorf("%+v", out)
	}
}

// t0 is the clock the thread tests start from, through FLEET_WATCH_NOW.
const t0 int64 = 1_800_000_000

// watchThreadWorld is a watch world with a fednet socket configured, a fake
// fednet and a fake atb next to the fake herdr. The fake fednet logs its
// argv to <dir>/fednet.log; `read-thread <key>` answers one message whose
// ts is in <dir>/threads/<key with / as _> (exit 1 without one), and
// `post` prints a msg_id, but exits with the code in <dir>/mention-exit,
// when there is one, for a post with -mention; `delete` exits with the
// code in <dir>/delete-exit, 0 without one; while <dir>/fednet-down
// exists it exits 4. The
// fake atb logs its argv to <dir>/atb.log, and fails while <dir>/atb-down
// exists.
func watchThreadWorld(t *testing.T) *watchWorld {
	t.Helper()
	w := newWatchWorld(t)
	fake := filepath.Join(w.dir, "fake-herdr")
	fednet := `#!/bin/sh
dir='` + w.dir + `'
printf '%s\n' "$*" >> "$dir/fednet.log"
[ -e "$dir/fednet-down" ] && { echo "hub unreachable" >&2; exit 4; }
case "$2" in
  read-thread)
    for last; do :; done
    f="$dir/threads/$(printf %s "$last" | tr / _)"
    [ -f "$f" ] || { echo "no thread $last" >&2; exit 1; }
    printf '{"messages":[{"ts":"1.000","user":"U1","text":"a"},{"ts":"%s","user":"U1","text":"b"}]}\n' "$(cat "$f")"
    ;;
  post)
    case " $* " in *" -mention "*) if [ -f "$dir/mention-exit" ]; then
      echo 'post: unknown command "check-mentions" (the hub runs an older fednet than this client; upgrade the hub)' >&2
      exit "$(cat "$dir/mention-exit")"
    fi ;; esac
    echo "msg-$(wc -l < "$dir/fednet.log" | tr -d ' ')" ;;
  delete)
    if [ -f "$dir/delete-exit" ]; then echo "delete: refused" >&2; exit "$(cat "$dir/delete-exit")"; fi ;;
esac
`
	// `linear query` prints <dir>/issues.json, no started issue without it.
	atb := `#!/bin/sh
dir='` + w.dir + `'
printf '%s\n' "$*" >> "$dir/atb.log"
[ -e "$dir/atb-down" ] && exit 1
if [ "$2" = query ]; then
  cat "$dir/issues.json" 2>/dev/null || echo '{"issues":{"nodes":[]}}'
fi
exit 0
`
	config := filepath.Join(w.dir, "home", ".config", "fleet")
	for path, body := range map[string]string{filepath.Join(fake, "fednet"): fednet, filepath.Join(fake, "atb"): atb,
		filepath.Join(config, scope+".json"): `{"fednet": {"socket": "/run/example/fednet.sock"}}`} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(w.dir, "threads"), 0o755); err != nil {
		t.Fatal(err)
	}
	w.herdrList()
	return w
}

// lastMessage sets the time of the newest message of thread `key`.
func (w *watchWorld) lastMessage(key string, at int64) {
	w.t.Helper()
	path := filepath.Join(w.dir, "threads", strings.ReplaceAll(key, "/", "_"))
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d.000100", at)), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

func (w *watchWorld) exec(stmts ...string) {
	w.t.Helper()
	for _, stmt := range stmts {
		if _, err := w.ledger.Exec(stmt); err != nil {
			w.t.Fatalf("%s: %v", stmt, err)
		}
	}
}

func (w *watchWorld) query(q string) string {
	w.t.Helper()
	var got sql.NullString
	if err := w.ledger.QueryRow(q).Scan(&got); err != nil {
		w.t.Fatalf("%s: %v", q, err)
	}
	return got.String
}

// log is a fake's log, with `--body-file <path>` cut to `--body-file F`.
func (w *watchWorld) log(name string) string {
	data, _ := os.ReadFile(filepath.Join(w.dir, name))
	fields := strings.Fields(strings.ReplaceAll(string(data), "\n", " | "))
	for i := range fields {
		if i > 0 && fields[i-1] == "--body-file" {
			fields[i] = "F"
		}
	}
	return strings.Join(fields, " ")
}

// watchAt runs watch with the clock at t0 + offset, expecting `code`.
func (w *watchWorld) watchAt(offset time.Duration, code int) result {
	w.t.Helper()
	out := w.run("", []string{"watch", "--scope", scope}, fmt.Sprintf("FLEET_WATCH_NOW=%d", t0+int64(offset/time.Second)))
	if out.code != code {
		w.t.Fatalf("watch at t0+%v: %+v", offset, out)
	}
	return out
}

// file writes `body` to <dir>/<name>, or removes it when body is empty.
func (w *watchWorld) file(name, body string) {
	w.t.Helper()
	path := filepath.Join(w.dir, name)
	if body == "" {
		if err := os.Remove(path); err != nil {
			w.t.Fatal(err)
		}
		return
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

// remindersOf is the thread's reminders as `<msg_id> <state>, ...`, oldest
// first.
func (w *watchWorld) remindersOf(thread string) string {
	return w.query("SELECT group_concat(msg_id || ' ' || state, ', ') FROM (SELECT * FROM reminders WHERE thread = '" +
		thread + "' ORDER BY id)")
}

func TestWatchRemindersMentionTheLastWriterAndReplaceTheOneBefore(t *testing.T) {
	w := watchThreadWorld(t)
	w.file(filepath.Join("home", ".config", "fleet", scope+".json"),
		`{"fednet": {"socket": "/run/example/fednet.sock"}, "watch": {"reminders": ["30m", "1h", "2h", "3h", "4h"]}}`)
	w.exec("INSERT INTO threads (thread, slug, created_at, last_user) VALUES ('C1/1.0', 'c1-1-0', 0, 'U0EXAMPLE1')",
		"INSERT INTO agents (name, role, thread, pane_id, state, started_at) VALUES ('thread-c1', 'thread', 'C1/1.0', 'p1', 'active', 0)",
		fmt.Sprintf("INSERT INTO questions (job, thread, asked_by, text, state, asked_at) VALUES ('', 'C1/1.0', 'thread-c1', 'Which month?', 'pending', %d)", t0))
	w.herdrList("thread-c1 idle 1")
	w.lastMessage("C1/1.0", t0)
	latest := func() string { return w.query("SELECT reminder_msg FROM questions") }
	deleted := func(msg string) bool {
		return strings.Contains(w.log("fednet.log"), "client delete -socket /run/example/fednet.sock -- "+msg+" |")
	}

	w.watchAt(30*time.Minute, 0)
	if got := w.log("fednet.log"); !strings.Contains(got, "client post -socket /run/example/fednet.sock -thread C1/1.0 "+
		"-mention U0EXAMPLE1 -- Reminder 1 of 5: ") || strings.Contains(got, "client delete") {
		t.Errorf("%s", got)
	}
	first := latest()
	if got := w.remindersOf("C1/1.0"); got != first+" posted" {
		t.Errorf("reminders %q", got)
	}

	// The next reminder replaces the first, which is deleted.
	out := w.watchAt(time.Hour, 0)
	second := latest()
	if !deleted(first) || !strings.Contains(out.stdout, "deleted the reminder "+first+" in thread C1/1.0") {
		t.Errorf("%+v\n%s", out, w.log("fednet.log"))
	}
	if got := w.remindersOf("C1/1.0"); got != first+" deleted, "+second+" posted" {
		t.Errorf("reminders %q", got)
	}

	// Not in Slack yet: tried again by the next run, with no reminder due.
	w.file("delete-exit", "5")
	out = w.watchAt(2*time.Hour, 0)
	third := latest()
	if got := w.remindersOf("C1/1.0"); got != first+" deleted, "+second+" stale, "+third+" posted" {
		t.Errorf("reminders %q", got)
	}
	if !strings.Contains(out.stderr, "the reminder "+second+" in thread C1/1.0 is not deleted, the next run tries again: "+
		"fednet client delete exit status 5: delete: refused") {
		t.Errorf("%+v", out)
	}
	w.file("delete-exit", "")
	w.watchAt(2*time.Hour+10*time.Minute, 0)
	if got := w.remindersOf("C1/1.0"); got != first+" deleted, "+second+" deleted, "+third+" posted" {
		t.Errorf("reminders %q", got)
	}

	// Gone already: counted as deleted.
	w.file("delete-exit", "1")
	w.watchAt(3*time.Hour, 0)
	fourth := latest()
	if got := w.remindersOf("C1/1.0"); !deleted(third) || !strings.HasSuffix(got, third+" deleted, "+fourth+" posted") {
		t.Errorf("reminders %q", got)
	}

	// A hub older than its client cannot delete: the reminder stays, and
	// no run asks again.
	w.file("delete-exit", "2")
	out = w.watchAt(4*time.Hour, 0)
	fifth := latest()
	if got := w.remindersOf("C1/1.0"); !strings.HasSuffix(got, fourth+" kept, "+fifth+" posted") {
		t.Errorf("reminders %q", got)
	}
	if !strings.Contains(out.stderr, "the reminder "+fourth+" in thread C1/1.0 stays in the thread: fednet client delete exit status 2") {
		t.Errorf("%+v", out)
	}
	deletes := strings.Count(w.log("fednet.log"), "client delete")
	w.watchAt(5*time.Hour, 0)
	if got := strings.Count(w.log("fednet.log"), "client delete"); got != deletes {
		t.Errorf("%d deletes, then %d: %s", deletes, got, w.log("fednet.log"))
	}
}

func TestWatchPostsTheReminderWithoutTheMentionFednetRefuses(t *testing.T) {
	w := watchThreadWorld(t)
	w.exec("INSERT INTO threads (thread, slug, created_at, last_user) VALUES ('C1/1.0', 'c1-1-0', 0, 'U0EXAMPLE1'), "+
		"('C2/2.0', 'c2-2-0', 0, '')",
		"INSERT INTO agents (name, role, thread, pane_id, state, started_at) VALUES ('thread-c1', 'thread', 'C1/1.0', 'p1', 'active', 0), "+
			"('thread-c2', 'thread', 'C2/2.0', 'p2', 'active', 0)",
		fmt.Sprintf("INSERT INTO questions (job, thread, asked_by, text, state, asked_at) VALUES "+
			"('', 'C1/1.0', 'thread-c1', 'Which month?', 'pending', %d), ('', 'C2/2.0', 'thread-c2', 'Which year?', 'pending', %d)", t0, t0))
	w.herdrList("thread-c1 idle 1", "thread-c2 idle 1")
	w.lastMessage("C1/1.0", t0)
	w.lastMessage("C2/2.0", t0)

	// A fednet too old for -mention (exit 2), then a user not on the hub's
	// list (exit 3): each reminder is posted again without the mention.
	for i, code := range []string{"2", "3"} {
		w.file("mention-exit", code)
		out := w.watchAt([]time.Duration{30 * time.Minute, 3 * time.Hour}[i], 0)
		n := fmt.Sprint(i + 1)
		if got := w.log("fednet.log"); !strings.Contains(got, "-thread C1/1.0 -mention U0EXAMPLE1 -- Reminder "+n+" of 3") ||
			!strings.Contains(got, "-thread C1/1.0 -- Reminder "+n+" of 3") {
			t.Errorf("%s", got)
		}
		if !strings.Contains(out.stderr, "the reminder in thread C1/1.0 is posted without mentioning anyone: fednet refused "+
			"the mention (exit status: "+code+"): post: unknown command") {
			t.Errorf("%+v", out)
		}
		if got := w.query("SELECT reminders FROM questions WHERE thread = 'C1/1.0'"); got != n {
			t.Errorf("reminders %s", got)
		}
	}
	// Nobody has written in C2 yet: its reminders mention no one.
	if got := w.log("fednet.log"); strings.Count(got, "-thread C2/2.0 -- Reminder") != 2 || strings.Contains(got, "-thread C2/2.0 -mention") {
		t.Errorf("%s", got)
	}
}

func TestWatchRemindsOfAThreadAgentsQuestionThenReclaimsTheSession(t *testing.T) {
	w := watchThreadWorld(t)
	w.exec("INSERT INTO threads (thread, slug, ticket, ticket_url, created_at) VALUES ('C1/1.0', 'c1-1-0', 'EX-1', 'https://linear.example.test/EX-1', 0)",
		"INSERT INTO agents (name, role, thread, pane_id, state, started_at) VALUES ('thread-c1', 'thread', 'C1/1.0', 'p1', 'active', 0)",
		fmt.Sprintf("INSERT INTO questions (job, thread, asked_by, text, state, asked_at) VALUES ('', 'C1/1.0', 'thread-c1', 'Which month?\nDetails.', 'pending', %d)", t0))
	w.herdrList("thread-c1 idle 1")
	w.lastMessage("C1/1.0", t0)
	reminders := func() string { return w.query("SELECT reminders || ' ' || reminder_msg FROM questions") }

	w.watchAt(29*time.Minute, 0)
	if got := w.log("fednet.log"); strings.Contains(got, "post") {
		t.Errorf("posted before 30m: %s", got)
	}
	w.watchAt(30*time.Minute, 0)
	if got := w.log("fednet.log"); !strings.Contains(got, "client post -socket /run/example/fednet.sock -thread C1/1.0 -- "+
		"Reminder 1 of 3: 1 question(s) here still wait for an answer, the first asked 30m ago: | - from thread-c1: Which month? |") {
		t.Errorf("%s", got)
	}
	if got := reminders(); got != "1 msg-4" {
		t.Errorf("reminders %q", got)
	}
	// Quiet for an hour, but a question waits: no notice to the agent.
	w.watchAt(time.Hour, 0)
	w.watchAt(3*time.Hour, 0)
	w.watchAt(25*time.Hour, 0)
	w.watchAt(48*time.Hour, 0)
	if got := strings.Count(w.log("fednet.log"), "-- Reminder"); got != 3 {
		t.Errorf("%d reminders: %s", got, w.log("fednet.log"))
	}
	if got := w.log("fednet.log"); !strings.Contains(got, "Reminder 2 of 3") || !strings.Contains(got, "Reminder 3 of 3") {
		t.Errorf("%s", got)
	}
	if got := reminders(); !strings.HasPrefix(got, "3 msg-") {
		t.Errorf("reminders %q", got)
	}
	if got := len(w.prompts()); got != 0 {
		t.Errorf("%q", w.prompts())
	}

	// 72 hours: the question is closed and the agent asked to end.
	w.watchAt(72*time.Hour, 0)
	if got := w.query("SELECT state FROM questions"); got != "closed" {
		t.Errorf("question %s", got)
	}
	prompts := w.prompts()
	if len(prompts) != 1 || prompts[0][0] != "thread-c1" {
		t.Fatalf("%q", prompts)
	}
	for _, want := range []string{"[FROM: watch]\n", "rule `thread question timeout`", "no answer for 3d00h",
		"Which month?", "--asked-to-end", "within 10m"} {
		if !strings.Contains(prompts[0][1], want) {
			t.Errorf("%q missing:\n%s", want, prompts[0][1])
		}
	}
	if got := w.query("SELECT reclaim_at FROM agents WHERE name = 'thread-c1'"); got != fmt.Sprint(t0+72*3600+600) {
		t.Errorf("reclaim_at %s", got)
	}
	w.watchAt(72*time.Hour+9*time.Minute, 0)
	if got := w.log("atb.log"); got != "" {
		t.Errorf("atb before the grace ran out: %s", got)
	}
	// Ten minutes later it is still live: fleet ends the session itself.
	out := w.watchAt(72*time.Hour+10*time.Minute, 0)
	if got := w.log("atb.log"); got != "linear comment EX-1 --body-file F | linear release EX-1 --agent thread-c1 --reason done --done |" {
		t.Errorf("atb: %s", got)
	}
	if got := w.log("fednet.log"); !strings.Contains(got, "-footer -- 会话长时间没有动静，已被回收，再说话会重新开始 · [EX-1](https://linear.example.test/EX-1)") {
		t.Errorf("fednet: %s", got)
	}
	if got := w.query("SELECT state FROM agents WHERE name = 'thread-c1'"); got != "ended" {
		t.Errorf("row %s", got)
	}
	if data, _ := os.ReadFile(filepath.Join(w.dir, "closed")); string(data) != "p1\n" {
		t.Errorf("closed %q", data)
	}
	if !strings.Contains(out.stdout, "reclaimed the session of thread-c1 on thread C1/1.0") {
		t.Errorf("%s", out.stdout)
	}
	if got := len(w.prompts()); got != 1 {
		t.Errorf("%q", w.prompts())
	}
}

func TestWatchAsksAboutAQuietThreadOncePerSpell(t *testing.T) {
	w := watchThreadWorld(t)
	w.exec("INSERT INTO threads (thread, slug, created_at) VALUES ('C2/2.0', 'c2-2-0', 0)",
		"INSERT INTO agents (name, role, thread, pane_id, state, started_at) VALUES ('thread-c2', 'thread', 'C2/2.0', 'p2', 'active', 0)",
		"INSERT INTO jobs (job, parent_issue, lead_cwd, home_thread, state, started_at) VALUES ('j', 'EX-9', '/c', 'C2/2.0', 'open', 0)")
	w.insert(watchRow{name: "j-lead", role: "lead", job: "j", parent: "thread-c2"})
	w.insert(watchRow{name: "j-w", role: "worker", job: "j", parent: "j-lead"})
	w.herdrList("thread-c2 idle 1", "j-lead working 4")
	w.screen("j-lead", claude("⏺ Planning", "1s", "2hr 59m"))
	w.lastMessage("C2/2.0", t0)

	w.watchAt(29*time.Minute, 0)
	// The worker gone from herdr goes to its lead; the thread agent is not asked yet.
	if prompts := w.prompts(); len(prompts) != 1 || prompts[0][0] != "j-lead" {
		t.Fatalf("%q", prompts)
	}
	w.watchAt(30*time.Minute, 0)
	prompts := w.prompts()
	if len(prompts) != 2 || prompts[1][0] != "thread-c2" {
		t.Fatalf("%q", prompts)
	}
	for _, want := range []string{"[FROM: watch]\n", "rule `thread quiet, jobs open`", "no new message for 30m",
		"- job j (parent EX-9): lead j-lead working, unchanged for 1m; suspect workers: j-w"} {
		if !strings.Contains(prompts[1][1], want) {
			t.Errorf("%q missing:\n%s", want, prompts[1][1])
		}
	}
	// The same quiet spell: not asked again.
	w.watchAt(2*time.Hour, 0)
	if got := len(w.prompts()); got != 2 {
		t.Fatalf("%q", w.prompts())
	}
	// A new message starts the count again.
	w.lastMessage("C2/2.0", t0+3*3600)
	w.watchAt(3*time.Hour+29*time.Minute, 0)
	w.watchAt(3*time.Hour+30*time.Minute, 0)
	if prompts := w.prompts(); len(prompts) != 3 || !strings.Contains(prompts[2][1], "rule `thread quiet, jobs open`") {
		t.Fatalf("%q", prompts)
	}
	// The job ended: the live agent is asked once why it has not ended.
	w.exec("UPDATE jobs SET state = 'ended', outcome = 'done'", "UPDATE agents SET state = 'ended' WHERE job = 'j'")
	w.lastMessage("C2/2.0", t0+4*3600)
	w.watchAt(4*time.Hour+30*time.Minute, 0)
	w.watchAt(5*time.Hour, 0)
	prompts = w.prompts()
	if len(prompts) != 4 || prompts[3][0] != "thread-c2" {
		t.Fatalf("%q", prompts)
	}
	for _, want := range []string{"rule `thread quiet, nothing open`", "Why has it not ended", "no message for 3d00h"} {
		if !strings.Contains(prompts[3][1], want) {
			t.Errorf("%q missing:\n%s", want, prompts[3][1])
		}
	}
	// Three days without a message: the session is reclaimed.
	w.watchAt(76*time.Hour, 0)
	prompts = w.prompts()
	if len(prompts) != 5 || !strings.Contains(prompts[4][1], "rule `thread idle`: thread C2/2.0 has had no message for 3d00h") {
		t.Fatalf("%q", prompts)
	}
	if got := w.query("SELECT reclaim_at FROM agents WHERE name = 'thread-c2'"); got != fmt.Sprint(t0+76*3600+600) {
		t.Errorf("reclaim_at %s", got)
	}
}

func TestWatchEndsAJobWhoseLeadsQuestionGoesUnanswered(t *testing.T) {
	w := watchThreadWorld(t)
	w.exec("INSERT INTO threads (thread, slug, created_at) VALUES ('C3/3.0', 'c3-3-0', 0)",
		"INSERT INTO jobs (job, lead_cwd, home_thread, state, started_at) VALUES ('k', '/c', 'C3/3.0', 'open', 0)",
		"INSERT INTO jobs (job, lead_cwd, home_thread, state, started_at) VALUES ('m', '/c', 'C3/3.0', 'open', 0)",
		fmt.Sprintf("INSERT INTO questions (job, thread, asked_by, text, state, asked_at) VALUES "+
			"('k', 'C3/3.0', 'k-lead', 'Merge it?', 'pending', %d), ('m', 'C3/3.0', 'm-lead', 'Which host?', 'pending', %d)", t0, t0+3600))
	w.insert(watchRow{name: "k-lead", role: "lead", job: "k", parent: "thread-c3"})
	w.exec("UPDATE agents SET issue = 'EX-20' WHERE name = 'k-lead'")
	w.herdrList("k-lead idle 2")
	w.screen("k-lead", claude("⏺ Waiting", "1s", "2hr 59m"))
	w.lastMessage("C3/3.0", t0+3600)

	// Watch was down for three days: one reminder, the last one, listing both.
	w.watchAt(72*time.Hour, 0)
	if got := w.log("fednet.log"); !strings.Contains(got, "Reminder 3 of 3: 2 question(s) here still wait for an answer, the first asked 3d00h ago: | - from k-lead: Merge it? | - from m-lead: Which host? |") {
		t.Errorf("%s", got)
	}
	prompts := w.prompts()
	if len(prompts) != 1 || prompts[0][0] != "k-lead" {
		t.Fatalf("%q", prompts)
	}
	for _, want := range []string{"[FROM: watch]\n", "rule `lead question timeout`", "no answer for 3d00h", "Merge it?",
		"Job k is ended by force at", "30m from now", "fleet job end --report-file <file> --abandon"} {
		if !strings.Contains(prompts[0][1], want) {
			t.Errorf("%q missing:\n%s", want, prompts[0][1])
		}
	}
	w.watchAt(72*time.Hour+29*time.Minute, 0)
	if got := w.query("SELECT state FROM jobs WHERE job = 'k'"); got != "open" {
		t.Errorf("job k %s", got)
	}
	out := w.watchAt(72*time.Hour+30*time.Minute, 0)
	if got := w.query("SELECT state || ' ' || outcome FROM jobs WHERE job = 'k'"); got != "ended abandoned" {
		t.Errorf("job k %s", got)
	}
	if got := w.query("SELECT group_concat(asked_by || ' ' || state, ', ') FROM questions"); got != "k-lead closed, m-lead pending" {
		t.Errorf("questions: %s", got)
	}
	if got := w.log("fednet.log"); !strings.Contains(got, "-- Job k was ended by force: its lead's question had no answer for 3d00h, "+
		"and the job was not ended within 30m of the lead being told. Linear steps not done: | - comment the report on work order EX-20 "+
		"| - release work order EX-20 (agent k-lead) |") {
		t.Errorf("%s", got)
	}
	if !strings.Contains(out.stdout, "reclaimed job k") {
		t.Errorf("%s", out.stdout)
	}
	// m's question, an hour younger, is next; nobody else is told.
	if got := len(w.prompts()); got != 1 {
		t.Errorf("%q", w.prompts())
	}
	// An answer after the lead was told: the deadline is dropped.
	w.watchAt(73*time.Hour, 0)
	if got := w.query("SELECT reclaim_at FROM jobs WHERE job = 'm'"); got == "" {
		t.Fatal("m's lead was not given a deadline")
	}
	// A person answered, so the thread is not quiet either.
	w.exec("UPDATE questions SET state = 'answered' WHERE job = 'm'")
	w.lastMessage("C3/3.0", t0+74*3600-60)
	w.watchAt(74*time.Hour, 0)
	if got := w.query("SELECT coalesce(reclaim_at, 'none') || ' ' || state FROM jobs WHERE job = 'm'"); got != "none open" {
		t.Errorf("job m %s", got)
	}
}

func TestWatchMovesTheQuestionsOfAJobThatEndedToItsThread(t *testing.T) {
	w := watchThreadWorld(t)
	// d ended with its lead's question pending, after one reminder; e's
	// thread has no live agent; f ended after its lead was told its
	// question expired.
	w.exec("INSERT INTO threads (thread, slug, created_at) VALUES ('C4/4.0', 'c4-4-0', 0), ('C5/5.0', 'c5-5-0', 0)",
		"INSERT INTO agents (name, role, thread, pane_id, state, started_at) VALUES ('thread-c4', 'thread', 'C4/4.0', 'p4', 'active', 0)",
		"INSERT INTO jobs (job, lead_cwd, home_thread, state, outcome, started_at, ended_at) VALUES "+
			"('d', '/c', 'C4/4.0', 'ended', 'done', 0, 1), ('e', '/c', 'C5/5.0', 'ended', 'abandoned', 0, 1)",
		fmt.Sprintf("INSERT INTO jobs (job, lead_cwd, home_thread, state, outcome, started_at, ended_at, reclaim_at) VALUES "+
			"('f', '/c', 'C4/4.0', 'ended', 'abandoned', 0, %d, %d)", t0+600, t0+1800),
		fmt.Sprintf("INSERT INTO questions (job, thread, asked_by, text, state, asked_at, reminders) VALUES "+
			"('d', 'C4/4.0', 'd-lead', 'Which schema?\nTwo options.\n\nThird line.\nFourth line.', 'pending', %d, 1), "+
			"('e', 'C5/5.0', 'e-lead', 'Which host?', 'pending', %d, 0), "+
			"('f', 'C4/4.0', 'f-lead', 'Which region?', 'pending', %d, 0)", t0, t0, t0))
	w.herdrList("thread-c4 idle 1")
	w.lastMessage("C4/4.0", t0+3600)
	w.lastMessage("C5/5.0", t0)

	out := w.watchAt(time.Hour, 0)
	if got := w.query("SELECT group_concat(job || ' ' || asked_by || ' ' || state, ', ') FROM questions"); got != "d d-lead pending, e e-lead pending, f f-lead closed" {
		t.Errorf("questions: %s", got)
	}
	prompts := w.prompts()
	if len(prompts) != 1 || prompts[0][0] != "thread-c4" {
		t.Fatalf("%q", prompts)
	}
	for _, want := range []string{"[FROM: watch]\n", "rule `questions of an ended job`: 1 question(s) a lead asked in thread C4/4.0",
		"your session is not reclaimed for it", "- job d, from d-lead, asked at ", "    Which schema?\n    Two options.\n    Third line."} {
		if !strings.Contains(prompts[0][1], want) {
			t.Errorf("%q missing:\n%s", want, prompts[0][1])
		}
	}
	if strings.Contains(prompts[0][1], "Fourth line.") {
		t.Errorf("more than three lines:\n%s", prompts[0][1])
	}
	if strings.Contains(prompts[0][1], "Which region?") {
		t.Errorf("told of f's question:\n%s", prompts[0][1])
	}
	if !strings.Contains(out.stdout, "told thread-c4 of 1 question(s) of ended jobs in thread C4/4.0") ||
		!strings.Contains(out.stdout, "closed 1 pending question(s) of ended jobs: a screen's, or the job ended after its lead was told") {
		t.Errorf("%s", out.stdout)
	}
	// e's thread gets its reminder, and no agent is started for it.
	if got := w.log("fednet.log"); !strings.Contains(got, "-thread C5/5.0 -- Reminder 1 of 3: 1 question(s) here still wait for an answer, "+
		"the first asked 1h00m ago: | - from e-lead: Which host? |") || strings.Contains(got, "-thread C4/4.0 -- Reminder") {
		t.Errorf("%s", got)
	}
	if got := w.query("SELECT count(*) FROM agents WHERE thread = 'C5/5.0'"); got != "0" {
		t.Errorf("%s agents on C5", got)
	}

	// Told once; a new job of the same name does not take the question,
	// and the reminders go on from when it was asked.
	w.exec("INSERT INTO jobs (job, lead_cwd, home_thread, state, started_at) VALUES ('d', '/c', 'C4/4.0', 'open', " +
		fmt.Sprint(t0+2*3600) + ")")
	w.watchAt(3*time.Hour, 0)
	if got := len(w.prompts()); got != 1 {
		t.Errorf("%q", w.prompts())
	}
	if got := w.log("fednet.log"); !strings.Contains(got, "-thread C4/4.0 -- Reminder 2 of 3: 1 question(s) here still wait for an answer, "+
		"the first asked 3h00m ago: | - from d-lead: Which schema? |") {
		t.Errorf("%s", got)
	}

	// After thread_question it is closed, the session kept and d left alone.
	out = w.watchAt(72*time.Hour, 0)
	if got := w.query("SELECT group_concat(state, ', ') FROM questions"); got != "closed, closed, closed" {
		t.Errorf("questions: %s", got)
	}
	if !strings.Contains(out.stdout, "closed 1 question(s) of ended jobs in thread C4/4.0: no answer for 3d00h") {
		t.Errorf("%s", out.stdout)
	}
	if got := w.query("SELECT coalesce(reclaim_at, 'none') FROM agents WHERE name = 'thread-c4'"); got != "none" {
		t.Errorf("thread-c4 reclaim_at %s", got)
	}
	if got := w.query("SELECT coalesce(reclaim_at, 'none') FROM jobs WHERE job = 'd' AND state = 'open'"); got != "none" {
		t.Errorf("job d reclaim_at %s", got)
	}
	for _, p := range w.prompts() {
		if strings.Contains(p[1], "--asked-to-end") {
			t.Errorf("asked to end: %q", p)
		}
	}
}

func TestWatchRemindsOfScreenQuestionsButNeverTimesThemOut(t *testing.T) {
	w := watchThreadWorld(t)
	w.exec("INSERT INTO threads (thread, slug, created_at) VALUES ('C5/5.0', 'c5-5-0', 0)",
		"INSERT INTO agents (name, role, thread, pane_id, state, started_at) VALUES ('thread-c5', 'thread', 'C5/5.0', 'p5', 'active', 0)",
		"INSERT INTO jobs (job, lead_cwd, home_thread, state, started_at) VALUES ('q', '/c', 'C5/5.0', 'open', 0)",
		fmt.Sprintf("INSERT INTO questions (job, thread, asked_by, text, state, asked_at) VALUES "+
			"('q', 'C5/5.0', 'unblock-41', 'Press 1 to run it?', 'pending', %d), ('', 'C5/5.0', 'unblock-42', 'Trust it?', 'pending', %d)", t0, t0))
	w.insert(watchRow{name: "q-lead", role: "lead", job: "q", parent: "thread-c5"})
	w.herdrList("thread-c5 idle 1", "q-lead idle 2")
	w.screen("q-lead", claude("⏺ Waiting", "1s", "2hr 59m"))
	w.lastMessage("C5/5.0", t0)
	w.watchAt(30*time.Minute, 0)
	if got := w.log("fednet.log"); !strings.Contains(got, "Reminder 1 of 3: 2 question(s) here still wait for an answer") {
		t.Errorf("%s", got)
	}
	// Past both limits (the thread itself is not idle): the screen
	// questions still wait, the job stays open, the session is not ended.
	w.lastMessage("C5/5.0", t0+72*3600)
	w.watchAt(72*time.Hour+31*time.Minute, 0)
	if got := w.query("SELECT group_concat(asked_by || ' ' || state, ', ') FROM questions"); got != "unblock-41 pending, unblock-42 pending" {
		t.Errorf("questions: %s", got)
	}
	if got := w.query("SELECT state || ' ' || coalesce(reclaim_at, 'none') FROM jobs WHERE job = 'q'"); got != "open none" {
		t.Errorf("job q %s", got)
	}
	if got := w.query("SELECT coalesce(reclaim_at, 'none') FROM agents WHERE name = 'thread-c5'"); got != "none" {
		t.Errorf("thread-c5 reclaim_at %s", got)
	}
	if got := w.prompts(); len(got) != 0 {
		t.Errorf("%q", got)
	}
	if got := strings.Count(w.log("fednet.log"), "-- Reminder"); got != 2 {
		t.Errorf("%d reminders: %s", got, w.log("fednet.log"))
	}
	// The job ended: its screen question is closed, not moved to the
	// thread; the thread agent's own still waits, and nobody is told.
	w.exec("UPDATE jobs SET state = 'ended', outcome = 'done', ended_at = 1", "UPDATE agents SET state = 'ended' WHERE job = 'q'")
	out := w.watchAt(73*time.Hour, 0)
	if got := w.query("SELECT group_concat(asked_by || ' ' || state, ', ') FROM questions"); got != "unblock-41 closed, unblock-42 pending" {
		t.Errorf("questions: %s", got)
	}
	if !strings.Contains(out.stdout, "closed 1 pending question(s) of ended jobs: a screen's") {
		t.Errorf("%s", out.stdout)
	}
	if got := w.prompts(); len(got) != 0 {
		t.Errorf("%q", got)
	}
}

func TestWatchEndsTheSessionOfAThreadAgentGoneFromHerdr(t *testing.T) {
	w := watchThreadWorld(t)
	w.exec("INSERT INTO threads (thread, slug, ticket, ticket_url, created_at) VALUES ('C4/4.0', 'c4-4-0', 'EX-4', 'https://linear.example.test/EX-4', 0)",
		"INSERT INTO agents (name, role, thread, pane_id, state, started_at) VALUES ('thread-c4', 'thread', 'C4/4.0', 'p4', 'active', 0)")
	w.lastMessage("C4/4.0", t0)
	out := w.watchAt(time.Minute, 0)
	if got := w.log("atb.log"); got != "linear release EX-4 --agent thread-c4 --reason the session ended abnormally: its agent is gone from herdr |" {
		t.Errorf("atb: %s", got)
	}
	if got := w.log("fednet.log"); !strings.Contains(got, "-footer -- 会话意外中断了，再说话会重新开始 · [EX-4](https://linear.example.test/EX-4)") {
		t.Errorf("fednet: %s", got)
	}
	if got := w.query("SELECT state FROM agents WHERE name = 'thread-c4'"); got != "ended" {
		t.Errorf("row %s", got)
	}
	if !strings.Contains(out.stdout, "ended the session of thread-c4 on thread C4/4.0: its agent is gone from herdr") {
		t.Errorf("%s", out.stdout)
	}
}

func TestWatchSkipsAThreadFednetCannotGiveAndStopsWhenFednetIsDown(t *testing.T) {
	w := watchThreadWorld(t)
	// C5's session ended with its question pending, and fednet has no C5.
	w.exec("INSERT INTO agents (name, role, thread, pane_id, state, started_at) VALUES ('thread-c5', 'thread', 'C5/5.0', 'p5', 'ended', 0)",
		"INSERT INTO questions (job, thread, asked_by, text, state, asked_at) VALUES ('', 'C5/5.0', 'thread-c5', 'Q?', 'pending', 0)")
	w.insert(watchRow{name: "n-lead", role: "lead", job: "n", parent: "thread-1"})
	w.insert(watchRow{name: "n-w", role: "worker", job: "n", parent: "n-lead"})
	w.herdrList("n-lead idle 1")
	w.screen("n-lead", claude("⏺ Waiting", "1s", "2hr 59m"))

	// fednet unreachable: nothing done.
	if err := os.WriteFile(filepath.Join(w.dir, "fednet-down"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out := w.watchAt(time.Minute, 5)
	if !strings.Contains(out.stderr, "fednet client read-thread") || len(w.prompts()) != 0 || w.suspects() != "" {
		t.Errorf("%+v %q %q", out, w.prompts(), w.suspects())
	}
	// fednet answers, but not for C5: the thread is skipped, the rest runs.
	if err := os.Remove(filepath.Join(w.dir, "fednet-down")); err != nil {
		t.Fatal(err)
	}
	out = w.watchAt(2*time.Minute, 5)
	if !strings.Contains(out.stderr, "thread C5/5.0 skipped: fednet client read-thread C5/5.0 failed (exit status: 1): no thread C5/5.0") {
		t.Errorf("%+v", out)
	}
	if prompts := w.prompts(); len(prompts) != 1 || prompts[0][0] != "n-lead" || w.suspects() != "n-w" {
		t.Errorf("%q %q", prompts, w.suspects())
	}
}

func TestWatchPostsNothingWhenItCannotStartAThreadAgent(t *testing.T) {
	w := watchThreadWorld(t)
	checkout := filepath.Join(w.dir, "home", "dev", "r")
	w.exec("INSERT INTO threads (thread, slug, mapping, cwd, created_at) VALUES ('C6/6.0', 'c6-6-0', 'repo-r', '"+checkout+"', 0)",
		"INSERT INTO jobs (job, lead_cwd, home_thread, state, started_at) VALUES ('o', '/c', 'C6/6.0', 'open', 0)")
	if err := os.WriteFile(filepath.Join(w.dir, "home", ".config", "fleet", scope+".json"),
		[]byte(`{"linear": {"team": "EX"}, "fednet": {"socket": "/run/example/fednet.sock"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	w.lastMessage("C6/6.0", t0)
	// No checkout on this machine, then Linear down: no agent, no post.
	out := w.watchAt(30*time.Minute, 5)
	if !strings.Contains(out.stderr, "is not checked out at") {
		t.Errorf("%+v", out)
	}
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.dir, "atb-down"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out = w.watchAt(31*time.Minute, 5)
	if !strings.Contains(out.stderr, "Linear is unavailable") || !strings.Contains(w.log("atb.log"), "linear create --team EX") {
		t.Errorf("%+v %s", out, w.log("atb.log"))
	}
	if got := w.log("fednet.log"); strings.Contains(got, "client post") {
		t.Errorf("posted: %s", got)
	}
	if got := w.query("SELECT quiet_asked || ' ' || sessions FROM threads"); got != " 0" {
		t.Errorf("thread row %q", got)
	}
}

func TestWatchDoesNotSuspectABlockedAgent(t *testing.T) {
	w := newWatchWorld(t)
	anHourAgo := db.Now() - 3600
	blocked := "Do you want to proceed?\n❯ 1. Yes\n"
	w.insert(watchRow{name: "b-lead", role: "lead", job: "b", parent: "thread-1", startedAt: anHourAgo})
	w.insert(watchRow{name: "b-w1", role: "worker", job: "b", parent: "b-lead", startedAt: anHourAgo,
		lastStatus: "blocked", lastSeq: 3, lastScreenHash: cmd.Hash(cmd.FilterClaudeScreen(blocked)), lastChangeAt: anHourAgo})
	// The people were already asked about its screen, so the unblocking
	// (judge/unblock.sh) waits for them and does nothing here.
	if _, err := w.ledger.Exec("INSERT INTO questions (job, thread, asked_by, text, state, asked_at) " +
		"SELECT 'b', 'C1/1.0', 'unblock-' || id, 'what to press?', 'pending', 0 FROM agents WHERE name = 'b-w1'"); err != nil {
		t.Fatal(err)
	}
	w.herdrList("b-lead working 2", "b-w1 blocked 3")
	w.screen("b-lead", claude("⏺ Planning", "1s", "2hr 59m"))
	w.screen("b-w1", blocked)
	if out := w.run("", []string{"watch", "--scope", scope}); out.code != 0 || w.suspects() != "" || len(w.prompts()) != 0 {
		t.Errorf("%+v %q %q", out, w.suspects(), w.prompts())
	}
}

func TestWatchRunsOnceAtATimeAndChecksItsClock(t *testing.T) {
	w := newWatchWorld(t)
	w.herdrList()
	if out := w.run("", []string{"watch", "--scope", scope}, "FLEET_WATCH_NOW=soon"); out.code != 1 ||
		!strings.Contains(out.stderr, "FLEET_WATCH_NOW") {
		t.Errorf("%+v", out)
	}
	lock, err := os.OpenFile(filepath.Join(w.dir, "home", ".local", "state", "fleet", scope+".watch.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if out := w.run("", []string{"watch", "--scope", scope}); out.code != 1 || !strings.Contains(out.stderr, "another fleet watch is running") {
		t.Errorf("%+v", out)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if out := w.run("", []string{"watch", "--scope", scope}); out.code != 0 {
		t.Errorf("%+v", out)
	}
}

func TestWatchAsksARevisitAgentsQuestionInTheLatestJobsThreadAndSkipsParentsWhenLinearIsDown(t *testing.T) {
	w := watchThreadWorld(t)
	w.exec("INSERT INTO threads (thread, slug, created_at) VALUES ('C3/3.0', 'c3-3-0', 0)",
		"INSERT INTO agents (name, role, thread, pane_id, state, started_at) VALUES ('thread-c3', 'thread', 'C3/3.0', 'p3', 'active', 0)",
		"INSERT INTO jobs (job, parent_issue, lead_cwd, home_thread, state, outcome, started_at, ended_at) VALUES "+
			"('r1', 'EX-5', '/c', 'C9/9.0', 'ended', 'done', 0, 1), ('r2', 'EX-5', '/c', 'C3/3.0', 'ended', 'done', 2, 3), "+
			"('r3', 'EX-5', '/c', '', 'ended', 'done', 4, 5)",
		"INSERT INTO jobs (job, parent_issue, lead_cwd, home_thread, state, started_at) VALUES ('o', 'EX-7', '/c', 'C3/3.0', 'open', 0)",
		fmt.Sprintf("INSERT INTO agents (name, role, parent_issue, pane_id, state, started_at) VALUES "+
			"('revisit-ex-5', 'revisit', 'EX-5', 'p5', 'active', %d)", t0))
	w.insert(watchRow{name: "o-lead", role: "lead", job: "o", parent: "thread-c3"})
	w.insert(watchRow{name: "o-w", role: "worker", job: "o", parent: "o-lead"})
	w.herdrList("thread-c3 idle 1", "o-lead idle 1", "revisit-ex-5 idle 2")
	w.screen("o-lead", claude("⏺ Waiting", "", ""))
	w.lastMessage("C3/3.0", t0)
	dir := filepath.Join(w.dir, "home", ".local", "state", "fleet", scope+"-unblock")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "revisit-ex-5-question.md"), []byte("Close EX-5? 0xQUESTION\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.dir, "atb-down"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	out := w.watchAt(time.Minute, 5)
	if !strings.Contains(out.stderr, "the parent issues are skipped this run, Linear could not be read") {
		t.Errorf("%s", out.stderr)
	}
	// Only the parents of jobs that are not open are asked for.
	if got := w.log("atb.log"); !strings.Contains(got, `id: {in: ["EX-5"]}`) || strings.Contains(got, "EX-7") {
		t.Errorf("atb: %s", got)
	}
	// The other rules still ran: the worker gone from herdr went to its lead.
	prompts := w.prompts()
	if len(prompts) != 2 || prompts[0][0] != "o-lead" || !strings.Contains(prompts[0][1], "o-w") {
		t.Fatalf("%q", prompts)
	}
	// The finished revisit agent is closed, its question posted to the
	// latest job's thread that has one, and given to that thread's agent.
	if got := w.log("fednet.log"); !strings.Contains(got, "post -socket /run/example/fednet.sock -thread C3/3.0 -- revisit-ex-5 "+
		"looked at the parent issue EX-5, which has not changed for a while and has no job open on it, and asks: | | Close EX-5? 0xQUESTION") {
		t.Errorf("fednet: %s", got)
	}
	if got := w.query("SELECT job || '|' || thread || '|' || asked_by || '|' || state FROM questions"); got != "|C3/3.0|revisit-ex-5|pending" {
		t.Errorf("question %s", got)
	}
	if prompts[1][0] != "thread-c3" {
		t.Fatalf("%q", prompts)
	}
	for _, want := range []string{"[FROM: watch]\n", "rule `parent issue unchanged`: revisit-ex-5 asked the people in thread C3/3.0 about the parent issue EX-5",
		"0xQUESTION", "When they answer, carry out what they decide"} {
		if !strings.Contains(prompts[1][1], want) {
			t.Errorf("%q missing:\n%s", want, prompts[1][1])
		}
	}
	if got := w.query("SELECT state FROM agents WHERE name = 'revisit-ex-5'"); got != "ended" {
		t.Errorf("revisit row %s", got)
	}
	if data, _ := os.ReadFile(filepath.Join(w.dir, "closed")); string(data) != "p5\n" {
		t.Errorf("closed %q", data)
	}
	if _, err := os.Stat(filepath.Join(dir, "revisit-ex-5-question.md")); !os.IsNotExist(err) {
		t.Errorf("question file left: %v", err)
	}

	// Days later the question still waits: reminded of, never timed out,
	// and the thread agent's session is not reclaimed for it.
	if err := os.Remove(filepath.Join(w.dir, "atb-down")); err != nil {
		t.Fatal(err)
	}
	w.lastMessage("C3/3.0", t0+100*3600)
	w.watchAt(100*time.Hour, 0)
	if got := w.query("SELECT state || ' ' || reminders FROM questions"); got != "pending 3" {
		t.Errorf("question %s", got)
	}
	if got := w.query("SELECT coalesce(reclaim_at, 'none') FROM agents WHERE name = 'thread-c3'"); got != "none" {
		t.Errorf("reclaim_at %s", got)
	}
}

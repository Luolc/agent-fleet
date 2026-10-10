// `fleet watch` against a fake herdr with a screen per agent, with ledger
// rows inserted directly. Hermetic, see main_test.go.
package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/Luolc/agent-fleet/internal/cmd"
	"github.com/Luolc/agent-fleet/internal/db"
)

const rule = "────────────────────────────────────────"

// watchHerdr is the stand-in for herdr in the watch tests, with the world's
// paths baked in. `agent list` prints <dir>/list.json; `agent read <name>`
// prints <dir>/screens/<name> and fails like herdr when there is none;
// `agent prompt` appends the target and the text to <dir>/prompts and
// answers with `reply`.
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
		{"[FROM: cron]\n", "a-w2 (worker, job a, parent a-lead): no change for 1h00m", "Running the migration"},
		{"[FROM: cron]\n", "b-w1 (worker, job b, parent b-lead): gone from herdr"},
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

func TestWatchRefusesABadScope(t *testing.T) {
	w := newWatchWorld(t)
	if out := w.run("", []string{"watch", "--scope", "a/b"}); out.code != 1 {
		t.Errorf("%+v", out)
	}
}

package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
)

// idleClaude is Claude Code at its input box after `transcript`.
func idleClaude(transcript string) string {
	return transcript + "\n\n" + rule + "\n❯ \n" + rule + "\n  ⏵⏵ bypass permissions on\n"
}

// The two errors a proxy in front of the API gives when its quota runs
// out: in a session, after Claude Code's `API Error`, and before Claude
// Code started, the pane left at the shell.
const (
	proxyError = "API Error: Server is temporarily limiting requests (not your usage limit) · example-proxy: quota " +
		"exhausted, earliest reset 2031-01-02T03:04:05Z"
	proxyShell = "❯ claude\nexample-proxy: quota exhausted or rate-limited, earliest recovery 2031-01-02T06:07:08Z\n\n" +
		" ~/dev/example  main\n❯ \n"
)

func TestOnlyTheAgentsOwnQuotaErrorAtTheEndOfItsTranscriptCounts(t *testing.T) {
	for name, c := range map[string]struct {
		status, screen string
		want           quotaState
		reset          string
	}{
		"a proxy's error": {"idle", idleClaude("> go on\n  ⎿  " + proxyError), quotaShown, "2031-01-02T03:04:05Z"},
		"wrapped, under a tip": {"done", idleClaude("● API Error: Server is temporarily limiting requests (not your\n" +
			"  usage limit) · example-proxy: quota exhausted, earliest reset\n  2031-01-02T03:04:05Z\n\n" +
			"  ⎿  Tip: press ? for shortcuts"), quotaShown, "2031-01-02T03:04:05Z"},
		"blocked at its input box": {"blocked", idleClaude("  ⎿  " + proxyError), quotaShown, "2031-01-02T03:04:05Z"},
		"429": {"idle", idleClaude(`  ⎿  API Error: 429 {"type":"error","error":{"type":"rate_limit_error"}}`),
			quotaShown, "unknown"},
		"another API error": {"idle", idleClaude(`  ⎿  API Error: 500 {"type":"error","error":{"type":"api_error"}}`),
			quotaClear, ""},
		"the words quoted": {"idle", idleClaude("⏺ The proxy said: quota exhausted, earliest reset " +
			"2031-01-02T03:04:05Z.\n  The test reads that line."), quotaClear, ""},
		"a reply after it": {"idle", idleClaude("  ⎿  " + proxyError + "\n> go on\n⏺ Done."), quotaClear, ""},
		"plain idle":       {"idle", idleClaude("⏺ Done."), quotaClear, ""},
		"no input box":     {"idle", "⏺ Done.\n", quotaUnknown, ""},
		"usage-limit menu": {"blocked", limitMenu, quotaUnknown, ""},
		"another dialog":   {"blocked", bashPrompt, quotaClear, ""},
	} {
		got, reset := quotaOnScreen(c.status, c.screen, "/w/x")
		if got != c.want || reset != c.reset {
			t.Errorf("%s: %v %q, want %v %q", name, got, reset, c.want, c.reset)
		}
	}
	for name, c := range map[string]struct {
		screen string
		want   quotaState
		reset  string
	}{
		"the proxy's line": {proxyShell, quotaShown, "2031-01-02T06:07:08Z"},
		"its time cut by the wrap": {strings.Replace(proxyShell, "2031-01-0", "2031-01-0\n", 1), quotaShown,
			"2031-01-02T06:07:08Z"},
		"a reset that is no time": {strings.Replace(proxyShell, "earliest recovery 2031-01-02T06:07:08Z",
			"resets in a while", 1), quotaShown, "unknown"},
		"commands after it": {proxyShell + "❯ ls\nnotes.md\n", quotaUnknown, ""},
	} {
		if got, reset := quotaOnShell(c.screen); got != c.want || reset != c.reset {
			t.Errorf("shell, %s: %v %q, want %v %q", name, got, reset, c.want, c.reset)
		}
	}
	if reset, ok := dialogQuota(limitMenu, "/w/x"); !ok || reset != "3pm (UTC)" {
		t.Errorf("the usage-limit menu: %q, %v", reset, ok)
	}
}

// One message per thread names the agents that newly show a quota error;
// a later run that still finds them posts nothing, one that finds an
// agent past it says so once, and its next error is told again.
func TestAQuotaErrorIsToldOnceAndItsEndOnce(t *testing.T) {
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	fednet := "#!/bin/sh\n[ -f " + filepath.Join(bin, "down") + " ] && exit 1\nprintf '%s\\000' \"$*\" >> " + calls + "\n"
	herdrCalls := filepath.Join(bin, "herdr-calls")
	herdrFake := "#!/bin/sh\nif [ \"$1\" = --session ]; then shift 2; fi\nprintf '%s\\n' \"$*\" >> \"" + herdrCalls +
		"\"\ncat \"" + bin + "/$1-$3\"\n"
	for name, script := range map[string]string{"fednet": fednet, "herdr": herdrFake} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	show := func(kind, name, screen string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, kind+"-"+name), []byte(screen), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := db.OpenAt(filepath.Join(t.TempDir(), "quota.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sc, err := config.ParseScope([]byte(`{"fednet": {"socket": "/run/fednet.sock"}}`), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"INSERT INTO jobs (job, lead_cwd, state, started_at, home_thread) VALUES ('j', '/w/j', 'open', 0, 'C1/1.0')",
		"INSERT INTO agents (name, role, job, parent, cwd, thread, state, started_at, pane_id) VALUES " +
			"('j-lead', 'lead', 'j', 'thread-c1', '/w/j', '', 'active', 0, 'p1'), " +
			"('j-impl', 'worker', 'j', 'j-lead', '/w/j', '', 'active', 0, 'p2'), " +
			"('j-new', 'worker', 'j', 'j-lead', '/w/j', '', 'starting', 0, 'p3'), " +
			"('j-menu', 'worker', 'j', 'j-lead', '/w/j', '', 'active', 0, 'p4'), " +
			"('thread-c2', 'thread', '', '', '/w/c2', 'C2/2.0', 'active', 0, 'p5'), " +
			"('watch-x-1', 'worker', '', 'watch', '/w/x', '', 'active', 0, 'p6')",
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	u := testWorkers(conn, sc)
	u.h = herdr.New(nil)
	u.inHerdr = map[string]InHerdr{"j-lead": {Status: "idle"}, "j-impl": {Status: "done"}, "j-menu": {Status: "blocked"},
		"thread-c2": {Status: "idle"}, "watch-x-1": {Status: "idle"}}
	u.panes = map[string]herdrPane{"p3": {}}
	show("agent", "j-lead", idleClaude("  ⎿  "+proxyError))
	show("agent", "j-impl", idleClaude(`  ⎿  API Error: 429 {"type":"error","error":{"type":"rate_limit_error"}}`))
	show("pane", "p3", proxyShell)
	show("agent", "j-menu", limitMenu)
	show("agent", "thread-c2", idleClaude("⏺ The proxy said: quota exhausted, earliest reset 2031-01-02T03:04:05Z."))
	show("agent", "watch-x-1", idleClaude("  ⎿  "+proxyError))
	posts := func() []string {
		t.Helper()
		got, _ := os.ReadFile(calls)
		if len(got) == 0 {
			return nil
		}
		return strings.Split(strings.TrimSuffix(string(got), "\x00"), "\x00")
	}
	run := func(at int64, want exit.Code) {
		t.Helper()
		u.now, u.start, u.code = at, time.Now(), exit.Ok
		if err := u.quota(); err != nil || u.code != want {
			t.Fatalf("run at %d: %v, exit %d", at, err, u.code)
		}
	}
	host := machine()
	hit := "client post -socket /run/fednet.sock -thread C1/1.0 -- fleet watch on " + host + " found an error naming " +
		"a quota, usage limit or rate limit on these agents' screens (fleet's own check, no model called):\n"
	// The first post fails: nothing is marked posted, and the next run
	// posts it, with the time the first run found the errors.
	down := filepath.Join(bin, "down")
	if err := os.WriteFile(down, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	run(1700000000, exit.Environment)
	if err := os.Remove(down); err != nil {
		t.Fatal(err)
	}
	run(1700000300, exit.Ok)
	want := hit + "- j-lead: seen since 2023-11-14T22:13:20Z; reset: 2031-01-02T03:04:05Z\n" +
		"- j-impl: seen since 2023-11-14T22:13:20Z; reset: unknown\n" +
		"- j-new: seen since 2023-11-14T22:13:20Z; reset: 2031-01-02T06:07:08Z\n"
	if got := posts(); len(got) != 1 || got[0] != want {
		t.Fatalf("the post after the failed one: %q\nwant %q", got, want)
	}
	run(1700000600, exit.Ok)
	if got := posts(); len(got) != 1 {
		t.Fatalf("the same screens again posted %q", got[1:])
	}
	// j-lead got past it; j-impl is working, which is no recovery.
	show("agent", "j-lead", idleClaude("  ⎿  "+proxyError+"\n> go on\n⏺ Done."))
	show("agent", "j-impl", idleClaude("⏺ Done."))
	u.inHerdr["j-impl"] = InHerdr{Status: "working"}
	run(1700000900, exit.Ok)
	want = "client post -socket /run/fednet.sock -thread C1/1.0 -- fleet watch on " + host + " no longer finds that " +
		"error on these agents' screens:\n- j-lead: seen since 2023-11-14T22:13:20Z; gone by 2023-11-14T22:28:20Z\n"
	if got := posts(); len(got) != 2 || got[1] != want {
		t.Fatalf("after j-lead got past it, posted %q\nwant %q", got[1:], want)
	}
	show("agent", "j-lead", idleClaude("  ⎿  "+proxyError))
	if _, err := conn.Exec("UPDATE agents SET state = 'ended' WHERE name = 'j-impl'"); err != nil {
		t.Fatal(err)
	}
	run(1700001200, exit.Ok)
	want = hit + "- j-lead: seen since 2023-11-14T22:33:20Z; reset: 2031-01-02T03:04:05Z\n"
	if got := posts(); len(got) != 3 || got[2] != want {
		t.Fatalf("j-lead's next error posted %q\nwant %q", got[2:], want)
	}
	var left int
	if err := conn.QueryRow("SELECT count(*) FROM steps WHERE key = 'quota:2'").Scan(&left); err != nil || left != 0 {
		t.Errorf("the ended j-impl keeps %d step(s), %v", left, err)
	}
	// herdr was only asked for screens: no key was pressed, no text sent.
	read, err := os.ReadFile(herdrCalls)
	if err != nil || len(read) == 0 {
		t.Fatalf("no herdr call recorded: %v", err)
	}
	screenRead := regexp.MustCompile(`^(agent|pane) read \S+ --source visible$`)
	for _, call := range strings.Split(strings.TrimSuffix(string(read), "\n"), "\n") {
		if !screenRead.MatchString(call) {
			t.Errorf("herdr was called with %q, not only to read a screen", call)
		}
	}
}

// The card about an agent at a usage-limit menu gives the machine and the
// reset time the menu gives.
func TestTheUsageLimitCardNamesTheMachineAndTheReset(t *testing.T) {
	bin := t.TempDir()
	fake := "#!/bin/sh\necho '{\"id\":\"card-1\",\"channel\":\"C1\",\"ts\":\"1.0\",\"permalink\":\"https://slack.example.test/card-1\"}'\n"
	if err := os.WriteFile(filepath.Join(bin, "fednet"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	conn, err := db.OpenAt(filepath.Join(t.TempDir(), "screens.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sc, err := config.ParseScope([]byte(`{"fednet": {"socket": "/run/fednet.sock"}}`), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	u := testWorkers(conn, sc)
	row := stopRow{ID: 7, Name: "thread-c1", Role: "thread", Thread: "C1/1.0", State: "active"}
	if err := u.handle(u.builtinWatch(config.BlockedScreens), false, row, limitMenu); err != nil {
		t.Fatal(err)
	}
	var card string
	if err := conn.QueryRow("SELECT text FROM questions").Scan(&card); err != nil ||
		!strings.Contains(card, "Machine: "+machine()+"; reset, as the screen gives it: 3pm (UTC).") {
		t.Errorf("card %s, %v", card, err)
	}
}

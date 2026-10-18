package cmd

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
)

const (
	handoffFrom  = "C0SRC/1700000000.100"
	handoffAgent = "thread-c0src-1700000000-100"
	handoffTo    = "C0DST/1700000500.000"
)

// handoffSetup is a thread agent of repo-example-a with its live row and
// thread ticket EX-1, fake fednet and atb that log their calls, and a
// checkout of example-b; fednet's routes name #repo-example-b, with no
// machine, unless the file routes.json in the log directory gives them,
// and fail while the file routes-down is there; whoami names this machine
// machine-here, and fails while whoami-fails is there. It returns
// the ledger, the task file and the directory the calls are logged in.
func handoffSetup(t *testing.T) (*sql.DB, string, string) {
	t.Helper()
	home, bin := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	scripts := map[string]string{
		"fednet": "#!/bin/sh\necho \"$*\" >> " + filepath.Join(bin, "fednet.log") + "\n" +
			"[ \"$2\" = routes ] && [ -f " + filepath.Join(bin, "routes-down") + " ] && exit 4\n" +
			"[ \"$2\" = routes ] && [ -f " + filepath.Join(bin, "routes.json") + " ] && { cat " + filepath.Join(bin, "routes.json") + "; exit 0; }\n" +
			"[ \"$2\" = routes ] && { echo '{\"routes\":[{\"channel\":\"C0DST\",\"name\":\"repo-example-b\"}]}'; exit 0; }\n" +
			"[ \"$2\" = whoami ] && [ -f " + filepath.Join(bin, "whoami-fails") + " ] && exit 2\n" +
			"[ \"$2\" = whoami ] && { echo '{\"text\":\"machine-here\"}'; exit 0; }\n" +
			"[ \"$2\" = open-thread ] && { echo \"$6/1700000500.000\"; exit 0; }\n" +
			"[ -f " + filepath.Join(bin, "post-down") + " ] && [ \"$2\" = post ] && exit 4\n" +
			"[ -f " + filepath.Join(bin, "mention-refused") + " ] && [ \"$7\" = -mention ] && " +
			"{ echo 'U0NOBODY is not on the user list' >&2; exit 3; }\necho m-1\n",
		"atb": "#!/bin/sh\necho \"$*\" >> " + filepath.Join(bin, "atb.log") + "\n",
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	for _, dir := range []string{filepath.Join(home, "config", "fleet"), filepath.Join(home, "dev", "example-b")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "config", "fleet", "main.json"),
		[]byte(`{"linear": {"team": "EX"}, "fednet": {"socket": "/run/fednet.sock"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"FLEET_AGENT": handoffAgent, "FLEET_ROLE": "thread", "FLEET_SCOPE": "main",
		"FLEET_THREAD": handoffFrom, "FLEET_ISSUE": "EX-1", "FLEET_PARENT": "", "FLEET_JOB": ""} {
		t.Setenv(k, v)
	}
	conn, err := db.Open("main")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	for _, stmt := range []string{
		"INSERT INTO threads (thread, slug, channel, ticket, mapping, cwd, sessions, created_at) VALUES ('" + handoffFrom +
			"', 'c0src', 'C0SRC', 'EX-1', 'repo-example-a', '/w/a', 1, 0)",
		"INSERT INTO agents (name, role, thread, issue, state, started_at) VALUES ('" + handoffAgent + "', 'thread', '" +
			handoffFrom + "', 'EX-1', 'active', 0)",
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	task := filepath.Join(home, "task.md")
	if err := os.WriteFile(task, []byte("# Import the B table\n\nThe people asked for it at C0SRC/1700000000.100, ts 1700000001.000.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return conn, task, bin
}

// fakeHandoffRoute stands in for the start of the new thread's agent: it
// records each message, and the first gives the thread its ticket as a
// start does; while `fail` is set it fails as a delivery not found.
func fakeHandoffRoute(t *testing.T, fail *bool) *[]inboundMessage {
	t.Helper()
	var got []inboundMessage
	handoffRoute = func(_ *herdr.Herdr, conn *sql.DB, _ string, _ *config.Scope, msg inboundMessage) (exit.Code, error) {
		got = append(got, msg)
		if *fail {
			return exit.NotFound, nil
		}
		_, err := conn.Exec("UPDATE threads SET ticket = 'EX-2' WHERE thread = ?1", msg.Thread)
		return exit.Ok, err
	}
	t.Cleanup(func() { handoffRoute = toThread })
	return &got
}

// fednetCalls are fednet's logged calls but the marks.
func fednetCalls(t *testing.T, bin string) []string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(bin, "fednet.log"))
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" && !strings.HasPrefix(line, "client react") {
			calls = append(calls, line)
		}
	}
	return calls
}

func TestAHandoffOpensAThreadStartsItsAgentTellsTheThreadAndRelatesTheTickets(t *testing.T) {
	conn, task, bin := handoffSetup(t)
	if err := os.WriteFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "fleet", "main.json"), []byte(`{"linear": {"team": "EX"}, `+
		`"fednet": {"socket": "/run/fednet.sock", "slack_url": "https://slack.example.test/"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fail := false
	got := fakeHandoffRoute(t, &fail)
	if code, err := ThreadHandoff(nil, ThreadHandoffArgs{Repo: "example-b", File: task}); err != nil || code != exit.Ok {
		t.Fatalf("exit %d, %v", code, err)
	}
	want := []string{
		"client routes -socket /run/fednet.sock -json",
		"client open-thread -socket /run/fednet.sock -channel C0DST -- 从 <#C0SRC> 的" +
			"[原线程](https://slack.example.test/archives/C0SRC/p1700000000100)转来：Import the B table (" + handoffAgent + ")",
		"client post -socket /run/fednet.sock -thread " + handoffFrom + " -- 已转到 <#C0DST> 的" +
			"[新线程](https://slack.example.test/archives/C0DST/p1700000500000)",
	}
	if calls := fednetCalls(t, bin); strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("fednet calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
	if atbLog, _ := os.ReadFile(filepath.Join(bin, "atb.log")); string(atbLog) != "linear relate EX-2 EX-1\n" {
		t.Errorf("atb calls %q", atbLog)
	}
	if len(*got) != 1 {
		t.Fatalf("%d deliveries", len(*got))
	}
	msg := (*got)[0]
	if msg.Thread != handoffTo || msg.sender() != handoffAgent || !msg.fromFleet() ||
		!strings.HasPrefix(msg.body(), "[HANDOFF from "+handoffAgent+" in thread "+handoffFrom+"]\n") ||
		!strings.Contains(msg.body(), "Import the B table") {
		t.Errorf("message %+v, body %q", msg, msg.body())
	}
	var mapping, cwd, last, fromTicket string
	if err := conn.QueryRow("SELECT mapping, cwd, last_text || last_user || last_ts FROM threads WHERE thread = ?1", handoffTo).
		Scan(&mapping, &cwd, &last); err != nil {
		t.Fatal(err)
	}
	if mapping != "repo-example-b" || cwd != filepath.Join(os.Getenv("HOME"), "dev", "example-b") || last != "" {
		t.Errorf("new thread: mapping %q, cwd %q, latest from a person %q", mapping, cwd, last)
	}
	if err := conn.QueryRow("SELECT ticket FROM threads WHERE thread = ?1", handoffFrom).Scan(&fromTicket); err != nil ||
		fromTicket != "EX-1" {
		t.Errorf("the source thread's ticket %q, %v", fromTicket, err)
	}

	// Run again: every step is done, so nothing happens twice.
	if code, err := ThreadHandoff(nil, ThreadHandoffArgs{Repo: "example-b", File: task}); err != nil || code != exit.Ok {
		t.Fatalf("again: exit %d, %v", code, err)
	}
	if calls := fednetCalls(t, bin); len(calls) != len(want) || len(*got) != 1 {
		t.Errorf("again: fednet calls %q, %d deliveries", calls, len(*got))
	}
}

func TestAHandoffWithoutTheWorkspaceAddressLinksNoThreadAndShowsNoKey(t *testing.T) {
	_, task, bin := handoffSetup(t)
	fail := false
	fakeHandoffRoute(t, &fail)
	if code, err := ThreadHandoff(nil, ThreadHandoffArgs{Repo: "example-b", File: task}); err != nil || code != exit.Ok {
		t.Fatalf("exit %d, %v", code, err)
	}
	want := []string{
		"client routes -socket /run/fednet.sock -json",
		"client open-thread -socket /run/fednet.sock -channel C0DST -- 从 <#C0SRC> 的一个线程转来：Import the B table (" + handoffAgent + ")",
		"client post -socket /run/fednet.sock -thread " + handoffFrom + " -- 已转到 <#C0DST> 的一个新线程",
	}
	if calls := fednetCalls(t, bin); strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("fednet calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

// A handoff to the caller's own channel opens a new thread beside the
// caller's, taking the channel's id from the caller's thread.
func TestAHandoffToItsOwnChannelOpensANewThreadThere(t *testing.T) {
	conn, task, bin := handoffSetup(t)
	if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), "dev", "example-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	fail := false
	got := fakeHandoffRoute(t, &fail)
	if code, err := ThreadHandoff(nil, ThreadHandoffArgs{Repo: "example-a", File: task}); err != nil || code != exit.Ok {
		t.Fatalf("exit %d, %v", code, err)
	}
	to := "C0SRC/1700000500.000"
	want := []string{
		"client routes -socket /run/fednet.sock -json",
		"client open-thread -socket /run/fednet.sock -channel C0SRC -- 从 <#C0SRC> 的一个线程转来：Import the B table (" + handoffAgent + ")",
		"client post -socket /run/fednet.sock -thread " + handoffFrom + " -- 已转到 <#C0SRC> 的一个新线程",
	}
	if calls := fednetCalls(t, bin); strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("fednet calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
	if atbLog, _ := os.ReadFile(filepath.Join(bin, "atb.log")); string(atbLog) != "linear relate EX-2 EX-1\n" {
		t.Errorf("atb calls %q", atbLog)
	}
	if len(*got) != 1 || (*got)[0].Thread != to {
		t.Fatalf("deliveries %+v", *got)
	}
	var mapping, cwd string
	if err := conn.QueryRow("SELECT mapping, cwd FROM threads WHERE thread = ?1", to).Scan(&mapping, &cwd); err != nil {
		t.Fatal(err)
	}
	if mapping != "repo-example-a" || cwd != filepath.Join(os.Getenv("HOME"), "dev", "example-a") {
		t.Errorf("new thread: mapping %q, cwd %q", mapping, cwd)
	}
}

// A delivery that fails leaves the thread open and recorded, and the
// source thread already told where the work went: the next run delivers
// to that thread, opens no other and posts no second line.
func TestAHandoffRunAgainAfterAFailedDeliveryOpensNoSecondThread(t *testing.T) {
	conn, task, bin := handoffSetup(t)
	fail := true
	got := fakeHandoffRoute(t, &fail)
	_, err := ThreadHandoff(nil, ThreadHandoffArgs{Repo: "example-b", File: task})
	if code(err) != exit.NotFound || !strings.Contains(err.Error(), "run thread handoff again") {
		t.Fatalf("first run: %v", err)
	}
	told := "client post -socket /run/fednet.sock -thread " + handoffFrom + " -- 已转到 <#C0DST> 的一个新线程"
	if calls := fednetCalls(t, bin); calls[len(calls)-1] != told {
		t.Errorf("first run: fednet calls %q, want the last %q", calls, told)
	}
	fail = false
	if code, err := ThreadHandoff(nil, ThreadHandoffArgs{Repo: "example-b", File: task}); err != nil || code != exit.Ok {
		t.Fatalf("second run: exit %d, %v", code, err)
	}
	opened, posted := 0, 0
	for _, call := range fednetCalls(t, bin) {
		if strings.HasPrefix(call, "client open-thread ") {
			opened++
		}
		if call == told {
			posted++
		}
	}
	if opened != 1 || posted != 1 || len(*got) != 2 || (*got)[1].Thread != handoffTo {
		t.Errorf("%d threads opened, %d lines posted, deliveries %+v", opened, posted, *got)
	}
	var rows int
	if err := conn.QueryRow("SELECT count(*) FROM handoffs WHERE to_thread = ?1 AND done_at IS NOT NULL", handoffTo).Scan(&rows); err != nil || rows != 1 {
		t.Errorf("%d done handoffs, %v", rows, err)
	}
}

// Refusals come before anything is recorded or opened.
func TestAHandoffIsRefusedBeforeAnythingForOtherCallersAndAnUnknownChannel(t *testing.T) {
	conn, task, bin := handoffSetup(t)
	fail := false
	got := fakeHandoffRoute(t, &fail)
	if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), "dev", "example-c"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, role, repo, mention, want string
	}{
		{"a lead", "lead", "example-b", "", "a lead does not hand work off"},
		{"a worker", "worker", "example-b", "", "a worker does not hand work off"},
		{"a channel without an id", "thread", "example-c", "", "fleet knows no id of #repo-example-c"},
		{"a repo not checked out", "thread", "example-d", "", "not on this machine"},
		{"a mention that is no user id", "thread", "example-b", "@alice", "is not a Slack user id"},
	} {
		t.Setenv("FLEET_ROLE", c.role)
		args := ThreadHandoffArgs{Repo: c.repo, File: task}
		if c.mention != "" {
			args.Mentions = []string{c.mention}
		}
		_, err := ThreadHandoff(nil, args)
		if code(err) != exit.Refused || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	var rows int
	if err := conn.QueryRow("SELECT (SELECT count(*) FROM handoffs) + (SELECT count(*) FROM steps)").Scan(&rows); err != nil || rows != 0 {
		t.Errorf("%d rows, %v", rows, err)
	}
	for _, call := range fednetCalls(t, bin) {
		if !strings.HasPrefix(call, "client routes ") {
			t.Errorf("fednet called: %s", call)
		}
	}
	if len(*got) != 0 {
		t.Errorf("%d deliveries", len(*got))
	}
}

// A thread without its ticket yet leaves the handoff unfinished: the
// next run, once both have one, relates them and opens nothing more.
func TestAHandoffRunAgainRelatesTheTicketsOnceBothThreadsHaveOne(t *testing.T) {
	conn, task, bin := handoffSetup(t)
	fail := false
	got := fakeHandoffRoute(t, &fail)
	if _, err := conn.Exec("UPDATE threads SET ticket = '' WHERE thread = ?1", handoffFrom); err != nil {
		t.Fatal(err)
	}
	if code, err := ThreadHandoff(nil, ThreadHandoffArgs{Repo: "example-b", File: task}); err != nil || code != exit.Ok {
		t.Fatalf("first run: exit %d, %v", code, err)
	}
	if atbLog, _ := os.ReadFile(filepath.Join(bin, "atb.log")); len(atbLog) != 0 {
		t.Errorf("first run: atb calls %q", atbLog)
	}
	if _, err := conn.Exec("UPDATE threads SET ticket = 'EX-1' WHERE thread = ?1", handoffFrom); err != nil {
		t.Fatal(err)
	}
	if code, err := ThreadHandoff(nil, ThreadHandoffArgs{Repo: "example-b", File: task}); err != nil || code != exit.Ok {
		t.Fatalf("second run: exit %d, %v", code, err)
	}
	if atbLog, _ := os.ReadFile(filepath.Join(bin, "atb.log")); string(atbLog) != "linear relate EX-2 EX-1\n" {
		t.Errorf("second run: atb calls %q", atbLog)
	}
	opened, posted := 0, 0
	for _, call := range fednetCalls(t, bin) {
		if strings.HasPrefix(call, "client open-thread ") {
			opened++
		}
		if strings.HasPrefix(call, "client post ") {
			posted++
		}
	}
	var done int
	if err := conn.QueryRow("SELECT count(*) FROM handoffs WHERE done_at IS NOT NULL").Scan(&done); err != nil ||
		opened != 1 || posted != 1 || len(*got) != 1 || done != 1 {
		t.Errorf("%d opened, %d posted, %d deliveries, %d done, %v", opened, posted, len(*got), done, err)
	}
}

// The person who asked for the handoff is mentioned in the new thread
// once, after the line in the source thread: a rerun mentions nobody
// again, whatever its --mention.
func TestAHandoffWithMentionMentionsThemInTheNewThreadOnce(t *testing.T) {
	ledger, task, bin := handoffSetup(t)
	fail := false
	fakeHandoffRoute(t, &fail)
	if _, err := ledger.Exec("UPDATE threads SET ticket = '' WHERE thread = ?1", handoffFrom); err != nil {
		t.Fatal(err)
	}
	args := ThreadHandoffArgs{Repo: "example-b", File: task, Mentions: []string{"U0PERSON", "W0OTHER"}}
	if code, err := ThreadHandoff(nil, args); err != nil || code != exit.Ok {
		t.Fatalf("exit %d, %v", code, err)
	}
	want := []string{
		"client routes -socket /run/fednet.sock -json",
		"client open-thread -socket /run/fednet.sock -channel C0DST -- 从 <#C0SRC> 的一个线程转来：Import the B table (" + handoffAgent + ")",
		"client post -socket /run/fednet.sock -thread " + handoffFrom + " -- 已转到 <#C0DST> 的一个新线程",
		"client post -socket /run/fednet.sock -thread " + handoffTo + " -mention U0PERSON -mention W0OTHER -- 你要求的转交在这里",
	}
	if calls := fednetCalls(t, bin); strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("fednet calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}

	// The handoff is unfinished (no ticket to relate yet), so a rerun runs.
	args.Mentions = []string{"U0LATER"}
	if code, err := ThreadHandoff(nil, args); err != nil || code != exit.Ok {
		t.Fatalf("again: exit %d, %v", code, err)
	}
	if calls := fednetCalls(t, bin); len(calls) != len(want) {
		t.Errorf("again: fednet calls %q", calls)
	}
}

// A mention fednet refuses fails the run after the line in the source
// thread; a rerun without it finishes the handoff.
func TestAHandoffWithARefusedMentionFinishesOnARunWithoutIt(t *testing.T) {
	c, task, bin := handoffSetup(t)
	fail := false
	got := fakeHandoffRoute(t, &fail)
	if err := os.WriteFile(filepath.Join(bin, "mention-refused"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ThreadHandoff(nil, ThreadHandoffArgs{Repo: "example-b", File: task, Mentions: []string{"U0NOBODY"}})
	if code(err) != exit.Environment || !strings.Contains(err.Error(), "U0NOBODY is not on the user list") ||
		!strings.Contains(err.Error(), "without that --mention") {
		t.Fatalf("first run: %v", err)
	}
	if code, err := ThreadHandoff(nil, ThreadHandoffArgs{Repo: "example-b", File: task}); err != nil || code != exit.Ok {
		t.Fatalf("second run: exit %d, %v", code, err)
	}
	posted := 0
	for _, call := range fednetCalls(t, bin) {
		if strings.HasPrefix(call, "client post -socket /run/fednet.sock -thread "+handoffFrom+" ") {
			posted++
		}
	}
	var done int
	if err := c.QueryRow("SELECT count(*) FROM handoffs WHERE done_at IS NOT NULL").Scan(&done); err != nil ||
		posted != 1 || len(*got) != 1 || done != 1 {
		t.Errorf("%d posted to the source thread, %d deliveries, %d done, %v", posted, len(*got), done, err)
	}
}

// handoffRoutes makes fednet's routes give #repo-example-b and
// #data-example to `machine`, and its whoami name this machine
// `machine-here`.
func handoffRoutes(t *testing.T, bin, machine string) {
	t.Helper()
	reply := `{"routes":[{"channel":"C0DST","machine":"` + machine + `","name":"repo-example-b"},` +
		`{"channel":"C0DATA","machine":"` + machine + `","name":"data-example"}]}`
	if err := os.WriteFile(filepath.Join(bin, "routes.json"), []byte(reply), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A target whose route gives its new threads to another machine, named
// by repo or by channel (another machine's prefix, no checkout here), is
// handed off there: the thread is opened as the route's, from the
// caller's thread, its first message carrying the task and the signed
// line, and the caller's thread is told; no agent starts here, nothing is
// recorded as a thread of this scope and no tickets are related. A rerun
// does nothing.
func TestAHandoffToAChannelRoutedToAnotherMachineOpensItsThreadThere(t *testing.T) {
	conn, task, bin := handoffSetup(t)
	fail := false
	got := fakeHandoffRoute(t, &fail)
	handoffRoutes(t, bin, "machine-elsewhere")
	if code, err := ThreadHandoff(nil, ThreadHandoffArgs{Channel: "data-example", File: task}); err != nil || code != exit.Ok {
		t.Fatalf("exit %d, %v", code, err)
	}
	log, _ := os.ReadFile(filepath.Join(bin, "fednet.log"))
	want := "client routes -socket /run/fednet.sock -json\n" +
		"client whoami -socket /run/fednet.sock -json\n" +
		"client open-thread -socket /run/fednet.sock -channel C0DATA -owner route -from-thread " + handoffFrom +
		" -- 从 <#C0SRC> 的一个线程转来：Import the B table (" + handoffAgent + ")\n\n# Import the B table\n\n" +
		"The people asked for it at C0SRC/1700000000.100, ts 1700000001.000.\n\n— " + handoffAgent + " · " + handoffFrom + "\n" +
		"client post -socket /run/fednet.sock -thread " + handoffFrom + " -- 已转到 <#C0DATA> 的一个新线程\n"
	if string(log) != want {
		t.Errorf("fednet calls:\n%s\nwant:\n%s", log, want)
	}
	if len(*got) != 0 {
		t.Errorf("deliveries %+v", *got)
	}
	if atbLog, _ := os.ReadFile(filepath.Join(bin, "atb.log")); len(atbLog) != 0 {
		t.Errorf("atb calls %q", atbLog)
	}
	var threads, done int
	if err := conn.QueryRow("SELECT (SELECT count(*) FROM threads WHERE thread = 'C0DATA/1700000500.000'), "+
		"(SELECT count(*) FROM handoffs WHERE to_thread = 'C0DATA/1700000500.000' AND done_at IS NOT NULL)").
		Scan(&threads, &done); err != nil || threads != 0 || done != 1 {
		t.Errorf("%d thread rows, %d done handoffs, %v", threads, done, err)
	}
	if code, err := ThreadHandoff(nil, ThreadHandoffArgs{Channel: "data-example", File: task}); err != nil || code != exit.Ok {
		t.Fatalf("again: exit %d, %v", code, err)
	}
	if again, _ := os.ReadFile(filepath.Join(bin, "fednet.log")); string(again) != want {
		t.Errorf("again: fednet called:\n%s", again[len(log):])
	}
}

// A run stopped after the thread for another machine was opened finishes
// on the next run without opening another or starting an agent here.
func TestAHandoffToAnotherMachineRunAgainFinishesWithoutASecondThread(t *testing.T) {
	_, task, bin := handoffSetup(t)
	fail := false
	got := fakeHandoffRoute(t, &fail)
	handoffRoutes(t, bin, "machine-elsewhere")
	if err := os.WriteFile(filepath.Join(bin, "post-down"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ThreadHandoff(nil, ThreadHandoffArgs{Repo: "example-b", File: task}); code(err) != exit.Environment {
		t.Fatalf("first run: %v", err)
	}
	if err := os.Remove(filepath.Join(bin, "post-down")); err != nil {
		t.Fatal(err)
	}
	if code, err := ThreadHandoff(nil, ThreadHandoffArgs{Repo: "example-b", File: task}); err != nil || code != exit.Ok {
		t.Fatalf("second run: exit %d, %v", code, err)
	}
	log, _ := os.ReadFile(filepath.Join(bin, "fednet.log"))
	if n := strings.Count(string(log), "client open-thread "); n != 1 || len(*got) != 0 {
		t.Errorf("%d threads opened, deliveries %+v", n, *got)
	}
}

// What cannot go to another machine is refused before anything is
// recorded or opened: a task over fednet's first message, a whoami that
// fails (a fednet before v0.9.0), routes fednet cannot give, a channel
// name with its #, and, on this machine, a name the scope's prefixes do
// not serve. A target routed to this machine, as whoami names it, is
// handed off here.
func TestAHandoffIsRefusedWhenItCannotGoWhereItsChannelIsRouted(t *testing.T) {
	conn, task, bin := handoffSetup(t)
	fail := false
	got := fakeHandoffRoute(t, &fail)
	handoffRoutes(t, bin, "machine-elsewhere")
	long := filepath.Join(t.TempDir(), "long.md")
	if err := os.WriteFile(long, []byte("# Long\n\n"+strings.Repeat("字", 4000)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ThreadHandoff(nil, ThreadHandoffArgs{Channel: "data-example", File: long}); code(err) != exit.Refused ||
		!strings.Contains(err.Error(), "over fednet's 4000") {
		t.Errorf("a long task: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bin, "whoami-fails"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ThreadHandoff(nil, ThreadHandoffArgs{Channel: "data-example", File: task}); code(err) != exit.Environment ||
		!strings.Contains(err.Error(), "fednet v0.9.0 or later") {
		t.Errorf("whoami fails: %v", err)
	}
	if err := os.Remove(filepath.Join(bin, "whoami-fails")); err != nil {
		t.Fatal(err)
	}
	if _, err := ThreadHandoff(nil, ThreadHandoffArgs{Channel: "#repo-example-b", File: task}); code(err) != exit.Refused ||
		!strings.Contains(err.Error(), "is not a Slack channel's name") {
		t.Errorf("a channel with its #: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), "dev", "example-unserved"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "routes.json"),
		[]byte(`{"routes":[{"channel":"C0UNS","machine":"machine-here","name":"example-unserved"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ThreadHandoff(nil, ThreadHandoffArgs{Channel: "example-unserved", File: task}); code(err) != exit.Refused ||
		!strings.Contains(err.Error(), "fleet does not serve #example-unserved on this machine") {
		t.Errorf("a channel without the scope's prefixes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bin, "routes-down"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ThreadHandoff(nil, ThreadHandoffArgs{Repo: "example-b", File: task}); code(err) != exit.Environment ||
		!strings.Contains(err.Error(), "fleet cannot tell which machine takes #repo-example-b") {
		t.Errorf("routes down: %v", err)
	}
	var rows int
	if err := conn.QueryRow("SELECT (SELECT count(*) FROM handoffs) + (SELECT count(*) FROM steps)").Scan(&rows); err != nil || rows != 0 {
		t.Errorf("%d rows, %v", rows, err)
	}
	for _, call := range fednetCalls(t, bin) {
		if !strings.HasPrefix(call, "client routes ") && !strings.HasPrefix(call, "client whoami ") {
			t.Errorf("fednet called: %s", call)
		}
	}
	if err := os.Remove(filepath.Join(bin, "routes-down")); err != nil {
		t.Fatal(err)
	}
	handoffRoutes(t, bin, "machine-here")
	if code, err := ThreadHandoff(nil, ThreadHandoffArgs{Channel: "repo-example-b", File: task}); err != nil || code != exit.Ok {
		t.Fatalf("routed here: exit %d, %v", code, err)
	}
	if len(*got) != 1 || (*got)[0].Thread != handoffTo {
		t.Errorf("deliveries %+v", *got)
	}
	if log, _ := os.ReadFile(filepath.Join(bin, "fednet.log")); strings.Contains(string(log), "-owner") {
		t.Errorf("routed here, opened as the route's:\n%s", log)
	}
}

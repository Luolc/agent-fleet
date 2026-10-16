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
// checkout of example-b; fednet's routes name #repo-example-b. It returns
// the ledger, the task file and the directory the calls are logged in.
func handoffSetup(t *testing.T) (*sql.DB, string, string) {
	t.Helper()
	home, bin := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	scripts := map[string]string{
		"fednet": "#!/bin/sh\necho \"$*\" >> " + filepath.Join(bin, "fednet.log") + "\n" +
			"[ \"$2\" = routes ] && { echo '{\"routes\":[{\"channel\":\"C0DST\",\"name\":\"repo-example-b\"}]}'; exit 0; }\n" +
			"[ \"$2\" = open-thread ] && { echo \"$6/1700000500.000\"; exit 0; }\n" +
			"[ -f " + filepath.Join(bin, "post-down") + " ] && [ \"$2\" = post ] && exit 4\necho m-1\n",
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
	fail := false
	got := fakeHandoffRoute(t, &fail)
	if code, err := ThreadHandoff(nil, ThreadHandoffArgs{Repo: "example-b", File: task}); err != nil || code != exit.Ok {
		t.Fatalf("exit %d, %v", code, err)
	}
	want := []string{
		"client routes -socket /run/fednet.sock -json",
		"client open-thread -socket /run/fednet.sock -channel C0DST -- 从#repo-example-a的线程 " + handoffFrom + " 转来 (" +
			handoffAgent + ")：Import the B table",
		"client post -socket /run/fednet.sock -thread " + handoffFrom + " -- 已转到 #repo-example-b 的线程 " + handoffTo,
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

// A delivery that fails leaves the thread open and recorded: the next
// run delivers to that thread and opens no other.
func TestAHandoffRunAgainAfterAFailedDeliveryOpensNoSecondThread(t *testing.T) {
	conn, task, bin := handoffSetup(t)
	fail := true
	got := fakeHandoffRoute(t, &fail)
	_, err := ThreadHandoff(nil, ThreadHandoffArgs{Repo: "example-b", File: task})
	if code(err) != exit.NotFound || !strings.Contains(err.Error(), "run thread handoff again") {
		t.Fatalf("first run: %v", err)
	}
	fail = false
	if code, err := ThreadHandoff(nil, ThreadHandoffArgs{Repo: "example-b", File: task}); err != nil || code != exit.Ok {
		t.Fatalf("second run: exit %d, %v", code, err)
	}
	opened := 0
	for _, call := range fednetCalls(t, bin) {
		if strings.HasPrefix(call, "client open-thread ") {
			opened++
		}
	}
	if opened != 1 || len(*got) != 2 || (*got)[1].Thread != handoffTo {
		t.Errorf("%d threads opened, deliveries %+v", opened, *got)
	}
	var rows int
	if err := conn.QueryRow("SELECT count(*) FROM handoffs WHERE to_thread = ?1 AND done_at IS NOT NULL", handoffTo).Scan(&rows); err != nil || rows != 1 {
		t.Errorf("%d done handoffs, %v", rows, err)
	}
}

// Refusals come before anything is recorded or opened.
func TestAHandoffIsRefusedBeforeAnythingForOtherCallersItsOwnChannelAndAnUnknownChannel(t *testing.T) {
	conn, task, bin := handoffSetup(t)
	fail := false
	got := fakeHandoffRoute(t, &fail)
	if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), "dev", "example-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), "dev", "example-c"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, role, repo, want string
	}{
		{"a lead", "lead", "example-b", "a lead does not hand work off"},
		{"a worker", "worker", "example-b", "a worker does not hand work off"},
		{"its own channel", "thread", "example-a", "#repo-example-a is your own thread's channel"},
		{"a channel without an id", "thread", "example-c", "fleet knows no id of #repo-example-c"},
		{"a repo not checked out", "thread", "example-d", "not on this machine"},
	} {
		t.Setenv("FLEET_ROLE", c.role)
		_, err := ThreadHandoff(nil, ThreadHandoffArgs{Repo: c.repo, File: task})
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

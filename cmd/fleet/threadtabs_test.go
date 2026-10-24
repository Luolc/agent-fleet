// `fleet watch` and `fleet status` on the tabs of the `threads` workspace
// that no live agent has, against the watch fake. Hermetic, see
// main_test.go; the shells of the panes are real processes, so that their
// child processes are read from /proc.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// childlessShell starts a process with no child process, the shell of a
// pane where nothing runs, and kills it when the test ends.
func childlessShell(t *testing.T) int {
	t.Helper()
	sleep := exec.Command("sleep", "600")
	if err := sleep.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sleep.Process.Kill()
		_ = sleep.Wait()
	})
	return sleep.Process.Pid
}

// procinfo sets what `herdr pane process-info` says of `pane`: `shell` its
// shell, alone in the foreground.
func (w *watchWorld) procinfo(pane string, shell int) {
	w.t.Helper()
	w.file("procinfo-"+pane, fmt.Sprintf(`{"result":{"process_info":{"foreground_process_group_id":%d,`+
		`"foreground_processes":[{"name":"zsh","pid":%d}],"pane_id":"%s","shell_pid":%d}}}`, shell, shell, pane, shell))
}

// A tab of the `threads` workspace is closed only when every criterion
// holds in two runs 10 minutes apart: its pane an ended thread row's, no
// live row on it, its label that thread's slug, no agent and nothing but
// the shell in it. A thread row left `starting` past the grace with no
// agent is ended first, and its tab closed the same way. The tabs watch
// leaves are listed by `status`.
func TestWatchClosesAnOrphanThreadTabOnlyWhenEveryCriterionHolds(t *testing.T) {
	w := newWatchWorld(t)
	idle, other := childlessShell(t), childlessShell(t)
	// The test process has a child while the binary runs: the fleet it runs.
	busy := os.Getpid()
	// thread-c0d-13-13's start is at its input box past the grace.
	w.herdrList("thread-c0d-13-13 idle 1")
	w.screen("thread-c0d-13-13", rule+"\n❯ \n"+rule+"\n")
	w.file("workspaces.json", `{"result":{"workspaces":[{"workspace_id":"w1","label":"threads"}]}}`)
	tabs := []string{`{"tab_id":"tab-p0","label":"shell","workspace_id":"w1"}`}
	panes := []string{`{"pane_id":"p0","tab_id":"tab-p0","label":"example-shell","workspace_id":"w1"}`}
	for _, p := range []struct{ pane, label, agent string }{
		{"p1", "c01-1-1", ""},       // closed
		{"p2", "c02-2-2", ""},       // its shell has a child process
		{"p3", "c03-3-3", ""},       // a live row records it
		{"p4", "c04-x", ""},         // not its thread's label
		{"p5", "c05-5-5", ""},       // no ledger row records it
		{"p6", "c06-6-6", "claude"}, // an agent runs in it
		{"p7", "c07-7-7", ""},       // its shell has a child process in the second run
		{"p8", "c08-8-8", ""},       // its start failed: closed
		{"p9", "c09-9-9", ""},       // still starting
		{"pa", "c0a-10-10", ""},     // a second pane in its tab
		{"pb", "c0b-11-11", ""},     // its shell is another in the second run
		{"pc", "c0c-12-12", ""},     // a process other than its shell in the foreground
	} {
		tabs = append(tabs, fmt.Sprintf(`{"tab_id":"tab-%s","label":"%s","workspace_id":"w1"}`, p.pane, p.label))
		panes = append(panes, fmt.Sprintf(`{"pane_id":"%s","tab_id":"tab-%s","agent":"%s","workspace_id":"w1"}`,
			p.pane, p.pane, p.agent))
	}
	// pa's tab has a second pane; thread-c0d-13-13 and thread-c0e-14-14
	// are still starting past the grace, the first in `agent list` (its
	// pane not showing it yet), the second with an agent in its pane.
	panes = append(panes, `{"pane_id":"pa2","tab_id":"tab-pa","workspace_id":"w1"}`,
		`{"pane_id":"pd","tab_id":"tab-pd","workspace_id":"w1"}`,
		`{"pane_id":"pe","tab_id":"tab-pe","agent":"claude","workspace_id":"w1"}`)
	w.file("tabs.json", `{"result":{"tabs":[`+strings.Join(tabs, ",")+`]}}`)
	w.file("panes.json", `{"result":{"panes":[`+strings.Join(panes, ",")+`]}}`)
	for _, pane := range []string{"p1", "p3", "p6", "p7", "p8", "pa", "pb"} {
		w.procinfo(pane, idle)
	}
	w.file("procinfo-pc", fmt.Sprintf(`{"result":{"process_info":{"foreground_process_group_id":%d,`+
		`"foreground_processes":[{"name":"vim","pid":%d}],"pane_id":"pc","shell_pid":%d}}}`, other, other, idle))
	w.procinfo("p2", busy)
	ended := fmt.Sprintf("'thread', 'ended', %d, %d", t0-7200, t0-3600)
	w.exec("INSERT INTO agents (name, role, state, started_at, ended_at, thread, pane_id) VALUES "+
		"('thread-c01-1-1', "+ended+", 'C01/1.1', 'p1'), "+
		"('thread-c02-2-2', "+ended+", 'C02/2.2', 'p2'), "+
		"('thread-c03-3-3', "+ended+", 'C03/3.3', 'p3'), "+
		"('thread-c04-4-4', "+ended+", 'C04/4.4', 'p4'), "+
		"('thread-c06-6-6', "+ended+", 'C06/6.6', 'p6'), "+
		"('thread-c07-7-7', "+ended+", 'C07/7.7', 'p7'), "+
		"('thread-c0a-10-10', "+ended+", 'C0A/10.10', 'pa'), "+
		"('thread-c0b-11-11', "+ended+", 'C0B/11.11', 'pb'), "+
		"('thread-c0c-12-12', "+ended+", 'C0C/12.12', 'pc')",
		fmt.Sprintf("INSERT INTO agents (name, role, state, started_at, thread, pane_id) VALUES "+
			"('thread-c03-3-3', 'thread', 'active', %[1]d, 'C03/3.3', 'p3'), "+
			"('thread-c08-8-8', 'thread', 'starting', %[1]d, 'C08/8.8', 'p8'), "+
			"('thread-c09-9-9', 'thread', 'starting', %[2]d, 'C09/9.9', 'p9'), "+
			"('thread-c0d-13-13', 'thread', 'starting', %[1]d, 'C0D/13.13', 'pd'), "+
			"('thread-c0e-14-14', 'thread', 'starting', %[1]d, 'C0E/14.14', 'pe')", t0-3600, t0-60))
	sightings := func() string {
		return w.query("SELECT group_concat(pane_id || '@' || (first_seen_at - " + fmt.Sprint(t0) + "), ' ') FROM " +
			"(SELECT * FROM orphan_tabs ORDER BY pane_id)")
	}

	out := w.watchAt(0, 0)
	if !strings.Contains(out.stdout, "watch: ended thread-c08-8-8 (thread), starting since") ||
		strings.Contains(out.stdout, "thread-c09-9-9") {
		t.Errorf("first run: %+v", out)
	}
	if got := w.query("SELECT group_concat(name || '=' || state, ' ') FROM agents WHERE state != 'ended'"); got !=
		"thread-c03-3-3=active thread-c09-9-9=starting thread-c0d-13-13=starting thread-c0e-14-14=starting" {
		t.Errorf("live rows after the first run: %s", got)
	}
	if got := sightings(); got != "p1@0 p7@0 p8@0 pb@0" {
		t.Errorf("sightings after the first run: %s", got)
	}
	if strings.Contains(out.stderr, "closed tab") {
		t.Errorf("a tab closed in the first run: %+v", out)
	}

	w.procinfo("p7", busy)
	w.procinfo("pb", other)
	w.watchAt(5*time.Minute, 0)
	if got := sightings(); got != "p1@0 p8@0 pb@300" {
		t.Errorf("sightings after the second run: %s", got)
	}
	out = w.run("", []string{"status", "--scope", scope, "--json"})
	var report struct {
		OrphanTabs []struct {
			Tab         string `json:"tab_id"`
			WatchCloses bool   `json:"watch_closes"`
			Why         string `json:"why"`
		} `json:"orphan_tabs"`
	}
	if err := json.Unmarshal([]byte(out.stdout), &report); out.code != 0 || err != nil {
		t.Fatalf("status: %v %+v", err, out)
	}
	closesAt := "watch closes it at its first run from " + time.Unix(t0+600, 0).UTC().Format("2006-01-02 15:04 UTC")
	var got []string
	// How many children the test process has is not this test's.
	count := regexp.MustCompile(`has [0-9]+ child`)
	for _, o := range report.OrphanTabs {
		got = append(got, fmt.Sprintf("%s %t %s", o.Tab, o.WatchCloses, count.ReplaceAllString(o.Why, "has N child")))
	}
	want := []string{
		"tab-p1 true " + closesAt,
		fmt.Sprintf("tab-p2 false its shell (pid %d) has N child process(es)", busy),
		"tab-p4 false its label is not c04-4-4, the slug of thread C04/4.4, whose ended row records its pane",
		"tab-p5 false no thread agent's ended row records its pane",
		"tab-p6 false an agent runs in its pane",
		fmt.Sprintf("tab-p7 false its shell (pid %d) has N child process(es)", busy),
		"tab-p8 true " + closesAt,
		"tab-pa false it has 2 panes",
		"tab-pb true watch closes it at its first run from " + time.Unix(t0+900, 0).UTC().Format("2006-01-02 15:04 UTC"),
		"tab-pc false a process other than its shell is in its foreground",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("status orphan tabs:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	out = w.run("", []string{"status", "--scope", scope})
	if out.code != 0 || !strings.Contains(out.stdout, "\n\ntabs in the threads workspace that no live agent has:\nTAB ") ||
		!strings.Contains(out.stdout, "tab-p5  c05-5-5    p5    no thread agent's ended row records its pane\n") {
		t.Errorf("status table: %+v", out)
	}

	w.procinfo("p7", idle)
	out = w.watchAt(10*time.Minute, 0)
	for _, want := range []string{
		fmt.Sprintf("watch: closed tab tab-p1 (c01-1-1) of thread C01/1.1: its pane p1 is an ended session's, "+
			"with only its shell (pid %d) in it since %s\n", idle, time.Unix(t0, 0).UTC().Format("2006-01-02 15:04 UTC")),
		"watch: closed tab tab-p8 (c08-8-8) of thread C08/8.8: its pane p8",
	} {
		if !strings.Contains(out.stderr, want) {
			t.Errorf("%q missing from stderr: %+v", want, out)
		}
	}
	if closed, _ := os.ReadFile(filepath.Join(w.dir, "closed")); string(closed) != "p1\np8\n" {
		t.Errorf("closed: %q", closed)
	}
	if got := sightings(); got != "p7@600 pb@300" {
		t.Errorf("sightings after the third run: %s", got)
	}
}

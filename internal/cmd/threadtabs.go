// The tabs of the `threads` workspace that no live agent has (docs/design.md):
// `fleet watch` closes those it may, `fleet status` lists them all.

package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
)

// orphanTab is a tab of a `threads` workspace, other than its `shell` tab,
// none of whose panes a live row records. pane is its one pane, thread the
// thread of the newest ended row that records that pane, and pid the
// pane's shell; why is why watch may not close the tab, empty when it may.
type orphanTab struct {
	tab, label, pane, thread, why string
	pid                           int64
}

// readOrphans reads the orphan tabs of every workspace labelled `threads`,
// in herdr's order. Watch may close one only when all of these hold: its
// one pane is recorded by an ended thread row, its label is that thread's
// slug, herdr sees no agent in the pane, and the shell is the pane's
// foreground alone and has no child process.
func readOrphans(h *herdr.Herdr, conn *sql.DB) ([]orphanTab, error) {
	workspaces, err := WorkspacesLabelled(h, threadsWorkspace)
	if err != nil || len(workspaces) == 0 {
		return nil, err
	}
	live, ended, err := recordedPanes(conn)
	if err != nil {
		return nil, err
	}
	var orphans []orphanTab
	for _, w := range workspaces {
		tabs, err := herdrList(h, "tabs", "tab", "list", "--workspace", w)
		if err != nil {
			return nil, err
		}
		panes, err := herdrList(h, "panes", "pane", "list", "--workspace", w)
		if err != nil {
			return nil, err
		}
		byTab := map[string][]map[string]any{}
		for _, p := range panes {
			tab, _ := p["tab_id"].(string)
			byTab[tab] = append(byTab[tab], p)
		}
		for _, t := range tabs {
			id, _ := t["tab_id"].(string)
			label, _ := t["label"].(string)
			if id == "" || label == shellTab || slices.ContainsFunc(byTab[id], func(p map[string]any) bool {
				pane, _ := p["pane_id"].(string)
				return live[pane]
			}) {
				continue
			}
			o := orphanTab{tab: id, label: label}
			o.check(h, byTab[id], ended)
			orphans = append(orphans, o)
		}
	}
	return orphans, nil
}

// check sets why the tab may not be closed, the first criterion it fails,
// and its pane, thread and shell as far as they are known.
func (o *orphanTab) check(h *herdr.Herdr, panes []map[string]any, ended map[string]string) {
	if len(panes) != 1 {
		o.why = fmt.Sprintf("it has %d panes", len(panes))
		return
	}
	o.pane, _ = panes[0]["pane_id"].(string)
	thread, ok := ended[o.pane]
	o.thread = thread
	agent, _ := panes[0]["agent"].(string)
	switch {
	case !ok:
		o.why = "no thread agent's ended row records its pane"
	case o.label != ThreadSlug(thread):
		o.why = fmt.Sprintf("its label is not %s, the slug of thread %s, whose ended row records its pane",
			ThreadSlug(thread), thread)
	case agent != "":
		o.why = "an agent runs in its pane"
	default:
		o.pid, o.why = shellAlone(h, o.pane)
	}
}

// recordedPanes are the panes live rows record, and those ended thread
// rows record with the thread of the newest such row.
func recordedPanes(conn *sql.DB) (live map[string]bool, ended map[string]string, err error) {
	live, ended = map[string]bool{}, map[string]string{}
	err = eachRow(conn, "SELECT pane_id FROM agents WHERE state != 'ended' AND pane_id != ''", func(rows *sql.Rows) error {
		var pane string
		err := rows.Scan(&pane)
		live[pane] = true
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	err = eachRow(conn, "SELECT pane_id, thread FROM agents WHERE state = 'ended' AND role = 'thread' AND pane_id != '' "+
		"ORDER BY id", func(rows *sql.Rows) error {
		var pane, thread string
		err := rows.Scan(&pane, &thread)
		ended[pane] = thread
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return live, ended, nil
}

// herdrList is the array `key` of a herdr reply, its entries the objects.
func herdrList(h *herdr.Herdr, key string, args ...string) ([]map[string]any, error) {
	result, err := h.CallOK(args...)
	if err != nil {
		return nil, err
	}
	list, ok := herdr.Lookup(result, key).([]any)
	if !ok {
		return nil, exit.Environmentf("herdr %s: no %s array in the reply", strings.Join(args[:2], " "), key)
	}
	var entries []map[string]any
	for _, entry := range list {
		if object, ok := entry.(map[string]any); ok {
			entries = append(entries, object)
		}
	}
	return entries, nil
}

// shellAlone is the pid of the shell of `pane` when nothing else runs in
// it: the shell is its foreground alone and has no child process. Else why
// is what runs there, or why that cannot be told.
func shellAlone(h *herdr.Herdr, pane string) (pid int64, why string) {
	result, err := h.CallOK("pane", "process-info", "--pane", pane)
	if err != nil {
		return 0, fmt.Sprintf("its processes cannot be read: %v", err)
	}
	info, _ := herdr.Lookup(result, "process_info").(map[string]any)
	shell := jsonInt(info["shell_pid"])
	if shell <= 0 {
		return 0, "herdr names no shell in its pane"
	}
	foreground, _ := info["foreground_processes"].([]any)
	var only map[string]any
	if len(foreground) == 1 {
		only, _ = foreground[0].(map[string]any)
	}
	if jsonInt(info["foreground_process_group_id"]) != shell || jsonInt(only["pid"]) != shell {
		return 0, "a process other than its shell is in its foreground"
	}
	children, err := childProcesses(shell)
	if err != nil {
		return 0, fmt.Sprintf("the child processes of its shell (pid %d) cannot be read: %v", shell, err)
	}
	if children > 0 {
		return 0, fmt.Sprintf("its shell (pid %d) has %d child process(es)", shell, children)
	}
	return shell, ""
}

// childProcesses is how many child processes `pid` has, as each of its
// threads' /proc children file lists them.
func childProcesses(pid int64) (int, error) {
	files, err := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/children", pid))
	if err != nil {
		return 0, err
	}
	if len(files) == 0 {
		return 0, fmt.Errorf("no /proc/%d/task/*/children", pid)
	}
	n := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return 0, err
		}
		n += len(strings.Fields(string(data)))
	}
	return n, nil
}

// sighting is a pane's row in `orphan_tabs`.
type sighting struct {
	tab   string
	pid   int64
	since int64
}

// sightings are the rows of `orphan_tabs`, by pane.
func sightings(conn *sql.DB) (map[string]sighting, error) {
	seen := map[string]sighting{}
	err := eachRow(conn, "SELECT pane_id, tab_id, shell_pid, first_seen_at FROM orphan_tabs", func(rows *sql.Rows) error {
		var pane string
		var s sighting
		err := rows.Scan(&pane, &s.tab, &s.pid, &s.since)
		seen[pane] = s
		return err
	})
	return seen, err
}

// same is whether the sighting is of the orphan as it is now: the same tab
// and the same shell.
func (s sighting) same(o orphanTab) bool {
	return s.tab == o.tab && s.pid == o.pid
}

// threadTabs squares the `threads` workspace with the ledger. A thread
// agent's row still `starting` startGraceSecs after its start, with no
// agent in herdr, is ended: a start that failed and was not tried again.
// Then each orphan tab watch may close is closed once two runs goneSecs
// apart or more found it so, with every run between them that read it;
// the others are left for `fleet status` to list. A failure is the run's.
func (r *watchRun) threadTabs(inHerdr map[string]InHerdr) {
	panes, err := herdrPanes(r.h)
	if err != nil {
		r.failed(err)
		return
	}
	if err := r.endStartingThreads(inHerdr, panes); err != nil {
		r.failed(err)
		return
	}
	orphans, err := readOrphans(r.h, r.conn)
	if err != nil {
		r.failed(fmt.Errorf("reading the tabs of the threads workspace: %w", err))
		return
	}
	if err := r.closeOrphans(orphans); err != nil {
		r.failed(err)
	}
}

// endStartingThreads ends each thread agent's row still `starting`
// startGraceSecs after its start, missing from `herdr agent list` with no
// agent in the pane it records.
func (r *watchRun) endStartingThreads(inHerdr map[string]InHerdr, panes map[string]herdrPane) error {
	type starting struct {
		id         int64
		name, pane string
		startedAt  int64
	}
	var all []starting
	err := eachRow(r.conn, "SELECT id, name, pane_id, started_at FROM agents WHERE role = 'thread' AND state = 'starting' "+
		"AND started_at <= ?1 ORDER BY id", func(rows *sql.Rows) error {
		var s starting
		err := rows.Scan(&s.id, &s.name, &s.pane, &s.startedAt)
		all = append(all, s)
		return err
	}, r.now-startGraceSecs)
	if err != nil {
		return err
	}
	for _, s := range all {
		if _, ok := inHerdr[s.name]; ok {
			continue
		}
		if p, ok := panes[s.pane]; ok && p.agent {
			continue
		}
		if _, err := r.conn.Exec("UPDATE agents SET state = 'ended', ended_at = ?1 WHERE id = ?2 AND state = 'starting'",
			r.now, s.id); err != nil {
			return exit.Database(err)
		}
		fmt.Fprintf(os.Stdout, "watch: ended %s (thread), starting since %s with no agent in herdr\n", s.name, at(s.startedAt))
	}
	return nil
}

// closeOrphans closes the orphan tabs watch may close that a run goneSecs
// ago or more already found so, records those found so for the first
// time, and forgets every other pane.
func (r *watchRun) closeOrphans(orphans []orphanTab) error {
	seen, err := sightings(r.conn)
	if err != nil {
		return err
	}
	kept := map[string]bool{}
	for _, o := range orphans {
		if o.why != "" {
			continue
		}
		kept[o.pane] = true
		s, ok := seen[o.pane]
		if !ok || !s.same(o) {
			if _, err := r.conn.Exec("INSERT OR REPLACE INTO orphan_tabs (pane_id, tab_id, shell_pid, first_seen_at) "+
				"VALUES (?1, ?2, ?3, ?4)", o.pane, o.tab, o.pid, r.now); err != nil {
				return exit.Database(err)
			}
			continue
		}
		if r.now-s.since < goneSecs || r.outOfTime() {
			continue
		}
		if err := closeTab(r.h, o.tab, "thread "+o.thread); err != nil {
			r.failed(fmt.Errorf("closing tab %s (%s) of thread %s: %w", o.tab, o.label, o.thread, err))
			continue
		}
		delete(kept, o.pane)
		fmt.Fprintf(os.Stderr, "watch: closed tab %s (%s) of thread %s: its pane %s is an ended session's, with only "+
			"its shell (pid %d) in it since %s\n", o.tab, o.label, o.thread, o.pane, o.pid, at(s.since))
	}
	for pane := range seen {
		if kept[pane] {
			continue
		}
		if _, err := r.conn.Exec("DELETE FROM orphan_tabs WHERE pane_id = ?1", pane); err != nil {
			return exit.Database(err)
		}
	}
	return nil
}

// orphanLine is one orphan tab as `status` shows it: watchCloses is
// whether watch closes it, and why says when, or why it does not.
type orphanLine struct {
	Tab         string `json:"tab_id"`
	Label       string `json:"label"`
	Pane        string `json:"pane_id"`
	WatchCloses bool   `json:"watch_closes"`
	Why         string `json:"why"`
}

// orphanLines are the orphan tabs as `status` shows them.
func orphanLines(h *herdr.Herdr, conn *sql.DB) ([]orphanLine, error) {
	orphans, err := readOrphans(h, conn)
	if err != nil {
		return nil, err
	}
	seen, err := sightings(conn)
	if err != nil {
		return nil, err
	}
	lines := []orphanLine{}
	for _, o := range orphans {
		l := orphanLine{Tab: o.tab, Label: o.label, Pane: o.pane, WatchCloses: o.why == "", Why: o.why}
		if s, ok := seen[o.pane]; l.WatchCloses && ok && s.same(o) {
			l.Why = "watch closes it at its first run from " + at(s.since+goneSecs)
		} else if l.WatchCloses {
			l.Why = fmt.Sprintf("watch closes it once two runs %s apart find it so", Duration(goneSecs))
		}
		lines = append(lines, l)
	}
	return lines, nil
}

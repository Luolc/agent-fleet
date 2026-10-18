// `fleet watch` squaring the ledger with herdr (docs/design.md): an active
// lead or job worker gone from herdr for goneSecs has its row ended, and
// the tab of an ended lead's or worker's pane herdr still has is closed.

package cmd

import (
	"database/sql"
	"fmt"
	"os"

	"github.com/Luolc/agent-fleet/internal/atb"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
)

const (
	// goneStatus is what watch records as the herdr status of an active
	// lead or worker gone from herdr, so the time it went is kept in
	// last_change_at.
	goneStatus = "gone"
	// goneSecs is how long an active lead or worker is gone from herdr
	// before watch ends its row: at least two runs of a 5-minute timer
	// after the one that first missed it, so one `herdr agent list` that
	// missed an agent, or a herdr restart, ends nothing.
	goneSecs int64 = 10 * 60
)

// herdrPane is one pane of `herdr pane list`.
type herdrPane struct {
	label, tab string
}

// herdrPanes is every pane herdr has in this session, by pane id.
func herdrPanes(h *herdr.Herdr) (map[string]herdrPane, error) {
	result, err := h.CallOK("pane", "list")
	if err != nil {
		return nil, err
	}
	list, ok := herdr.Lookup(result, "panes").([]any)
	if !ok {
		return nil, exit.Environmentf("herdr pane list: no panes array in the reply")
	}
	panes := map[string]herdrPane{}
	for _, entry := range list {
		pane, _ := entry.(map[string]any)
		id, _ := pane["pane_id"].(string)
		label, _ := pane["label"].(string)
		tab, _ := pane["tab_id"].(string)
		if id != "" && tab != "" {
			panes[id] = herdrPane{label: label, tab: tab}
		}
	}
	return panes, nil
}

// endGone ends the row of each active lead and job worker gone from herdr
// for goneSecs, and returns the others; one it could not end stays, its
// failure the run's. A worker's work order gets a line saying it is gone
// and is released as abandoned first, each step recorded, as `worker end`
// does, and its lead, when in herdr, is told. A lead's job stays open: the
// agent of its home thread is told, once, since the row it is told about
// is ended, and decides whether to end the job.
func (r *watchRun) endGone(all []watched) []watched {
	var kept []watched
	for _, w := range all {
		if w.goneSince == nil || r.now-*w.goneSince < goneSecs || r.outOfTime() {
			kept = append(kept, w)
			continue
		}
		var err error
		if w.live.Role == "worker" {
			err = r.endGoneWorker(w, all)
		} else {
			err = r.endGoneLead(w)
		}
		if err != nil {
			r.failed(fmt.Errorf("ending %s, gone from herdr: %w", w.live.Name, err))
			kept = append(kept, w)
		}
	}
	return kept
}

// goneFor says how long `w` has been gone from herdr.
func (r *watchRun) goneFor(w watched) string {
	return fmt.Sprintf("gone from herdr for %s (since %s)", Duration(r.now-*w.goneSince), at(*w.goneSince))
}

// endGoneRow ends the row of `w`.
func (r *watchRun) endGoneRow(w watched) error {
	_, err := r.conn.Exec("UPDATE agents SET state = 'ended', ended_at = ?1 WHERE id = ?2 AND state != 'ended'",
		r.now, w.live.ID)
	if err != nil {
		return exit.Database(err)
	}
	fmt.Fprintf(os.Stdout, "watch: ended %s (%s, job %s): %s\n", w.live.Name, w.live.Role, w.live.Job, r.goneFor(w))
	return nil
}

func (r *watchRun) endGoneWorker(w watched, all []watched) error {
	name := w.live.Name
	var issue string
	if err := r.conn.QueryRow("SELECT issue FROM agents WHERE id = ?1", w.live.ID).Scan(&issue); err != nil {
		return exit.Database(err)
	}
	if issue != "" {
		key := fmt.Sprintf("watch-gone:%d", w.live.ID)
		if err := runStep(r.conn, key, "comment-work-order", func() error {
			return commentText(issue, fmt.Sprintf("%s was ended by fleet watch, without a report: %s. Its work order "+
				"is released as abandoned.\n", name, r.goneFor(w)))
		}); err != nil {
			return err
		}
		if err := runStep(r.conn, key, "release-work-order", func() error {
			return atb.Release(issue, name, true)
		}); err != nil {
			return err
		}
	}
	if err := r.endGoneRow(w); err != nil {
		return err
	}
	if !inHerdr(all, w.live.Parent) {
		return nil
	}
	text := fmt.Sprintf("fleet watch, rule `worker gone`: your worker %s is %s, so watch ended its row; do not "+
		"wait for its `fleet done`.", name, r.goneFor(w))
	if issue != "" {
		text += fmt.Sprintf(" Its work order %s is released as abandoned.", issue)
	}
	text += " Its worktree and branch are left as they are.\n"
	if err := r.send(w.live.Parent, text); err != nil {
		r.failed(err)
	}
	return nil
}

func (r *watchRun) endGoneLead(w watched) error {
	job, err := openJob(r.conn, w.live.Job)
	if err != nil {
		return err
	}
	if err := r.endGoneRow(w); err != nil {
		return err
	}
	thread := ""
	if job != nil {
		thread = job.HomeThread
	}
	text := fmt.Sprintf("fleet watch, rule `lead gone`: %s, the lead of job %s, is %s, so watch ended its row. The job "+
		"is still open and watch does not end it, nor tell you again. %s; at the least, see what is left with "+
		"`fleet status --job %s`, then decide whether to end the job with `fleet job end %s --force`.\n",
		w.live.Name, w.live.Job, r.goneFor(w), seePrompt, w.live.Job, w.live.Job)
	if thread == "" || r.cfg.FednetSocket == "" {
		fmt.Fprintf(os.Stderr, "fleet watch: no thread to tell that %s is gone: %s", w.live.Name, text)
		return nil
	}
	code, err := toThread(r.h, r.conn, r.scope, r.cfg, inboundMessage{Thread: thread, Text: text, Watch: true})
	if err != nil {
		r.failed(err)
		return nil
	}
	r.got(code)
	return nil
}

// inHerdr is whether `all` has the agent `name` in herdr this run.
func inHerdr(all []watched, name string) bool {
	for _, w := range all {
		if w.live.Name == name && w.reading != nil {
			return true
		}
	}
	return false
}

// closeEnded closes the tab of each ended lead's or worker's pane that
// herdr still has under the agent's name, as `done`, a crash or an older
// fleet may leave it; a pane a live row records is never closed. A
// failure is the run's, and the next run tries again.
func (r *watchRun) closeEnded(panes map[string]herdrPane) error {
	type ended struct{ name, pane string }
	var all []ended
	err := eachRow(r.conn, "SELECT DISTINCT a.name, a.pane_id FROM agents a WHERE a.state = 'ended' AND "+
		"a.role IN ('lead', 'worker') AND a.pane_id != '' AND NOT EXISTS (SELECT 1 FROM agents l WHERE "+
		"l.state != 'ended' AND l.pane_id = a.pane_id) ORDER BY a.pane_id", func(rows *sql.Rows) error {
		var e ended
		if err := rows.Scan(&e.name, &e.pane); err != nil {
			return err
		}
		all = append(all, e)
		return nil
	})
	if err != nil {
		return err
	}
	for _, e := range all {
		if p, ok := panes[e.pane]; !ok || p.label != e.name {
			continue
		}
		if r.outOfTime() {
			return nil
		}
		if err := closePaneTab(r.h, e.pane, e.name); err != nil {
			r.failed(fmt.Errorf("closing the tab of %s, whose row is ended: %w", e.name, err))
			continue
		}
		fmt.Fprintf(os.Stdout, "watch: closed the tab of %s, whose row is ended (pane %s)\n", e.name, e.pane)
	}
	return nil
}

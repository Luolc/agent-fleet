// `fleet worker end`: a worker that cannot run `done` itself is ended by
// its lead, or from outside.

package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Luolc/agent-fleet/internal/atb"
	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
	"github.com/Luolc/agent-fleet/internal/identity"
)

// WorkerAbout, WorkerEndAbout and WorkerEndLongAbout are the help texts of
// `worker` and `worker end`.
const (
	WorkerAbout        = "Act on a worker of a job"
	WorkerEndAbout     = "End a worker that cannot end itself: its lead, a thread agent or a shell"
	WorkerEndLongAbout = "End a worker that cannot end itself: its lead, a thread agent or a shell.\n\n" +
		"For a worker stuck where it cannot run `fleet done` (an error it cannot get past, a " +
		"pane that never started). The caller is the lead of the worker's job, a thread agent, " +
		"or a shell with no FLEET_ROLE; a worker, a lead of another job, a row that is not a " +
		"worker and a worker `watch` started (no job) are refused.\n\n" +
		"First the checks: no agent but the worker may have its cwd inside the worker's " +
		"worktree (the job's recorded worktree that holds the cwd the ledger records for the " +
		"worker), and the worktree must have nothing `job end` would refuse to lose " +
		"(uncommitted changes, its submodules' included); what is in the way is listed and " +
		"`worker end` exits 5 having changed nothing. Then, when the worker has a work order: " +
		"the report (--report-file, else a fixed line naming the caller) is written to it " +
		"(`atb linear comment`) and it is released as abandoned (`atb linear release`), each " +
		"step recorded in the ledger so a retry after a failure continues where it stopped. " +
		"Then the tab of the pane the ledger records for the worker is closed (gone already is fine), its worktree removed " +
		"(its branch kept, with any commits not pushed, for the lead to hand on or delete), and its row ended, so `job end` is no longer refused for it. The " +
		"job's other worktrees are left alone.\n\n" +
		"When the caller is not the worker's parent (a thread agent or a shell ending a lead's " +
		"worker), the parent is told with a message headed `[FROM: <caller>]` (`a shell` without " +
		"FLEET_AGENT): who ended which worker, the work order released as abandoned, the branches " +
		"kept and the report file. Telling it is best effort: the worker is ended by then, so a " +
		"parent that is gone, blocked, paused on Claude Code's `Session paused` dialog (nothing is " +
		"typed into it, as for `fleet send`'s exit 8) or gives no clear signal leaves a warning on stderr and " +
		"the exit code unchanged.\n\n" +
		"With --issue <ISSUE>, for a worker whose row ended without releasing its work order " +
		"(a `spawn` that stopped after atb did not confirm the claim): only the newest ended row " +
		"of a worker with the name and that work order is taken, refused when its release is " +
		"recorded already, and the work order is released as abandoned in the worker's name " +
		"(`atb linear release --reason <who and why> --abandon`, one comment, recorded so it is " +
		"not done twice); nothing else is done, nobody is told. atb refuses, writing nothing, " +
		"when the worker holds nothing there (another agent's claim, or none): exit 5 after its " +
		"exit 4. Not with --report-file. The caller is checked as above, so while the job is open its " +
		"lead or a thread agent runs it, and once the job has ended a thread agent or a shell on " +
		"the machine whose ledger has the row.\n\n" +
		"Exit: 0 when the row is ended (with --issue: the work order released); 1 when a check " +
		"refuses (the caller, the row, the report file, a release recorded already); 4 when no " +
		"live row has the name (with --issue: no ended row with the name and the work order); 5 " +
		"when atb, git, herdr or the database fails, or something is in the way (listed)."
)

// WorkerEndArgs are the arguments of `worker end`.
type WorkerEndArgs struct {
	// Name is the worker's full name, as `fleet status` shows it.
	Name string
	// ReportFile is written to the worker's work order; nil writes a
	// fixed line.
	ReportFile *string
	// Issue, when set, releases only this work order of an ended row
	// (releaseLeftOrder).
	Issue *string
}

// workerRow is the live ledger row `worker end` ends.
type workerRow struct {
	id                                  int64
	role, job, parent, cwd, issue, pane string
}

// liveWorker is the live row named `name`, or nil.
func liveWorker(conn *sql.DB, name string) (*workerRow, error) {
	var r workerRow
	err := conn.QueryRow("SELECT id, role, job, parent, cwd, issue, pane_id FROM agents WHERE name = ?1 AND state != 'ended'",
		name).Scan(&r.id, &r.role, &r.job, &r.parent, &r.cwd, &r.issue, &r.pane)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, exit.Database(err)
	}
	return &r, nil
}

// workerEndCaller refuses a worker; returns the job a lead may end
// workers of, "" for a thread agent or a caller without FLEET_ROLE.
func workerEndCaller() (string, error) {
	if os.Getenv("FLEET_ROLE") == "" {
		return "", nil
	}
	me, err := identity.FromEnv()
	if err != nil {
		return "", err
	}
	switch me.Role {
	case identity.Thread:
		return "", nil
	case identity.Lead:
		if me.Job == "" {
			return "", exit.Refusedf("FLEET_JOB is not set")
		}
		return me.Job, nil
	default:
		return "", exit.Refusedf("a %s cannot end an agent; it ends itself with `fleet done`", me.Role)
	}
}

// WorkerEnd runs `worker end`.
func WorkerEnd(h *herdr.Herdr, args WorkerEndArgs) (exit.Code, error) {
	if err := CheckName(args.Name); err != nil {
		return 0, err
	}
	leadJob, err := workerEndCaller()
	if err != nil {
		return 0, err
	}
	if args.Issue != nil {
		return releaseLeftOrder(args.Name, *args.Issue, leadJob, args.ReportFile != nil)
	}
	var report string
	if args.ReportFile != nil {
		report, err = canonicalize(*args.ReportFile)
		if err == nil {
			_, err = os.ReadFile(report)
		}
		if err != nil {
			return 0, exit.Refusedf("cannot read %s: %v", *args.ReportFile, err)
		}
	}
	conn, err := OpenLedger()
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	row, err := workerToEnd(conn, args.Name, leadJob)
	if err != nil {
		return 0, err
	}
	dirs, err := workerDirs(conn, row)
	if err != nil {
		return 0, err
	}
	blocking, err := inTheWay(h, nil, dirs, args.Name)
	if err != nil {
		return 0, err
	}
	if len(blocking) > 0 {
		return 0, exit.Environmentf("not ending %s: in the way: %s", args.Name, strings.Join(blocking, ", "))
	}
	if err := releaseWorkOrder(conn, row, args.Name, report); err != nil {
		return 0, err
	}
	// The tab of the pane the row recorded, whether or not herdr lists an
	// agent there: a start that stopped early leaves a tab with none.
	if row.pane != "" {
		if err := closeOwnTab(h, conn, args.Name); err != nil {
			return 0, err
		}
	}
	branches, err := keptBranches(conn, dirs)
	if err != nil {
		return 0, err
	}
	if err := removeDirs(h, nil, dirs, args.Name); err != nil {
		return 0, err
	}
	if err := endWorkerRow(conn, row, dirs); err != nil {
		return 0, err
	}
	fmt.Fprintf(os.Stdout, "ended worker %s of job %s\n", args.Name, row.job)
	tellParent(h, row, args.Name, report, branches)
	return exit.Ok, nil
}

// keptBranches are the branches the ledger records for `dirs`.
func keptBranches(conn *sql.DB, dirs []jobDir) ([]string, error) {
	var branches []string
	for _, d := range dirs {
		var branch string
		if err := conn.QueryRow("SELECT branch FROM worktrees WHERE path = ?1 AND removed_at IS NULL", d.path).
			Scan(&branch); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, exit.Database(err)
		}
		if branch != "" {
			branches = append(branches, branch)
		}
	}
	return branches, nil
}

// tellParent tells the worker's parent who ended it, unless the parent is
// the caller; a failure is only a warning, since the worker is ended
// already.
func tellParent(h *herdr.Herdr, row *workerRow, name, report string, branches []string) {
	if row.parent == "" || row.parent == os.Getenv("FLEET_AGENT") {
		return
	}
	var body strings.Builder
	fmt.Fprintf(&body, "%s ended your worker %s with `fleet worker end`; do not wait for its `fleet done`.\n", callerName(), name)
	if row.issue != "" {
		fmt.Fprintf(&body, "Its work order %s is released as abandoned.\n", row.issue)
	}
	for _, b := range branches {
		fmt.Fprintf(&body, "Its branch %s is kept.\n", b)
	}
	if report != "" {
		fmt.Fprintf(&body, "Report: %s\n", report)
	}
	// The body is never empty and starts with the caller's name, so the
	// header cannot fail.
	text, _ := WithHeader(callerName(), body.String())
	if agent := herdrAgent(h, row.parent); agent != nil && pausedOnDialog(h, row.parent, agent) {
		fmt.Fprintf(os.Stderr, "warning: %s is ended, but its parent %s was not told: it is paused on Claude Code's "+
			"`Session paused` dialog, so nothing was typed (press no key there)\n", name, row.parent)
		return
	}
	outcome, err := h.Prompt(row.parent, text)
	var reason string
	switch {
	case err != nil:
		reason = err.Error()
	case outcome.Kind == herdr.Prompted:
		fmt.Fprintf(os.Stdout, "told %s\n", row.parent)
		return
	case outcome.Kind == herdr.Blocked:
		reason = "it is blocked by an interactive prompt"
	case outcome.Kind == herdr.NotFound:
		reason = "it is not found"
	default:
		reason = "no clear signal: " + outcome.Reason
	}
	fmt.Fprintf(os.Stderr, "warning: %s is ended, but its parent %s was not told: %s\n", name, row.parent, reason)
}

// endWorkerRow marks `dirs` removed and the worker's row ended.
func endWorkerRow(conn *sql.DB, row *workerRow, dirs []jobDir) error {
	now := db.Now()
	for _, d := range dirs {
		if _, err := conn.Exec("UPDATE worktrees SET removed_at = ?1 WHERE path = ?2 AND removed_at IS NULL",
			now, d.path); err != nil {
			return exit.Database(err)
		}
	}
	if _, err := conn.Exec("UPDATE agents SET state = 'ended', ended_at = ?1 WHERE id = ?2", now, row.id); err != nil {
		return exit.Database(err)
	}
	return nil
}

// releaseLeftOrder is `worker end --issue`: the work order `issue` of the
// newest ended row of the worker `name` (of `leadJob` when set) is
// released as abandoned in the worker's name, recorded under the row's
// `worker-end:<id>`; refused when that row's release is recorded there or
// under `watch-gone:<id>`.
func releaseLeftOrder(name, issue, leadJob string, report bool) (exit.Code, error) {
	if report {
		return 0, exit.Refusedf("--report-file does not go with --issue: the release names who released the work order and why")
	}
	if err := atb.CheckIdentifier(issue); err != nil {
		return 0, err
	}
	conn, err := OpenLedger()
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	var row workerRow
	err = conn.QueryRow("SELECT id, role, job FROM agents WHERE name = ?1 AND issue = ?2 AND state = 'ended' "+
		"ORDER BY id DESC LIMIT 1", name, issue).Scan(&row.id, &row.role, &row.job)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, exit.New(exit.NotFound, fmt.Sprintf("no ended agent %s with the work order %s in the ledger", name, issue))
	case err != nil:
		return 0, exit.Database(err)
	case row.role != "worker" || row.job == "":
		return 0, exit.Refusedf("%s is not a worker of a job; `worker end --issue` releases only those work orders", name)
	case leadJob != "" && row.job != leadJob:
		return 0, exit.Refusedf("%s is in job %s, not yours (%s); a lead ends only its own workers", name, row.job, leadJob)
	}
	key := fmt.Sprintf("worker-end:%d", row.id)
	for _, k := range []string{key, fmt.Sprintf("watch-gone:%d", row.id)} {
		if stepDone(conn, k, "release-work-order") {
			return 0, exit.Refusedf("the release of %s by %s is recorded already (%s)", issue, name, k)
		}
	}
	reason := fmt.Sprintf("released by %s with `fleet worker end --issue`: the row of %s ended without releasing it",
		callerName(), name)
	if err := runStep(conn, key, "release-work-order", func() error { return atb.Abandon(issue, name, reason) }); err != nil {
		return 0, err
	}
	fmt.Fprintf(os.Stdout, "released the work order %s of %s (job %s) as abandoned\n", issue, name, row.job)
	return exit.Ok, nil
}

// workerToEnd is the live row named `name`, refused unless it is a worker
// of a job, and of `leadJob` when that is set.
func workerToEnd(conn *sql.DB, name, leadJob string) (*workerRow, error) {
	row, err := liveWorker(conn, name)
	switch {
	case err != nil:
		return nil, err
	case row == nil:
		return nil, exit.New(exit.NotFound, fmt.Sprintf("no live agent %s in the ledger", name))
	case row.role != "worker":
		return nil, exit.Refusedf("%s is a %s; `worker end` ends only workers", name, row.role)
	case row.job == "":
		return nil, exit.Refusedf("%s has no job: `watch` started it and closes its tab on its next run", name)
	case leadJob != "" && row.job != leadJob:
		return nil, exit.Refusedf("%s is in job %s, not yours (%s); a lead ends only its own workers", name, row.job, leadJob)
	}
	return row, nil
}

// workerDirs are the job's recorded worktrees that hold the worker's cwd,
// their branches kept.
func workerDirs(conn *sql.DB, row *workerRow) ([]jobDir, error) {
	all, err := jobWorktrees(conn, row.job)
	if err != nil {
		return nil, err
	}
	var dirs []jobDir
	for _, d := range all {
		if row.cwd != "" && pathStartsWith(row.cwd, d.path) {
			d.keepBranch = true
			dirs = append(dirs, d)
		}
	}
	return dirs, nil
}

// releaseWorkOrder writes the report, or a fixed line without one, to the
// worker's work order and releases it as abandoned, each step recorded;
// nothing without a work order.
func releaseWorkOrder(conn *sql.DB, row *workerRow, name, report string) error {
	if row.issue == "" {
		return nil
	}
	key := fmt.Sprintf("worker-end:%d", row.id)
	if err := runStep(conn, key, "comment-work-order", func() error {
		if report != "" {
			return atb.Comment(row.issue, report)
		}
		return commentText(row.issue, fmt.Sprintf("%s was ended by %s with `fleet worker end`, without a report.\n",
			name, callerName()))
	}); err != nil {
		return err
	}
	return runStep(conn, key, "release-work-order", func() error {
		return atb.Release(row.issue, name, true)
	})
}

// callerName is FLEET_AGENT, or `a shell` without it.
func callerName() string {
	if name := os.Getenv("FLEET_AGENT"); name != "" {
		return name
	}
	return "a shell"
}

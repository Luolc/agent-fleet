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
		"Exit: 0 when the row is ended; 1 when a check refuses (the caller, the row, the report " +
		"file); 4 when no live row has the name; 5 when atb, git, herdr or the database fails, " +
		"or something is in the way (listed)."
)

// WorkerEndArgs are the arguments of `worker end`.
type WorkerEndArgs struct {
	// Name is the worker's full name, as `fleet status` shows it.
	Name string
	// ReportFile is written to the worker's work order; nil writes a
	// fixed line.
	ReportFile *string
}

// workerRow is the live ledger row `worker end` ends.
type workerRow struct {
	id                          int64
	role, job, cwd, issue, pane string
}

// liveWorker is the live row named `name`, or nil.
func liveWorker(conn *sql.DB, name string) (*workerRow, error) {
	var r workerRow
	err := conn.QueryRow("SELECT id, role, job, cwd, issue, pane_id FROM agents WHERE name = ?1 AND state != 'ended'",
		name).Scan(&r.id, &r.role, &r.job, &r.cwd, &r.issue, &r.pane)
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
	if err := removeDirs(h, nil, dirs, args.Name); err != nil {
		return 0, err
	}
	if err := endWorkerRow(conn, row, dirs); err != nil {
		return 0, err
	}
	fmt.Fprintf(os.Stdout, "ended worker %s of job %s\n", args.Name, row.job)
	return exit.Ok, nil
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

// `fleet job end`: the lead ends its job; `--force` reclaims a job from
// outside.

package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Luolc/agent-fleet/internal/atb"
	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
	"github.com/Luolc/agent-fleet/internal/identity"
)

// JobEndAbout and JobEndLongAbout are the help texts of `job end`.
const (
	JobEndAbout     = "End your job with a report (lead only), or reclaim a job from outside with --force"
	JobEndLongAbout = "End your job with a report (lead only), or reclaim a job from outside with --force.\n\n" +
		"Called by the job's lead from its own pane when the job is done, or abandoned with " +
		"--abandon. Refused while the job has live workers (named: wait for their `done`). " +
		"The report file is required.\n\n" +
		"With Linear on, first: the report is written to your work order (FLEET_ISSUE, `atb " +
		"linear comment`) and the work order is released as done or abandoned (`atb linear " +
		"release`); then the conclusion (outcome, lead, work order, report path) is written to " +
		"the job's parent issue and the parent is released the same way. If an atb step fails, " +
		"nothing else changes and `job end` exits 5 naming the step; the comment may already be " +
		"written.\n\n" +
		"Then the cleanup: every other agent in the job's workspace is closed; no agent but you " +
		"may have its cwd inside one of the job's directories (the worktrees `fleet worktree` " +
		"recorded for it and, for a cross-repo job, ~/cross-repo/<job>/); each worktree is " +
		"removed with `git worktree remove` (which refuses uncommitted changes) and its local " +
		"branch deleted, and the cross-repo directory is removed. When something is left it is " +
		"listed and `job end` exits 5 without ending the job. Otherwise the job is marked ended " +
		"with its outcome, your row ended, the worktrees removed, the conclusion is printed " +
		"(the line the job's home thread will receive once threads exist), and last the " +
		"workspace is closed, which ends your own pane.\n\n" +
		"`job end <JOB> --force` reclaims a job from outside: a thread agent, or a shell with " +
		"no FLEET_ROLE. No report, no Linear step. It closes the job's workspace, removes the " +
		"job's directories with the same checks (no agent inside, no uncommitted changes), and " +
		"ends every live row of the job; an open job is marked ended with the outcome " +
		"`abandoned`. Parts already gone are skipped, so it can be run again.\n\n" +
		"Exit: 0 when the job ended; 1 when a check refuses (live workers, the caller, the " +
		"report file, the flags); 5 when atb, git, herdr or the database fails, the target has " +
		"no ledger, or something is left (listed)."
)

// JobEndArgs are the arguments of `job end`.
type JobEndArgs struct {
	// Job is the job id; required with --force, else it must be the
	// caller's own job when given.
	Job *string
	// ReportFile is the lead's report; required without --force.
	ReportFile *string
	// Abandon ends the job as abandoned instead of done.
	Abandon bool
	// Force reclaims the job from outside.
	Force bool
}

func inside(agent map[string]any, dir string) bool {
	for _, key := range []string{"cwd", "foreground_cwd"} {
		if cwd, ok := agent[key].(string); ok && pathStartsWith(cwd, dir) {
			return true
		}
	}
	return false
}

// pathStartsWith is whether `base`'s components are a prefix of `path`'s,
// so `/w/item-10` does not start with `/w/item-1`.
func pathStartsWith(path, base string) bool {
	p, b := components(path), components(base)
	if len(b) > len(p) {
		return false
	}
	for i := range b {
		if p[i] != b[i] {
			return false
		}
	}
	return true
}

// components are a Unix path's components: the root as "/", repeated
// separators and `.` after the start dropped, `..` kept.
func components(path string) []string {
	var parts []string
	if strings.HasPrefix(path, "/") {
		parts = append(parts, "/")
	}
	for i, part := range strings.Split(path, "/") {
		if part == "" || part == "." && (i > 0 || len(parts) > 0) {
			continue
		}
		parts = append(parts, part)
	}
	return parts
}

func describe(agent map[string]any) string {
	field := func(key string) string {
		if value, ok := agent[key].(string); ok {
			return value
		}
		return "?"
	}
	return fmt.Sprintf("agent %s (pane %s, cwd %s)", field("name"), field("pane_id"), field("cwd"))
}

// jobDir is a directory `job end` removes: a worktree with the checkout it
// belongs to, or the cross-repo directory (no checkout).
type jobDir struct{ path, checkout string }

// agentsLeft are the agents in one of `workspaces` or with their cwd inside
// one of `dirs`, except the one named `except` (the lead ending its own
// job).
func agentsLeft(h *herdr.Herdr, workspaces []string, dirs []jobDir, except string) ([]string, error) {
	agents, err := HerdrAgentList(h)
	if err != nil {
		return nil, err
	}
	left := []string{}
	for _, entry := range agents {
		agent, _ := entry.(map[string]any)
		if name, _ := agent["name"].(string); except != "" && name == except {
			continue
		}
		id, ok := agent["workspace_id"].(string)
		if ok && slices.Contains(workspaces, id) || slices.ContainsFunc(dirs, func(d jobDir) bool {
			return inside(agent, d.path)
		}) {
			left = append(left, describe(agent))
		}
	}
	return left, nil
}

// jobDirs are the directories to remove for `job`: the worktrees the ledger
// records for it, then the cross-repo directory of a cross-repo job.
func jobDirs(conn *sql.DB, job *jobRow, name, home string) ([]jobDir, error) {
	dirs, err := jobWorktrees(conn, name, home)
	if err != nil {
		return nil, err
	}
	if job != nil && job.Repo == "" && job.LeadCwd == filepath.Join(home, "cross-repo", name) {
		dirs = append(dirs, jobDir{path: job.LeadCwd})
	}
	return dirs, nil
}

// jobWorktrees are the worktrees the ledger records for `job` and not yet
// removed, oldest first.
func jobWorktrees(conn *sql.DB, job, home string) ([]jobDir, error) {
	rows, err := conn.Query(
		"SELECT path, repo FROM worktrees WHERE job = ?1 AND removed_at IS NULL ORDER BY id", job)
	if err != nil {
		return nil, exit.Database(err)
	}
	defer rows.Close()
	var found []jobDir
	for rows.Next() {
		var path, repo string
		if err := rows.Scan(&path, &repo); err != nil {
			return nil, exit.Database(err)
		}
		found = append(found, jobDir{path, filepath.Join(home, "dev", repo)})
	}
	if err := rows.Err(); err != nil {
		return nil, exit.Database(err)
	}
	return found, nil
}

// liveRowsOf are the names of the live rows of `job` with `role`, or of
// every role when role is empty.
func liveRowsOf(conn *sql.DB, job, role string) ([]string, error) {
	rows, err := conn.Query("SELECT name FROM agents WHERE job = ?1 AND state != 'ended' AND (?2 = '' OR role = ?2) ORDER BY id",
		job, role)
	if err != nil {
		return nil, exit.Database(err)
	}
	defer rows.Close()
	live := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, exit.Database(err)
		}
		live = append(live, name)
	}
	if err := rows.Err(); err != nil {
		return nil, exit.Database(err)
	}
	return live, nil
}

// removeWorktree removes `worktree` from `checkout` and deletes the branch
// it had checked out, if any.
func removeWorktree(checkout, worktree string) error {
	var branch string
	if out, err := Git("-C", worktree, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		branch = strings.TrimSpace(out)
	}
	if _, err := Git("-C", checkout, "worktree", "remove", worktree); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "removed worktree %s\n", worktree)
	if branch != "" {
		// -D: after a squash merge the branch is not an ancestor of main.
		if _, err := Git("-C", checkout, "branch", "-D", branch); err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "deleted branch %s\n", branch)
	}
	return nil
}

// removeDirs refuses while an agent other than `except` is in one of
// `workspaces` or `dirs`, then removes each directory that still exists.
func removeDirs(h *herdr.Herdr, workspaces []string, dirs []jobDir, except string) error {
	blocking, err := agentsLeft(h, workspaces, dirs, except)
	if err != nil {
		return err
	}
	if len(blocking) > 0 {
		paths := make([]string, len(dirs))
		for i, d := range dirs {
			paths[i] = d.path
		}
		what := "not removing " + strings.Join(paths, ", ")
		if len(paths) == 0 {
			what = "not ending the job"
		}
		return exit.Environmentf("%s: still in use by %s", what, strings.Join(blocking, ", "))
	}
	for _, d := range dirs {
		if !exists(d.path) {
			continue
		}
		if d.checkout != "" {
			if err := removeWorktree(d.checkout, d.path); err != nil {
				return err
			}
			continue
		}
		if err := os.RemoveAll(d.path); err != nil {
			return exit.IO(err)
		}
		fmt.Fprintf(os.Stdout, "removed directory %s\n", d.path)
	}
	return nil
}

// whatIsLeft is what is left of `job`: agents (other than `except`) in its
// workspaces or directories, workspaces named after it unless they are
// `kept` (the lead's own, closed last), and paths.
func whatIsLeft(h *herdr.Herdr, job string, workspaces []string, dirs []jobDir, except string, kept bool) ([]string, error) {
	left, err := agentsLeft(h, workspaces, dirs, except)
	if err != nil {
		return nil, err
	}
	still, err := WorkspacesLabelled(h, job)
	if err != nil {
		return nil, err
	}
	for _, id := range still {
		if !kept {
			left = append(left, fmt.Sprintf("workspace %s (%s)", job, id))
		}
	}
	for _, d := range dirs {
		if exists(d.path) {
			left = append(left, "directory "+d.path)
		}
	}
	return left, nil
}

// endJob marks the job (with `outcome`), its live rows and its worktrees
// ended. Returns the number of agent rows ended.
func endJob(conn *sql.DB, job, outcome string) (int64, error) {
	now := db.Now()
	if _, err := conn.Exec("UPDATE worktrees SET removed_at = ?1 WHERE job = ?2 AND removed_at IS NULL",
		now, job); err != nil {
		return 0, exit.Database(err)
	}
	if _, err := conn.Exec("UPDATE jobs SET state = 'ended', outcome = ?1, ended_at = ?2 WHERE job = ?3 AND state = 'open'",
		outcome, now, job); err != nil {
		return 0, exit.Database(err)
	}
	res, err := conn.Exec(
		"UPDATE agents SET state = 'ended', ended_at = ?1 WHERE job = ?2 AND state != 'ended'", now, job)
	if err != nil {
		return 0, exit.Database(err)
	}
	ended, err := res.RowsAffected()
	if err != nil {
		return 0, exit.Database(err)
	}
	return ended, nil
}

// closeWorkspaces closes every workspace labelled `job` and returns their ids.
func closeWorkspaces(h *herdr.Herdr, job string) ([]string, error) {
	workspaces, err := WorkspacesLabelled(h, job)
	if err != nil {
		return nil, err
	}
	for _, id := range workspaces {
		if _, err := h.CallOK("workspace", "close", id); err != nil {
			return nil, err
		}
		fmt.Fprintf(os.Stdout, "closed workspace %s (%s)\n", job, id)
	}
	return workspaces, nil
}

// closeOtherAgents closes the pane of every agent in one of `workspaces`
// except `me`.
func closeOtherAgents(h *herdr.Herdr, workspaces []string, me string) error {
	agents, err := HerdrAgentList(h)
	if err != nil {
		return err
	}
	for _, entry := range agents {
		agent, _ := entry.(map[string]any)
		name, _ := agent["name"].(string)
		id, _ := agent["workspace_id"].(string)
		pane, _ := agent["pane_id"].(string)
		if name == me || !slices.Contains(workspaces, id) || pane == "" {
			continue
		}
		if _, err := h.CallOK("pane", "close", pane); err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "closed agent %s (pane %s)\n", name, pane)
	}
	return nil
}

// outcomeOf is the outcome `job end` records.
func outcomeOf(abandon bool) string {
	if abandon {
		return "abandoned"
	}
	return "done"
}

// concluded hands the conclusion of an ended job to its home thread. This
// is the call site of the home-thread hook: once thread agents exist, the
// text goes to the thread recorded in jobs.home_thread; until then it is
// printed, and a job without a home thread always prints it.
func concluded(job *jobRow, text string) error {
	_, err := fmt.Fprint(os.Stdout, text)
	return err
}

// JobEnd runs `job end`.
func JobEnd(h *herdr.Herdr, args JobEndArgs) (exit.Code, error) {
	if args.Force {
		return jobEndForced(h, args)
	}
	return jobEndByLead(h, args)
}

// jobEndForced reclaims a job from outside: a thread agent, or a shell
// with no identity.
func jobEndForced(h *herdr.Herdr, args JobEndArgs) (exit.Code, error) {
	if args.Job == nil {
		return 0, exit.Refusedf("--force needs the job: `fleet job end <JOB> --force`")
	}
	if args.ReportFile != nil || args.Abandon {
		return 0, exit.Refusedf("--force takes no report and no --abandon; it reclaims the job without a conclusion")
	}
	if os.Getenv("FLEET_ROLE") != "" {
		me, err := identity.FromEnv()
		if err != nil {
			return 0, err
		}
		if me.Role != identity.Thread {
			return 0, exit.Refusedf("a %s cannot reclaim a job; only a thread agent, or a shell outside fleet, does", me.Role)
		}
	}
	job := *args.Job
	if err := CheckName(job); err != nil {
		return 0, err
	}
	home, err := Home()
	if err != nil {
		return 0, err
	}
	conn, err := OpenLedger(nil)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	open, err := openJob(conn, job)
	if err != nil {
		return 0, err
	}
	dirs, err := jobDirs(conn, open, job, home)
	if err != nil {
		return 0, err
	}
	workspaces, err := closeWorkspaces(h, job)
	if err != nil {
		return 0, err
	}
	if err := removeDirs(h, workspaces, dirs, ""); err != nil {
		return 0, err
	}
	left, err := whatIsLeft(h, job, workspaces, dirs, "", false)
	if err != nil {
		return 0, err
	}
	if len(left) > 0 {
		return 0, exit.Environmentf("%d left after reclaiming %s: %s", len(left), job, strings.Join(left, ", "))
	}
	ended, err := endJob(conn, job, "abandoned")
	if err != nil {
		return 0, err
	}
	fmt.Fprintf(os.Stdout, "reclaimed job %s: 0 left, %d rows ended\n", job, ended)
	return exit.Ok, nil
}

// leadEnding is what the lead's checks established.
type leadEnding struct {
	me     *identity.Identity
	job    *jobRow
	report string
}

// leadArgs refuses a caller that is not a lead, a job that is not the
// caller's and a bad report file, before the ledger is opened.
func leadArgs(args JobEndArgs) (*leadEnding, error) {
	me, err := identity.FromEnv()
	if err != nil {
		return nil, err
	}
	if me.Role != identity.Lead {
		return nil, exit.Refusedf("a %s cannot end a job; its lead does, or a thread agent with --force", me.Role)
	}
	if me.Job == "" {
		return nil, exit.Refusedf("FLEET_JOB is not set")
	}
	if args.Job != nil && *args.Job != me.Job {
		return nil, exit.Refusedf("your job is %s, not %s; a lead ends only its own job", me.Job, *args.Job)
	}
	if args.ReportFile == nil {
		return nil, exit.Refusedf("--report-file is required: the report goes to your work order and names the conclusion")
	}
	report, err := canonicalize(*args.ReportFile)
	if err == nil {
		_, err = os.ReadFile(report)
	}
	if err != nil {
		return nil, exit.Refusedf("cannot read %s: %v", *args.ReportFile, err)
	}
	return &leadEnding{me: me, report: report}, nil
}

// ledgerChecks refuses a job that is not open and live workers.
func (e *leadEnding) ledgerChecks(conn *sql.DB) error {
	job, err := openJob(conn, e.me.Job)
	if err != nil {
		return err
	}
	if job == nil {
		return exit.Refusedf("job %s is not open in target %s", e.me.Job, e.me.Target)
	}
	workers, err := liveRowsOf(conn, e.me.Job, "worker")
	if err != nil {
		return err
	}
	if len(workers) > 0 {
		return exit.Refusedf("job %s still has live workers: %s; wait for their `done`", e.me.Job, strings.Join(workers, ", "))
	}
	e.job = job
	return nil
}

// conclusion is the text written to the parent issue.
func conclusion(job, outcome, lead, issue, report string) string {
	text := fmt.Sprintf("Job %s ended: %s.\nLead: %s.", job, outcome, lead)
	if issue != "" {
		text += " Work order: " + issue + "."
	}
	return text + " Report: " + report + "\n"
}

// linearEnd writes the report to the lead's work order and releases it,
// then the conclusion to the parent and releases it; each only when the
// job has one.
func (e *leadEnding) linearEnd(abandon bool) error {
	if e.me.Issue != "" {
		if err := atb.Comment(e.me.Issue, e.report); err != nil {
			return err
		}
		if err := atb.Release(e.me.Issue, e.me.Agent, abandon); err != nil {
			return err
		}
	}
	if e.job.ParentIssue == "" {
		return nil
	}
	file, err := os.CreateTemp("", "fleet-conclusion-*.md")
	if err != nil {
		return exit.IO(err)
	}
	defer os.Remove(file.Name())
	_, err = file.WriteString(conclusion(e.job.Job, outcomeOf(abandon), e.me.Agent, e.me.Issue, e.report))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return exit.IO(err)
	}
	if err := atb.Comment(e.job.ParentIssue, file.Name()); err != nil {
		return err
	}
	return atb.Release(e.job.ParentIssue, e.me.Agent, abandon)
}

// jobEndByLead is the lead ending its own job.
func jobEndByLead(h *herdr.Herdr, args JobEndArgs) (exit.Code, error) {
	e, err := leadArgs(args)
	if err != nil {
		return 0, err
	}
	conn, err := OpenLedger(nil)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if err := e.ledgerChecks(conn); err != nil {
		return 0, err
	}
	if err := e.linearEnd(args.Abandon); err != nil {
		return 0, err
	}
	home, err := Home()
	if err != nil {
		return 0, err
	}
	job := e.me.Job
	dirs, err := jobDirs(conn, e.job, job, home)
	if err != nil {
		return 0, err
	}
	workspaces, err := WorkspacesLabelled(h, job)
	if err != nil {
		return 0, err
	}
	if err := closeOtherAgents(h, workspaces, e.me.Agent); err != nil {
		return 0, err
	}
	if err := removeDirs(h, workspaces, dirs, e.me.Agent); err != nil {
		return 0, err
	}
	left, err := whatIsLeft(h, job, workspaces, dirs, e.me.Agent, true)
	if err != nil {
		return 0, err
	}
	if len(left) > 0 {
		return 0, exit.Environmentf("%d left while ending %s: %s", len(left), job, strings.Join(left, ", "))
	}
	outcome := outcomeOf(args.Abandon)
	ended, err := endJob(conn, job, outcome)
	if err != nil {
		return 0, err
	}
	fmt.Fprintf(os.Stdout, "ended job %s: %s, %d rows ended\n", job, outcome, ended)
	if err := concluded(e.job, conclusion(job, outcome, e.me.Agent, e.me.Issue, e.report)); err != nil {
		return 0, err
	}
	// Last, so the lead's own pane goes with it.
	if _, err := closeWorkspaces(h, job); err != nil {
		return 0, err
	}
	return exit.Ok, nil
}

// `fleet job end`: the lead ends its job; `--force` reclaims a job from
// outside.

package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Luolc/agent-fleet/internal/atb"
	"github.com/Luolc/agent-fleet/internal/config"
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
		"the job's parent issue and fleet's claim on the parent is released. The parent is " +
		"closed with --close-parent (done or abandoned), for when everything it asks for " +
		"is done or given up; without it the parent returns to the state it had before the " +
		"job claimed it (`--restore`), and a later job can take it, except that a parent the " +
		"job created (`job start --new-parent`) is closed as done when the job ends done. " +
		"Each step done is " +
		"recorded in the ledger; if a step fails, `job end` exits 5 naming it and nothing else " +
		"changes, and running it again continues from the first step not recorded, so a " +
		"release already done is not repeated; the same holds for the ledger update, the " +
		"conclusion and the workspace close at the end.\n\n" +
		"Then the cleanup: every other agent in the job's workspace is closed; no agent but you " +
		"may have its cwd inside one of the job's directories (the worktrees `fleet worktree` " +
		"recorded for it and, for a cross-repo job, its directory <I>/<job>/ under the scope's `paths.initiatives`); each worktree is " +
		"removed with `git worktree remove --force` once `git status` finds no uncommitted " +
		"changes in it, its submodules included, and no submodule has a stash or a commit " +
		"on a branch that is on no remote, and its local branch deleted, and the " +
		"cross-repo directory is removed unless a git checkout inside it has uncommitted " +
		"changes. When something is left it is " +
		"listed and `job end` exits 5 without ending the job. Otherwise the job is marked ended " +
		"with its outcome, your row ended, the worktrees removed, the conclusion is printed " +
		"(the line the job's home thread will receive once threads exist), and last the " +
		"workspace is closed, which ends your own pane.\n\n" +
		"`job end <JOB> --force` reclaims a job from outside: a thread agent, or a shell with " +
		"no FLEET_ROLE. No report. It closes the job's workspace, removes the " +
		"job's directories with the same checks (no agent inside, no uncommitted changes), and " +
		"ends every live row of the job; an open job is marked ended with the outcome " +
		"`abandoned`. Then the Linear steps, each recorded: the lead's work order and each " +
		"work order a live worker held get a line naming who reclaimed the job, without a " +
		"report, and are released as abandoned; the parent gets the same line and fleet's " +
		"claim on it is released with --restore. A step the lead's `job end`, `worker end` or " +
		"watch already did is not done again. A failed Linear step does not stop the reclaim: " +
		"the steps not done are listed and the exit is 5. Parts already gone are skipped, so " +
		"it can be run again, and a second run does only the Linear steps not yet done.\n\n" +
		"Exit: 0 when the job ended; 1 when a check refuses (live workers, the caller, the " +
		"report file, the flags, --close-parent on a job without a parent issue); 2/3/4/8 as `send` when the " +
		"conclusion did not reach the home thread's agent (run `job end` again to deliver it); 5 when atb, git, " +
		"herdr or the database fails, the scope has no ledger, or something is left (listed)."
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
	// CloseParent closes the parent issue as `done` or `abandoned`; nil
	// releases the claim only.
	CloseParent *string
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

// jobDir is a directory `job end` removes: a worktree, or the cross-repo
// directory.
type jobDir struct {
	path     string
	worktree bool
	// keepBranch keeps a worktree's branch: `worker end` runs mid-job,
	// when the branch's commits may be on no remote yet.
	keepBranch bool
}

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
// records for it, then the cross-repo directory of a cross-repo job, when
// it is still in an initiative's checkout under `initiatives`.
func jobDirs(conn *sql.DB, job *jobRow, name, initiatives string) ([]jobDir, error) {
	dirs, err := jobWorktrees(conn, name)
	if err != nil {
		return nil, err
	}
	if job != nil && job.Repo == "" && filepath.Base(job.LeadCwd) == name &&
		filepath.Dir(filepath.Dir(job.LeadCwd)) == initiatives {
		dirs = append(dirs, jobDir{path: job.LeadCwd})
	}
	return dirs, nil
}

// jobWorktrees are the worktrees the ledger records for `job` and not yet
// removed, oldest first.
func jobWorktrees(conn *sql.DB, job string) ([]jobDir, error) {
	rows, err := conn.Query(
		"SELECT path FROM worktrees WHERE job = ?1 AND removed_at IS NULL ORDER BY id", job)
	if err != nil {
		return nil, exit.Database(err)
	}
	defer rows.Close()
	var found []jobDir
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, exit.Database(err)
		}
		found = append(found, jobDir{path: path, worktree: true})
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

// removeWorktree removes `worktree` from the repository it was made from
// and deletes the branch it had checked out, if any, unless keepBranch.
// git names that
// repository's directory, so it is found wherever the checkout is and
// whatever its layout (a `.git` directory or a separate git dir).
func removeWorktree(worktree string, keepBranch bool) error {
	out, err := Git("-C", worktree, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	common := strings.TrimSpace(out)
	var branch string
	if out, err := Git("-C", worktree, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		branch = strings.TrimSpace(out)
	}
	// --force: git refuses any worktree with an initialized submodule
	// without it. dirtyCheckouts has already found nothing to lose.
	if _, err := Git("-C", common, "worktree", "remove", "--force", worktree); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "removed worktree %s\n", worktree)
	if branch != "" && keepBranch {
		fmt.Fprintf(os.Stdout, "kept branch %s\n", branch)
		return nil
	}
	if branch != "" {
		// -D: after a squash merge the branch is not an ancestor of main.
		if _, err := Git("-C", common, "branch", "-D", branch); err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "deleted branch %s\n", branch)
	}
	return nil
}

// inTheWay is what keeps `dirs` from being removed: the agents other than
// `except` in one of `workspaces` or `dirs`, else the checkouts with
// uncommitted changes.
func inTheWay(h *herdr.Herdr, workspaces []string, dirs []jobDir, except string) ([]string, error) {
	blocking, err := agentsLeft(h, workspaces, dirs, except)
	if err != nil || len(blocking) > 0 {
		return blocking, err
	}
	return dirtyCheckouts(dirs)
}

// removeDirs refuses while something is in the way (inTheWay), then
// removes each directory that still exists.
func removeDirs(h *herdr.Herdr, workspaces []string, dirs []jobDir, except string) error {
	blocking, err := inTheWay(h, workspaces, dirs, except)
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
		return exit.Environmentf("%s: in the way: %s", what, strings.Join(blocking, ", "))
	}
	for _, d := range dirs {
		if !exists(d.path) {
			continue
		}
		if d.worktree {
			if err := removeWorktree(d.path, d.keepBranch); err != nil {
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

// dirtyCheckouts are the git checkouts with uncommitted changes, which
// removing would lose: the worktrees, and those inside the directories that
// are removed whole (a cross-repo job's).
func dirtyCheckouts(dirs []jobDir) ([]string, error) {
	var checkouts []string
	for _, d := range dirs {
		if !exists(d.path) {
			continue
		}
		if d.worktree {
			checkouts = append(checkouts, d.path)
			continue
		}
		err := filepath.WalkDir(d.path, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.Name() != ".git" {
				return err
			}
			checkouts = append(checkouts, filepath.Dir(path))
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	var dirty []string
	for _, checkout := range checkouts {
		lost, err := wouldLose(checkout)
		if err != nil {
			return nil, err
		}
		if lost {
			dirty = append(dirty, "uncommitted changes in "+checkout)
		}
	}
	return dirty, nil
}

// wouldLose reports whether removing `checkout` would lose work: changes
// `git status` sees, its submodules' included, or what a submodule keeps
// only in its own repository (commits on no remote, stashes), which goes
// with the worktree's git dir.
func wouldLose(checkout string) (bool, error) {
	status, err := Git("-C", checkout, "status", "--porcelain", "--ignore-submodules=none")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(status) != "" {
		return true, nil
	}
	local, err := Git("-C", checkout, "submodule", "foreach", "--recursive", "--quiet",
		"git log --oneline --branches --not --remotes; git stash list")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(local) != "", nil
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

// concluded hands the conclusion of an ended job to its home thread, as
// `ask-human` hands a question: to the thread's live agent, or to one
// started for it, headed with the lead's name. The text is printed as
// well; a job without a home thread only prints it. A delivery that did
// not succeed is a failure with its exit code, so the step is not recorded
// and a retry delivers again.
func concluded(h *herdr.Herdr, conn *sql.DB, me *identity.Identity, job *jobRow, text string) error {
	if _, err := fmt.Fprint(os.Stdout, text); err != nil {
		return err
	}
	if job.HomeThread == "" {
		return nil
	}
	cfg, err := config.LoadScope(me.Scope)
	if err != nil {
		return err
	}
	msg := inboundMessage{Thread: job.HomeThread, Text: text, Question: me.Agent, Conclusion: true}
	code, err := toThread(h, conn, me.Scope, cfg, msg)
	if err != nil {
		return err
	}
	if code != exit.Ok {
		return exit.New(code, fmt.Sprintf("the conclusion did not reach the home thread %s (exit %d); run `job end` again to deliver it",
			job.HomeThread, code))
	}
	return nil
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
	job, err := forcedArgs(args)
	if err != nil {
		return 0, err
	}
	scope, err := identity.Scope()
	if err != nil {
		return 0, err
	}
	sc, err := config.LoadScope(scope)
	if err != nil {
		return 0, err
	}
	conn, err := OpenLedger()
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	ended, failed, err := reclaimJob(h, conn, sc, job, callerName())
	if err != nil {
		return 0, err
	}
	fmt.Fprintf(os.Stdout, "reclaimed job %s: 0 left, %d rows ended\n", job, ended)
	if len(failed) > 0 {
		fmt.Fprintf(os.Stdout, "Linear steps not done for job %s:\n", job)
		for _, step := range failed {
			fmt.Fprintf(os.Stdout, "  - %s\n", step)
		}
		return 0, exit.Environmentf("%d Linear steps of job %s failed; run `fleet job end %s --force` again to retry them",
			len(failed), job, job)
	}
	return exit.Ok, nil
}

// reclaimJob closes the job's workspaces, removes its directories, ends it
// as abandoned with its live rows, then does the Linear steps of the
// reclaim (reclaimLinear): the reclaim of `job end --force`, which `watch`
// also runs, `by` naming who reclaims. It returns the rows ended and the
// Linear steps that failed, which a later reclaim retries.
func reclaimJob(h *herdr.Herdr, conn *sql.DB, sc *config.Scope, job, by string) (int64, []string, error) {
	open, err := openJob(conn, job)
	if err != nil {
		return 0, nil, err
	}
	dirs, err := jobDirs(conn, open, job, sc.Paths.Initiatives)
	if err != nil {
		return 0, nil, err
	}
	workspaces, err := closeWorkspaces(h, job)
	if err != nil {
		return 0, nil, err
	}
	if err := removeDirs(h, workspaces, dirs, ""); err != nil {
		return 0, nil, err
	}
	left, err := whatIsLeft(h, job, workspaces, dirs, "", false)
	if err != nil {
		return 0, nil, err
	}
	if len(left) > 0 {
		return 0, nil, exit.Environmentf("%d left after reclaiming %s: %s", len(left), job, strings.Join(left, ", "))
	}
	if open != nil {
		if err := markReclaimed(conn, open); err != nil {
			return 0, nil, err
		}
	}
	ended, err := endJob(conn, job, "abandoned")
	if err != nil {
		return 0, nil, err
	}
	if open != nil {
		markThread(conn, sc.FednetSocket, open.HomeThread)
	}
	failed, err := reclaimLinear(conn, job, by)
	return ended, failed, err
}

// markReclaimed records, before its rows are ended, that the reclaim
// ends `open`, and which of its live workers hold a work order the
// reclaim releases: not one whose release watch recorded when it found
// the worker gone. A retry after the rows are ended finds them by these
// records.
func markReclaimed(conn *sql.DB, open *jobRow) error {
	key, now := endKey(open), db.Now()
	if _, err := conn.Exec("INSERT OR IGNORE INTO steps (key, step, done_at) VALUES (?1, 'reclaim', ?2)", key, now); err != nil {
		return exit.Database(err)
	}
	_, err := conn.Exec("INSERT OR IGNORE INTO steps (key, step, done_at) SELECT ?1, 'reclaim-worker:' || a.id, ?2 "+
		"FROM agents a WHERE a.job = ?3 AND a.role = 'worker' AND a.state != 'ended' AND a.issue != '' AND NOT EXISTS "+
		"(SELECT 1 FROM steps s WHERE s.key = 'watch-gone:' || a.id AND s.step = 'release-work-order')",
		key, now, open.Job)
	return dbErr(err)
}

// reclaimLinear does the Linear steps of the reclaim of the newest job
// named `name`, when a reclaim marked it (markReclaimed): the lead's work
// order and each marked worker's get a line saying who reclaimed the job,
// without a report, and are released as abandoned; the parent gets the
// same line and fleet's claim on it is released with --restore, as an
// abandoned `job end` does. Each step is recorded under the key the
// lead's `job end` or `worker end` uses for it, so one either of them, or
// an earlier reclaim, did is not done again. A failed step does not stop
// the others, but an issue's release waits for its comment; the failures
// are returned.
func reclaimLinear(conn *sql.DB, name, by string) ([]string, error) {
	job, err := latestJob(conn, name)
	if err != nil || job == nil || !stepDone(conn, endKey(job), "reclaim") {
		return nil, err
	}
	key := endKey(job)
	lead, issue, orders, err := reclaimedOrders(conn, key, name)
	if err != nil {
		return nil, err
	}
	var failed []string
	try := func(key, step, what string, do func() error) bool {
		err := runStep(conn, key, step, func() error {
			if err := do(); err != nil {
				return err
			}
			fmt.Fprintf(os.Stdout, "Linear: %s\n", what)
			return nil
		})
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", what, err))
			atb.WriteStderr(os.Stderr, err)
		}
		return err == nil
	}
	reclaimed := fmt.Sprintf("Job %s was reclaimed by %s with `fleet job end --force`, without a report", name, by)
	for _, o := range orders {
		line := fmt.Sprintf("%s: %s ended with it. Its work order is released as abandoned.\n", reclaimed, o.agent)
		if o.commented || try(o.key, "comment-work-order", "comment on work order "+o.issue, func() error { return commentText(o.issue, line) }) {
			try(o.key, "release-work-order", fmt.Sprintf("release work order %s (agent %s) as abandoned", o.issue, o.agent),
				func() error { return atb.Release(o.issue, o.agent, true) })
		}
	}
	if job.ParentIssue != "" {
		line := fmt.Sprintf("%s: abandoned.\nLead: %s.", reclaimed, lead)
		if issue != "" {
			line += " Work order: " + issue + "."
		}
		if try(key, "comment-parent", "comment on parent "+job.ParentIssue, func() error { return commentText(job.ParentIssue, line+"\n") }) {
			try(key, "release-parent", fmt.Sprintf("release parent %s (agent %s) with --restore", job.ParentIssue, lead),
				func() error { return atb.ReleaseRestore(job.ParentIssue, lead, "job "+name+" reclaimed") })
		}
	}
	return failed, nil
}

// heldOrder is a work order a reclaim releases: its issue, the agent
// holding it, and the steps key its comment and release are recorded under.
type heldOrder struct {
	key, issue, agent string
	// commented is whether watch's gone rule already wrote its line.
	commented bool
}

// reclaimedOrders are the lead of the job named `name` and its work order,
// and the work orders the reclaim marked under `key` releases: the lead's,
// then the marked workers'.
func reclaimedOrders(conn *sql.DB, key, name string) (string, string, []heldOrder, error) {
	var lead, issue string
	err := conn.QueryRow("SELECT name, issue FROM agents WHERE job = ?1 AND role = 'lead' ORDER BY id DESC LIMIT 1",
		name).Scan(&lead, &issue)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", "", nil, exit.Database(err)
	}
	var orders []heldOrder
	if issue != "" {
		orders = append(orders, heldOrder{key: key, issue: issue, agent: lead})
	}
	err = eachRow(conn, "SELECT a.id, a.name, a.issue, EXISTS (SELECT 1 FROM steps g WHERE g.key = 'watch-gone:' || a.id "+
		"AND g.step = 'comment-work-order') FROM agents a JOIN steps s ON s.key = ?1 AND "+
		"s.step = 'reclaim-worker:' || a.id ORDER BY a.id", func(rows *sql.Rows) error {
		var id int64
		var o heldOrder
		if err := rows.Scan(&id, &o.agent, &o.issue, &o.commented); err != nil {
			return err
		}
		o.key = fmt.Sprintf("worker-end:%d", id)
		orders = append(orders, o)
		return nil
	}, key)
	return lead, issue, orders, err
}

// forcedArgs refuses a reclaim without a job, with a report or --abandon,
// or by a lead or worker; returns the job name.
func forcedArgs(args JobEndArgs) (string, error) {
	if args.Job == nil {
		return "", exit.Refusedf("--force needs the job: `fleet job end <JOB> --force`")
	}
	if args.ReportFile != nil || args.Abandon || args.CloseParent != nil {
		return "", exit.Refusedf("--force takes no report, no --abandon and no --close-parent; it reclaims the job without a conclusion")
	}
	if os.Getenv("FLEET_ROLE") != "" {
		me, err := identity.FromEnv()
		if err != nil {
			return "", err
		}
		if me.Role != identity.Thread {
			return "", exit.Refusedf("a %s cannot reclaim a job; only a thread agent, or a shell outside fleet, does", me.Role)
		}
	}
	return *args.Job, CheckName(*args.Job)
}

// leadEnding is what the lead's checks established.
type leadEnding struct {
	me     *identity.Identity
	job    *jobRow
	report string
	// closeParent is `done`, `abandoned` or "" (--close-parent not given).
	closeParent string
	// parentCreated is whether `job start --new-parent` created the parent.
	parentCreated bool
}

// leadArgs refuses a caller that is not a lead, a job that is not the
// caller's, a bad report file and a bad --close-parent, before the ledger
// is opened.
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
	e := &leadEnding{me: me, report: report}
	if args.CloseParent != nil {
		e.closeParent = *args.CloseParent
		if e.closeParent != "done" && e.closeParent != "abandoned" {
			return nil, exit.Refusedf("--close-parent is `done` or `abandoned`, not %q", e.closeParent)
		}
	}
	return e, nil
}

// ledgerChecks refuses a job that is not open, unless it is the caller's
// ending that stopped before its last step (the workspace close), live
// workers, and --close-parent on a job without a parent issue.
func (e *leadEnding) ledgerChecks(conn *sql.DB) error {
	job, err := latestJob(conn, e.me.Job)
	if err != nil {
		return err
	}
	if job == nil || (job.State != "open" && !unfinished(conn, job)) {
		return exit.Refusedf("job %s is not open in scope %s", e.me.Job, e.me.Scope)
	}
	workers, err := liveRowsOf(conn, e.me.Job, "worker")
	if err != nil {
		return err
	}
	if len(workers) > 0 {
		return exit.Refusedf("job %s still has live workers: %s; wait for their `done`", e.me.Job, strings.Join(workers, ", "))
	}
	if e.closeParent != "" && job.ParentIssue == "" {
		return exit.Refusedf("job %s has no parent issue to close", e.me.Job)
	}
	if err := conn.QueryRow("SELECT parent_created FROM jobs WHERE id = ?1", job.ID).Scan(&e.parentCreated); err != nil {
		return exit.Database(err)
	}
	e.job = job
	return nil
}

// parentClose is how the parent is closed: as --close-parent says, else
// `done` when the job created it and ends done, else "": the claim is
// given back and the parent returns to its state from before the job.
func (e *leadEnding) parentClose(abandon bool) string {
	if e.closeParent == "" && e.parentCreated && !abandon {
		return "done"
	}
	return e.closeParent
}

// stepDone is whether `step` of the ending `key` is recorded.
func stepDone(conn querier, key, step string) bool {
	var done int64
	err := conn.QueryRow("SELECT count(*) FROM steps WHERE key = ?1 AND step = ?2", key, step).Scan(&done)
	return err == nil && done > 0
}

// unfinished is whether the ending of `job` marked it ended but stopped
// before closing the workspace.
func unfinished(conn querier, job *jobRow) bool {
	key := endKey(job)
	return stepDone(conn, key, "end-ledger") && !stepDone(conn, key, "close-workspace")
}

// endKey is the steps key of the ending of `job`.
func endKey(job *jobRow) string {
	return fmt.Sprintf("job-end:%d", job.ID)
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
// then the conclusion to the parent and releases fleet's claim on it,
// closing it only as parentClose says; each only when the job has one.
// Each step done is recorded under the job's key, so a retry after a
// failure continues where it stopped.
func (e *leadEnding) linearEnd(conn querier, abandon bool) error {
	key := endKey(e.job)
	if e.me.Issue != "" {
		if err := runStep(conn, key, "comment-work-order", func() error { return atb.Comment(e.me.Issue, e.report) }); err != nil {
			return err
		}
		if err := runStep(conn, key, "release-work-order", func() error { return atb.Release(e.me.Issue, e.me.Agent, abandon) }); err != nil {
			return err
		}
	}
	if e.job.ParentIssue == "" {
		return nil
	}
	if err := runStep(conn, key, "comment-parent", func() error {
		return e.commentParent(conclusion(e.job.Job, outcomeOf(abandon), e.me.Agent, e.me.Issue, e.report))
	}); err != nil {
		return err
	}
	return runStep(conn, key, "release-parent", func() error {
		how := e.parentClose(abandon)
		if how == "" {
			return atb.ReleaseRestore(e.job.ParentIssue, e.me.Agent, "job "+e.job.Job+" ended")
		}
		return atb.Release(e.job.ParentIssue, e.me.Agent, how == "abandoned")
	})
}

// commentParent writes `text` to the parent issue.
func (e *leadEnding) commentParent(text string) error {
	return commentText(e.job.ParentIssue, text)
}

// commentText writes `text` to `issue` through a temporary file.
func commentText(issue, text string) error {
	file, err := os.CreateTemp("", "fleet-comment-*.md")
	if err != nil {
		return exit.IO(err)
	}
	defer os.Remove(file.Name())
	_, err = file.WriteString(text)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return exit.IO(err)
	}
	return atb.Comment(issue, file.Name())
}

// jobEndByLead is the lead ending its own job.
func jobEndByLead(h *herdr.Herdr, args JobEndArgs) (exit.Code, error) {
	e, err := leadArgs(args)
	if err != nil {
		return 0, err
	}
	conn, err := OpenLedger()
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if err := e.ledgerChecks(conn); err != nil {
		return 0, err
	}
	// Read before the Linear steps, so an invalid file stops nothing midway.
	sc, err := config.LoadScope(e.me.Scope)
	if err != nil {
		return 0, err
	}
	if err := e.linearEnd(conn, args.Abandon); err != nil {
		return 0, err
	}
	job := e.me.Job
	dirs, err := jobDirs(conn, e.job, job, sc.Paths.Initiatives)
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
	key := endKey(e.job)
	// The last three steps are recorded too: a retry after the workspace
	// close failed does not end rows twice or conclude twice.
	if err := runStep(conn, key, "end-ledger", func() error {
		ended, err := endJob(conn, job, outcome)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "ended job %s: %s, %d rows ended\n", job, outcome, ended)
		return nil
	}); err != nil {
		return 0, err
	}
	if err := runStep(conn, key, "conclude", func() error {
		return concluded(h, conn, e.me, e.job, conclusion(job, outcome, e.me.Agent, e.me.Issue, e.report))
	}); err != nil {
		return 0, err
	}
	markThread(conn, sc.FednetSocket, e.job.HomeThread)
	// Last, so the lead's own pane goes with it.
	if err := runStep(conn, key, "close-workspace", func() error {
		_, err := closeWorkspaces(h, job)
		return err
	}); err != nil {
		return 0, err
	}
	return exit.Ok, nil
}

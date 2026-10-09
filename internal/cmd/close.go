// `fleet close`: clean up a job that has ended.

package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
	"github.com/Luolc/agent-fleet/internal/identity"
)

// CloseAbout and CloseLongAbout are the help texts of `close`.
const (
	CloseAbout     = "Clean up a job that has ended: workspace, worktrees, branches, ledger rows"
	CloseLongAbout = "Clean up a job that has ended: workspace, worktrees, branches, ledger rows.\n\n" +
		"Called by a thread agent. Refused while the job still has live rows unless --force is " +
		"given. Closes the job's workspace, then checks that no agent in herdr has its cwd " +
		"inside one of the job's directories: the worktrees `fleet worktree` recorded for it " +
		"and, for a cross-repo job, ~/cross-repo/<job>/. Only then removes each worktree with " +
		"`git worktree remove` (which refuses uncommitted changes), deletes its local branch, " +
		"and removes the cross-repo directory. Finally counts what is left: workspaces named " +
		"after the job, agents in them or in a directory, and the paths. Success only when " +
		"that count is 0; the job and its rows are then marked ended and its worktrees " +
		"removed. Parts already gone are skipped, so it can be run again.\n\n" +
		"Exit: 0 when nothing is left; 1 when live rows remain without --force, or the caller " +
		"is not a thread agent; 5 when git or herdr fails, or something is left (listed)."
)

// CloseArgs are the arguments of `close`.
type CloseArgs struct {
	// Job is the job id, as given to `fleet job start`.
	Job string
	// Force closes even if agents are still recorded as live (the cleanup
	// after a failed start).
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

// jobDir is a directory `close` removes: a worktree with the checkout it
// belongs to, or the cross-repo directory (no checkout).
type jobDir struct{ path, checkout string }

// agentsLeft are the agents in one of `workspaces` or with their cwd inside
// one of `dirs`.
func agentsLeft(h *herdr.Herdr, workspaces []string, dirs []jobDir) ([]string, error) {
	agents, err := HerdrAgentList(h)
	if err != nil {
		return nil, err
	}
	left := []string{}
	for _, entry := range agents {
		agent, _ := entry.(map[string]any)
		id, ok := agent["workspace_id"].(string)
		if ok && slices.Contains(workspaces, id) || slices.ContainsFunc(dirs, func(d jobDir) bool {
			return inside(agent, d.path)
		}) {
			left = append(left, describe(agent))
		}
	}
	return left, nil
}

// closeChecks refuses a caller that is not a thread agent, a bad job name
// and live rows without --force, and finds the job's directories: the
// worktrees `fleet worktree` recorded for it, then the cross-repo
// directory of a cross-repo job.
func closeChecks(args CloseArgs) (conn *sql.DB, dirs []jobDir, err error) {
	me, err := identity.FromEnv()
	if err != nil {
		return nil, nil, err
	}
	if me.Role != identity.Thread {
		return nil, nil, exit.Refusedf("only a thread agent closes jobs")
	}
	if err := CheckName(args.Job); err != nil {
		return nil, nil, err
	}
	home, err := Home()
	if err != nil {
		return nil, nil, err
	}
	conn, err = db.Open(me.Target)
	if err != nil {
		return nil, nil, err
	}
	if err := refuseLiveRows(conn, args); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if dirs, err = jobWorktrees(conn, args.Job, home); err != nil {
		conn.Close()
		return nil, nil, err
	}
	job, err := openJob(conn, args.Job)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	if job != nil && job.Repo == "" && job.LeadCwd == filepath.Join(home, "cross-repo", args.Job) {
		dirs = append(dirs, jobDir{path: job.LeadCwd})
	}
	return conn, dirs, nil
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

// refuseLiveRows refuses live rows without --force, naming them.
func refuseLiveRows(conn *sql.DB, args CloseArgs) error {
	rows, err := conn.Query("SELECT name FROM agents WHERE job = ?1 AND state != 'ended' ORDER BY id", args.Job)
	if err != nil {
		return exit.Database(err)
	}
	defer rows.Close()
	live := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return exit.Database(err)
		}
		live = append(live, name)
	}
	if err := rows.Err(); err != nil {
		return exit.Database(err)
	}
	if len(live) > 0 && !args.Force {
		return exit.Refusedf("job %s still has live agents: %s; wait for their `done`, or pass --force",
			args.Job, strings.Join(live, ", "))
	}
	return nil
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

// removeDirs refuses while an agent is in one of `workspaces` or `dirs`,
// then removes each directory that still exists.
func removeDirs(h *herdr.Herdr, workspaces []string, dirs []jobDir) error {
	blocking, err := agentsLeft(h, workspaces, dirs)
	if err != nil {
		return err
	}
	if len(blocking) > 0 {
		paths := make([]string, len(dirs))
		for i, d := range dirs {
			paths[i] = d.path
		}
		return exit.Environmentf("not removing %s: still in use by %s",
			strings.Join(paths, ", "), strings.Join(blocking, ", "))
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

// closeLeft is what is left of `job`: agents in its workspaces or
// directories, workspaces named after it, and paths.
func closeLeft(h *herdr.Herdr, job string, workspaces []string, dirs []jobDir) ([]string, error) {
	left, err := agentsLeft(h, workspaces, dirs)
	if err != nil {
		return nil, err
	}
	still, err := WorkspacesLabelled(h, job)
	if err != nil {
		return nil, err
	}
	for _, id := range still {
		left = append(left, fmt.Sprintf("workspace %s (%s)", job, id))
	}
	for _, d := range dirs {
		if exists(d.path) {
			left = append(left, "directory "+d.path)
		}
	}
	return left, nil
}

// endJob marks the job, its live rows and its worktrees ended. Returns the
// number of agent rows ended.
func endJob(conn *sql.DB, job string) (int64, error) {
	now := db.Now()
	if _, err := conn.Exec("UPDATE worktrees SET removed_at = ?1 WHERE job = ?2 AND removed_at IS NULL",
		now, job); err != nil {
		return 0, exit.Database(err)
	}
	if _, err := conn.Exec("UPDATE jobs SET state = 'ended', ended_at = ?1 WHERE job = ?2 AND state = 'open'",
		now, job); err != nil {
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

// Close runs `close`.
func Close(h *herdr.Herdr, args CloseArgs) (exit.Code, error) {
	conn, dirs, err := closeChecks(args)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	workspaces, err := WorkspacesLabelled(h, args.Job)
	if err != nil {
		return 0, err
	}
	for _, id := range workspaces {
		if _, err := h.CallOK("workspace", "close", id); err != nil {
			return 0, err
		}
		fmt.Fprintf(os.Stdout, "closed workspace %s (%s)\n", args.Job, id)
	}

	if err := removeDirs(h, workspaces, dirs); err != nil {
		return 0, err
	}
	left, err := closeLeft(h, args.Job, workspaces, dirs)
	if err != nil {
		return 0, err
	}
	if len(left) > 0 {
		return 0, exit.Environmentf("%d left after closing %s: %s", len(left), args.Job, strings.Join(left, ", "))
	}
	ended, err := endJob(conn, args.Job)
	if err != nil {
		return 0, err
	}
	fmt.Fprintf(os.Stdout, "closed job %s: 0 left, %d rows ended\n", args.Job, ended)
	return exit.Ok, nil
}

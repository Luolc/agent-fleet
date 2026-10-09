// `fleet close`: clean up a job after its PR is merged.

package cmd

import (
	"database/sql"
	"errors"
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
	CloseAbout     = "Clean up a job after its PR is merged: workspace, worktrees, branches, ledger rows"
	CloseLongAbout = "Clean up a job after its PR is merged: workspace, worktrees, branches, ledger rows.\n\n" +
		"Called by the orchestra. Refused while the job still has live rows unless --force is " +
		"given. Closes the job's workspace, then checks that no agent in herdr has its cwd " +
		"inside one of the job's worktrees: its own and those `fleet worktree` recorded for it. " +
		"Only then removes each with `git worktree remove` (which refuses uncommitted changes) " +
		"and deletes its local branch. Finally counts what is left: workspaces named after the " +
		"job, agents in them or in a worktree, and the worktree paths. Success only when that " +
		"count is 0; the job's rows are then marked ended and its worktrees removed. Parts " +
		"already gone are skipped, so it can be run again.\n\n" +
		"Exit: 0 when nothing is left; 1 when live rows remain without --force, or the caller " +
		"is not the orchestra; 5 when git or herdr fails, or something is left (listed)."
)

// CloseArgs are the arguments of `close`.
type CloseArgs struct {
	// Job is the job id, as given to `fleet spawn`.
	Job string
	// Force closes even if agents are still recorded as live (the cleanup
	// after a failed spawn).
	Force bool
}

func inside(agent map[string]any, worktree string) bool {
	for _, key := range []string{"cwd", "foreground_cwd"} {
		if cwd, ok := agent[key].(string); ok && pathStartsWith(cwd, worktree) {
			return true
		}
	}
	return false
}

// pathStartsWith is `Path::starts_with`: `base`'s components are a prefix
// of `path`'s, so `/w/item-10` does not start with `/w/item-1`.
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

// components are a Unix path's components as `Path::components` yields
// them: the root as "/", repeated separators and `.` after the start
// dropped, `..` kept.
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

// agentsLeft are the agents in one of `workspaces` or with their cwd inside
// one of `worktrees`.
func agentsLeft(h *herdr.Herdr, workspaces []string, worktrees []jobWorktree) ([]string, error) {
	agents, err := HerdrAgentList(h)
	if err != nil {
		return nil, err
	}
	left := []string{}
	for _, entry := range agents {
		agent, _ := entry.(map[string]any)
		id, ok := agent["workspace_id"].(string)
		if ok && slices.Contains(workspaces, id) || slices.ContainsFunc(worktrees, func(w jobWorktree) bool {
			return inside(agent, w.path)
		}) {
			left = append(left, describe(agent))
		}
	}
	return left, nil
}

// jobWorktree is a worktree `close` removes, with the checkout it belongs to.
type jobWorktree struct{ path, checkout string }

// closeChecks refuses a caller that is not the orchestra, a bad job name
// and live rows without --force, and finds the job's worktrees: its own
// (the last one recorded, else `$HOME/wt/<repo>/<job>`, in
// `$HOME/dev/<repo>`), then those `fleet worktree` recorded for the job.
func closeChecks(args CloseArgs) (conn *sql.DB, worktrees []jobWorktree, err error) {
	me, err := identity.FromEnv()
	if err != nil {
		return nil, nil, err
	}
	if me.Role != identity.Orchestra {
		return nil, nil, exit.Refusedf("only the orchestra closes jobs")
	}
	if err := CheckName(args.Job); err != nil {
		return nil, nil, err
	}
	repo, err := RepoName(me.Repo)
	if err != nil {
		return nil, nil, err
	}
	home, err := Home()
	if err != nil {
		return nil, nil, err
	}
	conn, err = db.Open(me.Repo)
	if err != nil {
		return nil, nil, err
	}
	own, err := closeWorktree(conn, args, filepath.Join(home, "wt", repo, args.Job))
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	worktrees = []jobWorktree{{own, filepath.Join(home, "dev", repo)}}
	recorded, err := jobWorktrees(conn, args.Job, home)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	for _, w := range recorded {
		if w.path != own {
			worktrees = append(worktrees, w)
		}
	}
	return conn, worktrees, nil
}

// jobWorktrees are the worktrees the ledger records for `job` and not yet
// removed, oldest first.
func jobWorktrees(conn *sql.DB, job, home string) ([]jobWorktree, error) {
	rows, err := conn.Query(
		"SELECT path, repo FROM worktrees WHERE job = ?1 AND removed_at IS NULL ORDER BY id", job)
	if err != nil {
		return nil, exit.Database(err)
	}
	defer rows.Close()
	var found []jobWorktree
	for rows.Next() {
		var path, repo string
		if err := rows.Scan(&path, &repo); err != nil {
			return nil, exit.Database(err)
		}
		found = append(found, jobWorktree{path, filepath.Join(home, "dev", repo)})
	}
	if err := rows.Err(); err != nil {
		return nil, exit.Database(err)
	}
	return found, nil
}

// closeWorktree refuses live rows without --force, then returns the job's
// last recorded worktree, or `fallback` when none is recorded.
func closeWorktree(conn *sql.DB, args CloseArgs, fallback string) (string, error) {
	rows, err := conn.Query("SELECT name FROM agents WHERE job = ?1 AND state != 'ended' ORDER BY id", args.Job)
	if err != nil {
		return "", exit.Database(err)
	}
	live := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return "", exit.Database(err)
		}
		live = append(live, name)
	}
	if err := rows.Close(); err != nil {
		return "", exit.Database(err)
	}
	if err := rows.Err(); err != nil {
		return "", exit.Database(err)
	}
	if len(live) > 0 && !args.Force {
		return "", exit.Refusedf("job %s still has live agents: %s; wait for their `done`, or pass --force",
			args.Job, strings.Join(live, ", "))
	}
	var recorded string
	err = conn.QueryRow(
		"SELECT worktree FROM agents WHERE job = ?1 AND worktree != '' ORDER BY id DESC LIMIT 1",
		args.Job).Scan(&recorded)
	if errors.Is(err, sql.ErrNoRows) {
		return fallback, nil
	}
	if err != nil {
		return "", exit.Database(err)
	}
	return recorded, nil
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

// removeWorktrees refuses while an agent is in one of `workspaces` or
// `worktrees`, then removes each worktree that still exists.
func removeWorktrees(h *herdr.Herdr, workspaces []string, worktrees []jobWorktree) error {
	blocking, err := agentsLeft(h, workspaces, worktrees)
	if err != nil {
		return err
	}
	if len(blocking) > 0 {
		paths := make([]string, len(worktrees))
		for i, w := range worktrees {
			paths[i] = w.path
		}
		return exit.Environmentf("not removing %s: still in use by %s",
			strings.Join(paths, ", "), strings.Join(blocking, ", "))
	}
	for _, w := range worktrees {
		if exists(w.path) {
			if err := removeWorktree(w.checkout, w.path); err != nil {
				return err
			}
		}
	}
	return nil
}

// closeLeft is what is left of `job`: agents in its workspaces or
// worktrees, workspaces named after it, and worktree paths.
func closeLeft(h *herdr.Herdr, job string, workspaces []string, worktrees []jobWorktree) ([]string, error) {
	left, err := agentsLeft(h, workspaces, worktrees)
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
	for _, w := range worktrees {
		if exists(w.path) {
			left = append(left, "worktree "+w.path)
		}
	}
	return left, nil
}

// Close runs `close`.
func Close(h *herdr.Herdr, args CloseArgs) (exit.Code, error) {
	conn, worktrees, err := closeChecks(args)
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

	if err := removeWorktrees(h, workspaces, worktrees); err != nil {
		return 0, err
	}
	left, err := closeLeft(h, args.Job, workspaces, worktrees)
	if err != nil {
		return 0, err
	}
	if len(left) > 0 {
		return 0, exit.Environmentf("%d left after closing %s: %s", len(left), args.Job, strings.Join(left, ", "))
	}
	now := db.Now()
	if _, err := conn.Exec("UPDATE worktrees SET removed_at = ?1 WHERE job = ?2 AND removed_at IS NULL",
		now, args.Job); err != nil {
		return 0, exit.Database(err)
	}
	res, err := conn.Exec(
		"UPDATE agents SET state = 'ended', ended_at = ?1 WHERE job = ?2 AND state != 'ended'",
		now, args.Job)
	if err != nil {
		return 0, exit.Database(err)
	}
	ended, err := res.RowsAffected()
	if err != nil {
		return 0, exit.Database(err)
	}
	fmt.Fprintf(os.Stdout, "closed job %s: 0 left, %d rows ended\n", args.Job, ended)
	return exit.Ok, nil
}

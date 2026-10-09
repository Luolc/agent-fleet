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
	CloseAbout     = "Clean up a job after its PR is merged: workspace, worktree, branch, ledger rows"
	CloseLongAbout = "Clean up a job after its PR is merged: workspace, worktree, branch, ledger rows.\n\n" +
		"Called by the orchestra. Refused while the job still has live rows unless --force is " +
		"given. Closes the job's workspace, then checks that no agent in herdr has its cwd " +
		"inside the job's worktree; only then removes the worktree with `git worktree remove` " +
		"(which refuses uncommitted changes) and deletes its local branch. Finally counts what " +
		"is left: workspaces named after the job, agents in them or in the worktree, and the " +
		"worktree path. Success only when that count is 0; the job's rows are then marked " +
		"ended. Parts already gone are skipped, so it can be run again.\n\n" +
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
// `worktree`.
func agentsLeft(h *herdr.Herdr, workspaces []string, worktree string) ([]string, error) {
	agents, err := HerdrAgentList(h)
	if err != nil {
		return nil, err
	}
	left := []string{}
	for _, entry := range agents {
		agent, _ := entry.(map[string]any)
		id, ok := agent["workspace_id"].(string)
		if ok && slices.Contains(workspaces, id) || inside(agent, worktree) {
			left = append(left, describe(agent))
		}
	}
	return left, nil
}

// closeChecks refuses a caller that is not the orchestra, a bad job name
// and live rows without --force, and finds the job's worktree: the last
// one recorded, else `$HOME/wt/<repo>/<job>`. Returns the ledger, the
// worktree and `$HOME/dev/<repo>`.
func closeChecks(args CloseArgs) (conn *sql.DB, worktree, checkout string, err error) {
	me, err := identity.FromEnv()
	if err != nil {
		return nil, "", "", err
	}
	if me.Role != identity.Orchestra {
		return nil, "", "", exit.Refusedf("only the orchestra closes jobs")
	}
	if err := CheckName(args.Job); err != nil {
		return nil, "", "", err
	}
	repo, err := RepoName(me.Repo)
	if err != nil {
		return nil, "", "", err
	}
	home, err := Home()
	if err != nil {
		return nil, "", "", err
	}
	conn, err = db.Open(me.Repo)
	if err != nil {
		return nil, "", "", err
	}
	worktree, err = closeWorktree(conn, args, filepath.Join(home, "wt", repo, args.Job))
	if err != nil {
		conn.Close()
		return nil, "", "", err
	}
	return conn, worktree, filepath.Join(home, "dev", repo), nil
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

// Close runs `close`.
func Close(h *herdr.Herdr, args CloseArgs) (exit.Code, error) {
	conn, worktree, checkout, err := closeChecks(args)
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

	blocking, err := agentsLeft(h, workspaces, worktree)
	if err != nil {
		return 0, err
	}
	if len(blocking) > 0 {
		return 0, exit.Environmentf("not removing %s: still in use by %s", worktree, strings.Join(blocking, ", "))
	}
	if exists(worktree) {
		if err := removeWorktree(checkout, worktree); err != nil {
			return 0, err
		}
	}

	left, err := agentsLeft(h, workspaces, worktree)
	if err != nil {
		return 0, err
	}
	still, err := WorkspacesLabelled(h, args.Job)
	if err != nil {
		return 0, err
	}
	for _, id := range still {
		left = append(left, fmt.Sprintf("workspace %s (%s)", args.Job, id))
	}
	if exists(worktree) {
		left = append(left, "worktree "+worktree)
	}
	if len(left) > 0 {
		return 0, exit.Environmentf("%d left after closing %s: %s", len(left), args.Job, strings.Join(left, ", "))
	}
	res, err := conn.Exec(
		"UPDATE agents SET state = 'ended', ended_at = ?1 WHERE job = ?2 AND state != 'ended'",
		db.Now(), args.Job)
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

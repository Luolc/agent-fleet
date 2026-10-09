// `fleet worktree`: open a worktree for the caller's job.

package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/identity"
)

// WorktreeAbout and WorktreeLongAbout are the help texts of `worktree`.
const (
	WorktreeAbout     = "Open a worktree of ~/dev/<repo> for your job and print its path"
	WorktreeLongAbout = "Open a worktree of ~/dev/<repo> for your job and print its path.\n\n" +
		"The job is FLEET_JOB. The worktree is ~/wt/<repo>/<job>, or ~/wt/<repo>/<job>-<name> " +
		"with --name, on a new branch of the same name (no prefix) unless --branch is given. " +
		"~/dev/<repo> runs `git fetch origin` and the branch starts at origin/HEAD, never at " +
		"the local HEAD. The ledger of your scope records the worktree for the job, so " +
		"`fleet job end` removes it.\n\n" +
		"When the path already exists and the ledger records it for this job, the path is " +
		"printed and nothing changes, so every agent of a job gets the same worktree. A path " +
		"that exists otherwise is refused. stdout carries only the path.\n\n" +
		"Exit: 0 when the path is printed; 1 when FLEET_JOB is not set, <repo> has no checkout " +
		"in ~/dev, --name or --branch is invalid, the branch already exists, or the path exists " +
		"and is not this job's; 5 when git or the database fails."
)

// WorktreeArgs are the arguments of `worktree`.
type WorktreeArgs struct {
	// Repo is the checkout's directory name under ~/dev.
	Repo string
	// Name, when set, is appended to the job: `<job>-<name>`.
	Name *string
	// Branch, when set, replaces the default branch `<job>[-<name>]`.
	Branch *string
}

// worktreeChecks refuses what can be refused without git or the ledger, and
// returns the caller, the checkout, the worktree path and the branch.
func worktreeChecks(args WorktreeArgs) (me *identity.Identity, checkout, path, branch string, err error) {
	if me, err = identity.FromEnv(); err != nil {
		return nil, "", "", "", err
	}
	if me.Job == "" {
		return nil, "", "", "", exit.Refusedf("FLEET_JOB is not set: a worktree belongs to a job")
	}
	if err := CheckName(me.Job); err != nil {
		return nil, "", "", "", err
	}
	if err := CheckRepo(args.Repo); err != nil {
		return nil, "", "", "", err
	}
	leaf := me.Job
	if args.Name != nil {
		if err := CheckName(*args.Name); err != nil {
			return nil, "", "", "", err
		}
		leaf += "-" + *args.Name
	}
	branch = leaf
	if args.Branch != nil {
		branch = *args.Branch
	}
	home, err := Home()
	if err != nil {
		return nil, "", "", "", err
	}
	checkout = filepath.Join(home, "dev", args.Repo)
	if info, err := os.Stat(checkout); err != nil || !info.IsDir() {
		return nil, "", "", "", exit.Refusedf("no checkout at %s", checkout)
	}
	return me, checkout, filepath.Join(home, "wt", args.Repo, leaf), branch, nil
}

// worktreeOwner is the job the ledger records for `path`, or "" when none.
func worktreeOwner(conn *sql.DB, path string) (string, error) {
	var job string
	err := conn.QueryRow("SELECT job FROM worktrees WHERE path = ?1 AND removed_at IS NULL", path).Scan(&job)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", exit.Database(err)
	}
	return job, nil
}

// Worktree runs `worktree`.
func Worktree(args WorktreeArgs) (exit.Code, error) {
	me, checkout, path, branch, err := worktreeChecks(args)
	if err != nil {
		return 0, err
	}
	conn, err := db.Open(me.Scope)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	owner, err := worktreeOwner(conn, path)
	if err != nil {
		return 0, err
	}
	if exists(path) {
		if owner != me.Job {
			if owner == "" {
				return 0, exit.Refusedf("%s exists and the ledger does not record it for a job", path)
			}
			return 0, exit.Refusedf("%s belongs to job %s", path, owner)
		}
		fmt.Fprintln(os.Stdout, path)
		return exit.Ok, nil
	}
	if _, err := Git("check-ref-format", "--branch", branch); err != nil {
		return 0, exit.Refusedf("%q is not a valid branch name", branch)
	}
	if _, err := Git("-C", checkout, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		return 0, exit.Refusedf("branch %s already exists in %s", branch, checkout)
	}
	base, err := originHead(checkout)
	if err != nil {
		return 0, err
	}
	// A row for a path that is gone is stale: the worktree was removed
	// outside fleet.
	if _, err := conn.Exec("UPDATE worktrees SET removed_at = ?1 WHERE path = ?2 AND removed_at IS NULL",
		db.Now(), path); err != nil {
		return 0, exit.Database(err)
	}
	// The row goes in first, so a worktree is never on disk without it.
	res, err := conn.Exec(
		"INSERT INTO worktrees (path, repo, branch, job, created_by, created_at) VALUES (?1, ?2, ?3, ?4, ?5, ?6)",
		path, args.Repo, branch, me.Job, me.Agent, db.Now())
	if err != nil {
		return 0, exit.Database(err)
	}
	if _, err := Git("-C", checkout, "worktree", "add", path, "--no-track", "-b", branch, base); err != nil {
		if id, idErr := res.LastInsertId(); idErr == nil {
			_, _ = conn.Exec("DELETE FROM worktrees WHERE id = ?1", id)
		}
		return 0, err
	}
	fmt.Fprintln(os.Stdout, path)
	return exit.Ok, nil
}

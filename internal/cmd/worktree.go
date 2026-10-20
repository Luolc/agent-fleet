// `fleet worktree`: open a worktree for the caller's job.

package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/identity"
)

// WorktreeAbout and WorktreeLongAbout are the help texts of `worktree`.
const (
	WorktreeAbout     = "Open a worktree of a checkout for your job and print its path"
	WorktreeLongAbout = "Open a worktree of a checkout for your job and print its path.\n\n" +
		"The checkout is <repo>'s main checkout under the scope's `paths.checkouts` (default " +
		"~/dev); when there is none and <repo> is an initiative's repo, <initiative " +
		"prefix><I> (`channels.initiative_prefix`, default x-repo-), the initiative's " +
		"checkout <I> under `paths.initiatives` (default ~/x-repo). These settings are in " +
		"$XDG_CONFIG_HOME/fleet/<scope>.json (see `fleet inbox --help`).\n\n" +
		"The job is FLEET_JOB. The worktree is <repo>/<job>, or <repo>/<job>-<name> with " +
		"--name, under `paths.worktrees` (default ~/wt), on a new branch of the same name (no " +
		"prefix) unless --branch is given. The checkout runs `git fetch origin` and the branch " +
		"starts at origin/HEAD, never at the local HEAD. With --detach <ref> the worktree is " +
		"a detached checkout of <ref> (a commit, such as a PR's head SHA, resolved in the " +
		"checkout after the fetch) on no branch, for reviewing or testing it; --detach and " +
		"--branch exclude each other. The ledger of your scope records the worktree for the " +
		"job, so `fleet job end` removes it (and its branch, when it has one). Use it rather " +
		"than `git worktree add`, which fleet would not know to remove.\n\n" +
		"When the path already exists and the ledger records it for this job, the path is " +
		"printed and nothing else changes (except statute's retry, below), so every agent of a " +
		"job gets the same worktree. A path " +
		"that exists otherwise is refused. stdout carries only the path.\n\n" +
		"When the new worktree's root has statute.toml, `statute upgrade --private` runs " +
		"there, to fetch the private sources and lock them for this worktree alone, then " +
		"`statute compile --private-only`, to generate the private output the checkout does " +
		"not track; neither touches tracked files, and their output goes to stderr. Each is " +
		"killed with what it started when still running after 60 s. When statute is not on " +
		"PATH, or the compile fails or is killed, the private output is missing: the worktree " +
		"stays, its path is still printed, stderr says why and how to generate it, the exit is " +
		"6, and the ledger records the failure, so `fleet spawn` tells a worker started in the " +
		"worktree. A failed upgrade alone is a warning on stderr: the compile still runs, on the " +
		"private lock as it is. When the same job calls again and the ledger records a failure " +
		"for the path, both commands run again and the ledger takes the new result, so every " +
		"agent that shares the worktree learns of a failure that remains.\n\n" +
		"Exit: 0 when the path is printed; 6 when the path is printed but statute did not " +
		"generate the private output (above); 1 when FLEET_JOB is not set, <repo> has no " +
		"checkout, the scope config is invalid, --name or --branch is invalid, --detach comes " +
		"with --branch or names no commit of the checkout, the branch already exists, or the " +
		"path exists and is not this job's; 5 when git or the database fails."
)

// WorktreeArgs are the arguments of `worktree`.
type WorktreeArgs struct {
	// Repo is the checkout's directory name, or an initiative's repo.
	Repo string
	// Name, when set, is appended to the job: `<job>-<name>`.
	Name *string
	// Branch, when set, replaces the default branch `<job>[-<name>]`.
	Branch *string
	// Detach, when set, is the commit to check out detached: no branch.
	Detach *string
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
	if args.Detach != nil {
		if args.Branch != nil {
			return nil, "", "", "", exit.Refusedf("--detach and --branch exclude each other: a detached checkout has no branch")
		}
		if strings.TrimSpace(*args.Detach) == "" {
			return nil, "", "", "", exit.Refusedf("--detach names no commit")
		}
		branch = ""
	}
	sc, err := config.LoadScope(me.Scope)
	if err != nil {
		return nil, "", "", "", err
	}
	if checkout, err = sourceCheckout(sc, args.Repo); err != nil {
		return nil, "", "", "", err
	}
	return me, checkout, filepath.Join(sc.Paths.Worktrees, args.Repo, leaf), branch, nil
}

// sourceCheckout is the checkout `repo` names: its main checkout under
// `paths.checkouts`, or when there is none and `repo` is an initiative's
// repo `<initiative prefix><I>`, the initiative's checkout under
// `paths.initiatives`.
func sourceCheckout(sc *config.Scope, repo string) (string, error) {
	tried := []string{filepath.Join(sc.Paths.Checkouts, repo)}
	if rest, ok := strings.CutPrefix(repo, sc.Channels.InitiativePrefix); ok && CheckRepo(rest) == nil {
		tried = append(tried, filepath.Join(sc.Paths.Initiatives, rest))
	}
	for _, checkout := range tried {
		if info, err := os.Stat(checkout); err == nil && info.IsDir() {
			return checkout, nil
		}
	}
	return "", exit.Refusedf("no checkout at %s", strings.Join(tried, " or "))
}

// worktreeAdd is the `git worktree add` that opens `path` from `checkout`:
// detached at `*detach`, or on the new branch `branch` from origin/HEAD.
func worktreeAdd(detach *string, checkout, path, branch string) ([]string, error) {
	if detach != nil {
		base, err := resolveCommit(checkout, *detach)
		if err != nil {
			return nil, err
		}
		return []string{"-C", checkout, "worktree", "add", "--detach", path, base}, nil
	}
	// Only a git that ran and exited non-zero judged the name; any other
	// failure means git cannot run.
	var exitErr *exec.ExitError
	if err := exec.Command("git", "check-ref-format", "--branch", branch).Run(); errors.As(err, &exitErr) {
		return nil, exit.Refusedf("%q is not a valid branch name", branch)
	} else if err != nil {
		return nil, exit.Environmentf("cannot run git: %v", err)
	}
	if _, err := Git("-C", checkout, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		return nil, exit.Refusedf("branch %s already exists in %s", branch, checkout)
	}
	base, err := originHead(checkout)
	if err != nil {
		return nil, err
	}
	return []string{"-C", checkout, "worktree", "add", path, "--no-track", "-b", branch, base}, nil
}

// resolveCommit is the commit `ref` names in `checkout` after `git fetch
// origin`, so a PR's head on origin resolves; a ref that names no commit
// is refused.
func resolveCommit(checkout, ref string) (string, error) {
	if _, err := Git("-C", checkout, "fetch", "--quiet", "origin"); err != nil {
		return "", err
	}
	sha, err := Git("-C", checkout, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", exit.Refusedf("%q names no commit in %s (after `git fetch origin`)", ref, checkout)
	}
	return strings.TrimSpace(sha), nil
}

// worktreeOwner is the job the ledger records for `path` and the statute
// failure recorded with it, or "" for both when none.
func worktreeOwner(conn *sql.DB, path string) (job, failure string, err error) {
	err = conn.QueryRow("SELECT job, statute_failure FROM worktrees WHERE path = ?1 AND removed_at IS NULL", path).
		Scan(&job, &failure)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", exit.Database(err)
	}
	return job, failure, nil
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
	owner, failure, err := worktreeOwner(conn, path)
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
		// A recorded failure is retried, so a fix since then clears it and
		// an agent that shares the worktree learns of one that remains.
		if failure != "" {
			failure = refreshStatute(path)
		}
		return statuteOutcome(conn, path, failure)
	}
	add, err := worktreeAdd(args.Detach, checkout, path, branch)
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
	if _, err := Git(add...); err != nil {
		if id, idErr := res.LastInsertId(); idErr == nil {
			_, _ = conn.Exec("DELETE FROM worktrees WHERE id = ?1", id)
		}
		return 0, err
	}
	return statuteOutcome(conn, path, refreshStatute(path))
}

// statuteOutcome records `failure` for the worktree at `path`, prints the
// path, and, when statute failed, says so on stderr and exits 6.
func statuteOutcome(conn *sql.DB, path, failure string) (exit.Code, error) {
	if _, err := conn.Exec("UPDATE worktrees SET statute_failure = ?1 WHERE path = ?2 AND removed_at IS NULL",
		failure, path); err != nil {
		return 0, exit.Database(err)
	}
	fmt.Fprintln(os.Stdout, path)
	if failure == "" {
		return exit.Ok, nil
	}
	fmt.Fprintf(os.Stderr, "fleet: %s is ready, but statute did not generate its private output: %s; "+
		"the agents in it lack the private rules until you %s\n", path, failure, statuteRepair(path))
	return exit.Partial, nil
}

// statuteTimeout bounds each statute command; a hung one must not hold up
// the worktree or the start. A variable so the test can shorten it.
var statuteTimeout = 60 * time.Second

// refreshStatute runs, in `dir` when its root has statute.toml, `statute
// upgrade --private`, which fetches the private sources and writes only
// the lock in the worktree's own git directory, then `statute compile
// --private-only`: the private output statute writes is not tracked, so a
// fresh checkout lacks it, a checkout's private lock goes stale unless
// something fetches, and the full commands would rewrite tracked files.
// It returns why the private output was not generated: statute is not on
// PATH, or the compile failed or timed out (after the upgrade's failure,
// when that failed too); "" when it was, or when `dir` has no
// statute.toml. A failed upgrade alone only warns, since the compile still
// generates the output from the lock as it is. Their output goes to
// stderr: stdout carries the path.
func refreshStatute(dir string) string {
	if !exists(filepath.Join(dir, "statute.toml")) {
		return ""
	}
	bin, err := exec.LookPath("statute")
	if err != nil {
		return "statute.toml is there but statute is not on PATH"
	}
	upgrade := runStatute(bin, dir, "upgrade", "--private")
	compile := runStatute(bin, dir, "compile", "--private-only")
	switch {
	case compile == "":
		if upgrade != "" {
			fmt.Fprintf(os.Stderr, "fleet: warning: %s in %s; the compile ran on the private lock as it was\n", upgrade, dir)
		}
		return ""
	case upgrade != "":
		return upgrade + ", then " + compile
	}
	return compile
}

// runStatute runs `statute <args>` in `dir` within statuteTimeout; how it
// failed or timed out, or "" when it exited 0.
func runStatute(bin, dir string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), statuteTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	// Its own process group, killed whole, so nothing it started outlives
	// the deadline.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	err := cmd.Run()
	name := "`statute " + strings.Join(args, " ") + "`"
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return fmt.Sprintf("%s timed out after %v and was killed", name, statuteTimeout)
	case errors.As(err, &exitErr):
		return fmt.Sprintf("%s failed (exit %d)", name, exitErr.ExitCode())
	case err != nil:
		return fmt.Sprintf("%s could not run: %v", name, err)
	}
	return ""
}

// statuteRepair is how an agent in `dir` makes the private output itself.
func statuteRepair(dir string) string {
	return "run `statute upgrade --private && statute compile --private-only` in " + dir
}

// statuteNote is what an agent starting in or under `dir` is told when
// statute did not generate the private output there.
func statuteNote(dir, failure string) string {
	return fmt.Sprintf("statute did not generate the private output of %s when fleet last ran it there: %s. "+
		"The instructions you read there lack the repo's private rules: before anything else, %s, and when "+
		"that fails too, say so to whoever you report to before you go on.", dir, failure, statuteRepair(dir))
}

// worktreeStatuteNote is statuteNote for the live worktree that holds
// `cwd` when the ledger records a statute failure for it; "" when none
// does.
func worktreeStatuteNote(conn *sql.DB, cwd string) (string, error) {
	rows, err := conn.Query("SELECT path, statute_failure FROM worktrees WHERE removed_at IS NULL AND statute_failure != ''")
	if err != nil {
		return "", exit.Database(err)
	}
	defer rows.Close()
	for rows.Next() {
		var path, failure string
		if err := rows.Scan(&path, &failure); err != nil {
			return "", exit.Database(err)
		}
		if pathStartsWith(cwd, path) {
			return statuteNote(path, failure), nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", exit.Database(err)
	}
	return "", nil
}

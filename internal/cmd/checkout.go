// Bringing a main checkout up to origin before a thread agent or a lead
// starts in it, so it reads the current AGENTS.md, charter and docs.

package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// fetchTimeout bounds `git fetch` in a start: a hung remote must not hold
// up the hook, which has the agent to start after it.
var fetchTimeout = 30 * time.Second

// syncCheckout fast-forwards the main checkout `dir` to origin, as a
// person would before working in it: `git fetch origin`, then, when the
// checkout is on origin/HEAD's branch, has no changes to tracked files and
// is only behind, `git merge --ff-only`, which leaves an untracked or
// ignored file alone. It never stops a start: anything
// else leaves the checkout as it is, and the returned sentence says why,
// for the agent's first message; "" when the checkout is up to date.
// Either way it then refreshes statute's private lock and output, which
// are not tracked, so a checkout left as it is still gets current private
// rules.
func syncCheckout(dir string) string {
	reason := fastForward(dir)
	refreshStatute(dir)
	if reason != "" {
		fmt.Fprintf(os.Stderr, "note: %s was not updated from origin: %s\n", dir, reason)
		return fmt.Sprintf("The checkout %s was not updated from origin before you started: %s. What you read there "+
			"(AGENTS.md, a charter, the docs) may be behind origin; leave the checkout itself as it is.", dir, reason)
	}
	return ""
}

// fastForward does syncCheckout's git steps; the reason it stopped, or ""
// when the checkout is now at origin. git's own messages are left out of
// the reason: a fetch error can carry the remote's URL with its token.
func fastForward(dir string) string {
	if err := gitFetch(dir); err != nil {
		return err.Error()
	}
	upstream, err := Git("-C", dir, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		return "origin/HEAD is not set"
	}
	upstream = strings.TrimSpace(upstream)
	branch, err := Git("-C", dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "HEAD is detached"
	}
	if branch = strings.TrimSpace(branch); "origin/"+branch != upstream {
		return fmt.Sprintf("it is on %s, not on %s's branch", branch, upstream)
	}
	if status, err := Git("-C", dir, "status", "--porcelain", "--untracked-files=no"); err != nil || status != "" {
		return "it has local changes"
	}
	counts, err := Git("-C", dir, "rev-list", "--left-right", "--count", "HEAD..."+upstream)
	fields := strings.Fields(counts)
	if err != nil || len(fields) != 2 {
		return "git rev-list could not compare it with " + upstream
	}
	ahead, _ := strconv.Atoi(fields[0])
	behind, _ := strconv.Atoi(fields[1])
	switch {
	case ahead > 0 && behind > 0:
		return fmt.Sprintf("it has diverged from %s (ahead by %d, behind by %d)", upstream, ahead, behind)
	case ahead > 0:
		return fmt.Sprintf("it is ahead of %s by %d", upstream, ahead)
	case behind == 0:
		return ""
	}
	// git counts ignored files as expendable; a local file the update
	// would track stops it instead.
	if _, err := Git("-C", dir, "merge", "--ff-only", "--no-overwrite-ignore", "--quiet", upstream); err != nil {
		return "`git merge --ff-only` failed, as it does when a file it would bring in is there untracked or ignored"
	}
	fmt.Fprintf(os.Stderr, "note: fast-forwarded %s to %s, %d behind before\n", dir, upstream, behind)
	return ""
}

// gitFetch is `git -C <dir> fetch --quiet origin` within fetchTimeout,
// never asking for a password.
func gitFetch(dir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "fetch", "--quiet", "origin")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("`git fetch origin` timed out after %v", fetchTimeout)
	case errors.As(err, &exitErr):
		return fmt.Errorf("`git fetch origin` failed (exit %d)", exitErr.ExitCode())
	case err != nil:
		return fmt.Errorf("cannot run git: %v", err)
	}
	return nil
}

// noteSection is `## Note from fleet` with the notes that are not empty,
// one paragraph each, for an agent's first message; "" when there are
// none.
func noteSection(notes ...string) string {
	var kept []string
	for _, n := range notes {
		if n != "" {
			kept = append(kept, n)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return "\n## Note from fleet\n\n" + strings.Join(kept, "\n\n") + "\n"
}

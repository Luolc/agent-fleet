// Integration tests for `worktree` over the binary against real git: a
// bare origin under the test's temp dir and its clone at ~/dev, and for
// `job end --force` a fake herdr with no workspaces.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const dataset = "example-dataset"

// git runs git with a fixed identity and no user config, failing the test
// on error. Returns stdout, trimmed.
func (w *world) git(args ...string) string {
	w.t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.test"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.Output()
	if err != nil {
		w.t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// origin makes a bare origin with one commit, clones it to
// ~/dev/example-dataset, then pushes a second commit the clone has not
// fetched. Returns the clone and the seed that pushes to origin.
func (w *world) origin() (checkout, seed string) {
	w.t.Helper()
	bare := filepath.Join(w.dir, "origin.git")
	seed = filepath.Join(w.dir, "seed")
	checkout = filepath.Join(w.dir, "home", "dev", dataset)
	w.git("init", "-q", "--bare", "-b", "main", bare)
	w.git("clone", "-q", bare, seed)
	w.git("-C", seed, "commit", "-q", "--allow-empty", "-m", "init")
	w.git("-C", seed, "push", "-q", "origin", "main")
	w.git("clone", "-q", bare, checkout)
	w.git("-C", seed, "commit", "-q", "--allow-empty", "-m", "newer on origin")
	w.git("-C", seed, "push", "-q", "origin", "main")
	return checkout, seed
}

// worktree runs `fleet worktree` as a worker of `job`.
func (w *world) worktree(job string, args ...string) result {
	w.t.Helper()
	return w.asAgent(job+"-a", "worker", job+"-lead", job, append([]string{"worktree"}, args...)...)
}

// worktreeRows are the ledger's worktree rows, one string per row.
func (w *world) worktreeRows() []string {
	w.t.Helper()
	conn := w.ledger()
	defer conn.Close()
	rows, err := conn.Query("SELECT path, repo, branch, job, created_by, removed_at IS NULL, " +
		"abs(created_at - strftime('%s', 'now')) < 120 FROM worktrees ORDER BY id")
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var path, repo, branch, job, by string
		var live, recent bool
		if err := rows.Scan(&path, &repo, &branch, &job, &by, &live, &recent); err != nil {
			w.t.Fatal(err)
		}
		state := "live"
		if !live {
			state = "removed"
		}
		if !recent {
			state += " old"
		}
		got = append(got, strings.Join([]string{path, repo, branch, job, by, state}, "|"))
	}
	return got
}

func TestWorktreeBranchesFromOriginAndRecordsTheJob(t *testing.T) {
	w := newWorld(t)
	checkout, seed := w.origin()
	stale := w.git("-C", checkout, "rev-parse", "HEAD")
	path := filepath.Join(w.dir, "home", "wt", dataset, "item-1")

	out := w.worktree("item-1", dataset)
	if out.code != 0 || out.stdout != path+"\n" {
		t.Fatalf("%+v", out)
	}
	if got := w.git("-C", path, "branch", "--show-current"); got != "item-1" {
		t.Errorf("branch = %s", got)
	}
	head := w.git("-C", path, "rev-parse", "HEAD")
	if want := w.git("-C", seed, "rev-parse", "HEAD"); head != want || head == stale {
		t.Errorf("HEAD = %s, want origin's %s, not the stale %s", head, want, stale)
	}
	if upstream := w.git("-C", path, "for-each-ref", "--format=%(upstream)", "refs/heads/item-1"); upstream != "" {
		t.Errorf("branch tracks %s", upstream)
	}
	want := []string{path + "|" + dataset + "|item-1|item-1|item-1-a|live"}
	if got := w.worktreeRows(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rows = %q, want %q", got, want)
	}
}

func TestWorktreeNameAndBranch(t *testing.T) {
	w := newWorld(t)
	w.origin()
	wt := filepath.Join(w.dir, "home", "wt", dataset)

	out := w.worktree("item-1", dataset, "--name", "review")
	if out.code != 0 || out.stdout != filepath.Join(wt, "item-1-review")+"\n" {
		t.Fatalf("%+v", out)
	}
	if got := w.git("-C", filepath.Join(wt, "item-1-review"), "branch", "--show-current"); got != "item-1-review" {
		t.Errorf("--name branch = %s", got)
	}
	out = w.worktree("item-1", dataset, "--name", "fix", "--branch", "fix/foo")
	if out.code != 0 || out.stdout != filepath.Join(wt, "item-1-fix")+"\n" {
		t.Fatalf("%+v", out)
	}
	if got := w.git("-C", filepath.Join(wt, "item-1-fix"), "branch", "--show-current"); got != "fix/foo" {
		t.Errorf("--branch branch = %s", got)
	}
}

func TestWorktreeDetachChecksOutTheCommitOnNoBranch(t *testing.T) {
	w := newWorld(t)
	checkout, seed := w.origin()
	// The commit is on origin only: the checkout has not fetched it.
	w.git("-C", seed, "commit", "-q", "--allow-empty", "-m", "a PR head")
	w.git("-C", seed, "push", "-q", "origin", "main:refs/heads/pr-head")
	head := w.git("-C", seed, "rev-parse", "HEAD")
	path := filepath.Join(w.dir, "home", "wt", dataset, "item-1-review")

	out := w.worktree("item-1", dataset, "--name", "review", "--detach", head)
	if out.code != 0 || out.stdout != path+"\n" {
		t.Fatalf("%+v", out)
	}
	if got := w.git("-C", path, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD = %s, want %s", got, head)
	}
	if got := w.git("-C", path, "branch", "--show-current"); got != "" {
		t.Errorf("on branch %q, want detached", got)
	}
	if got := w.git("-C", checkout, "branch", "--list"); got != "* main" {
		t.Errorf("branches = %q", got)
	}
	want := []string{path + "|" + dataset + "||item-1|item-1-a|live"}
	if got := w.worktreeRows(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rows = %q, want %q", got, want)
	}
	// Again: the same path, nothing changed; a job's checkout by ref name too.
	if out := w.worktree("item-1", dataset, "--name", "review", "--detach", head); out.code != 0 || out.stdout != path+"\n" {
		t.Errorf("again: %+v", out)
	}
	if out := w.worktree("item-1", dataset, "--name", "audit", "--detach", "origin/pr-head"); out.code != 0 {
		t.Errorf("by ref: %+v", out)
	}
	for _, c := range []struct {
		label  string
		needle string
		args   []string
	}{
		{"--detach with --branch", "exclude each other", []string{"--name", "x", "--detach", head, "--branch", "fix/x"}},
		{"an empty --detach", "names no commit", []string{"--name", "x", "--detach", " "}},
		{"a ref that is not there", "names no commit", []string{"--name", "x", "--detach", "no-such-ref"}},
	} {
		out := w.worktree("item-1", append([]string{dataset}, c.args...)...)
		if out.code != 1 || !strings.Contains(out.stderr, c.needle) {
			t.Errorf("%s: %+v", c.label, out)
		}
	}
	if got := w.worktreeRows(); len(got) != 2 {
		t.Errorf("rows after refusals = %q", got)
	}
	// job end --force removes it like any other, with no branch to delete.
	w.closeHerdr("")
	if out := w.asThread("job", "end", "item-1", "--force"); out.code != 0 {
		t.Fatalf("job end: %+v", out)
	}
	if _, err := os.Stat(path); err == nil {
		t.Errorf("%s still exists", path)
	}
}

func TestWorktreeOfAnInitiativeCheckoutUnderConfiguredRoots(t *testing.T) {
	w := newWorld(t)
	initiatives, trees := filepath.Join(w.dir, "multi"), filepath.Join(w.dir, "trees")
	w.scopeConfig(scope, `{"paths": {"initiatives": "`+initiatives+`", "worktrees": "`+trees+`"},
		"channels": {"initiative_prefix": "multi-"}}`)
	bare, seed, checkout := filepath.Join(w.dir, "init.git"), filepath.Join(w.dir, "seed"), filepath.Join(initiatives, "example-init")
	w.git("init", "-q", "--bare", "-b", "main", bare)
	w.git("clone", "-q", bare, seed)
	w.git("-C", seed, "commit", "-q", "--allow-empty", "-m", "charter")
	w.git("-C", seed, "push", "-q", "origin", "main")
	// A separate git dir: the checkout has a `.git` file, not a directory.
	w.git("clone", "-q", "--separate-git-dir", filepath.Join(w.dir, "init-git"), bare, checkout)
	head := w.git("-C", seed, "rev-parse", "HEAD")
	wt := filepath.Join(trees, "multi-example-init")

	out := w.worktree("item-1", "multi-example-init", "--branch", "docs/charter")
	if out.code != 0 || out.stdout != filepath.Join(wt, "item-1")+"\n" {
		t.Fatalf("%+v", out)
	}
	if got := w.git("-C", filepath.Join(wt, "item-1"), "branch", "--show-current"); got != "docs/charter" {
		t.Errorf("branch = %s", got)
	}
	out = w.worktree("item-1", "multi-example-init", "--name", "review", "--detach", head)
	if out.code != 0 || w.git("-C", filepath.Join(wt, "item-1-review"), "rev-parse", "HEAD") != head {
		t.Fatalf("--detach: %+v", out)
	}
	out = w.worktree("item-1", "multi-no-such")
	if out.code != 1 || !strings.Contains(out.stderr, "no checkout at "+filepath.Join(w.dir, "home", "dev", "multi-no-such")+
		" or "+filepath.Join(initiatives, "no-such")) {
		t.Errorf("no checkout: %+v", out)
	}

	w.closeHerdr("")
	if out := w.asThread("job", "end", "item-1", "--force"); out.code != 0 {
		t.Fatalf("job end: %+v", out)
	}
	for _, leaf := range []string{"item-1", "item-1-review"} {
		if _, err := os.Stat(filepath.Join(wt, leaf)); err == nil {
			t.Errorf("%s not removed", leaf)
		}
	}
	if got := w.git("-C", checkout, "branch", "--list", "docs/charter"); got != "" {
		t.Errorf("branch docs/charter not deleted")
	}
}

func TestWorktreeAgainInTheSameJobReturnsThePathAndChangesNothing(t *testing.T) {
	w := newWorld(t)
	_, seed := w.origin()
	path := filepath.Join(w.dir, "home", "wt", dataset, "item-1")
	if out := w.worktree("item-1", dataset); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	head := w.git("-C", path, "rev-parse", "HEAD")
	w.git("-C", seed, "commit", "-q", "--allow-empty", "-m", "newer still")
	w.git("-C", seed, "push", "-q", "origin", "main")

	out := w.asAgent("item-1-lead", "lead", "thread-1", "item-1", "worktree", dataset)
	if out.code != 0 || out.stdout != path+"\n" {
		t.Fatalf("%+v", out)
	}
	if got := w.git("-C", path, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD moved from %s to %s", head, got)
	}
	if rows := w.worktreeRows(); len(rows) != 1 {
		t.Errorf("rows = %q", rows)
	}
}

func TestWorktreeRefusals(t *testing.T) {
	w := newWorld(t)
	checkout, _ := w.origin()
	wt := filepath.Join(w.dir, "home", "wt", dataset)
	if out := w.worktree("item-1", dataset, "--name", "a"); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if err := os.MkdirAll(filepath.Join(wt, "item-2"), 0o755); err != nil {
		t.Fatal(err)
	}
	w.git("-C", checkout, "branch", "item-3")

	cases := []struct {
		job  string
		args []string
		says string
	}{
		{"", []string{dataset}, "FLEET_JOB"},
		{"job/other", []string{dataset}, "[a-z0-9-]"},
		{"..", []string{dataset}, "[a-z0-9-]"},
		// item-1 --name a made ~/wt/<repo>/item-1-a.
		{"item-1-a", []string{dataset}, "belongs to job item-1"},
		{"item-2", []string{dataset}, "does not record it"},
		{"item-3", []string{dataset}, "already exists"},
		{"item-4", []string{"no-such-repo"}, "no checkout"},
		{"item-4", []string{"../" + dataset}, "directory name"},
		{"item-4", []string{dataset, "--name", "Bad_Name"}, "[a-z0-9-]"},
		{"item-4", []string{dataset, "--branch", "bad..branch"}, "not a valid branch"},
	}
	for _, c := range cases {
		out := w.asAgent("x-a", "worker", "x-lead", c.job, append([]string{"worktree"}, c.args...)...)
		if out.code != 1 || out.stdout != "" || !strings.Contains(out.stderr, c.says) {
			t.Errorf("%s %v: %+v, want exit 1 saying %q", c.job, c.args, out, c.says)
		}
	}
	if rows := w.worktreeRows(); len(rows) != 1 {
		t.Errorf("a refusal wrote a row: %q", rows)
	}
	for _, leaf := range []string{"item-4", "job"} {
		if _, err := os.Stat(filepath.Join(wt, leaf)); err == nil {
			t.Errorf("a refusal made %s", leaf)
		}
	}
}

func TestWorktreeFailsWith5WhenGitFails(t *testing.T) {
	w := newWorld(t)
	checkout, _ := w.origin()
	w.git("-C", checkout, "remote", "set-url", "origin", filepath.Join(w.dir, "gone.git"))
	out := w.worktree("item-1", dataset)
	if out.code != 5 || out.stdout != "" {
		t.Errorf("%+v", out)
	}
	if rows := w.worktreeRows(); len(rows) != 0 {
		t.Errorf("rows = %q", rows)
	}
}

func TestWorktreeSaysGitCannotRunWhenGitIsMissing(t *testing.T) {
	w := newWorld(t)
	w.origin()
	// The last PATH wins: one with no git on it.
	out := w.run("", []string{"worktree", dataset, "--branch", "fix/foo"},
		"FLEET_AGENT=item-1-a", "FLEET_ROLE=worker", "FLEET_PARENT=item-1-lead", "FLEET_SCOPE="+scope,
		"FLEET_JOB=item-1", "PATH="+t.TempDir())
	if out.code != 5 || out.stdout != "" || !strings.Contains(out.stderr, "cannot run git") {
		t.Errorf("%+v, want exit 5 saying git cannot run", out)
	}
}

// closeHerdr is a fake herdr with no workspaces and the given agents.
func (w *world) closeHerdr(agents string) {
	w.t.Helper()
	script := `#!/bin/sh
if [ "$1" = --session ]; then shift 2; fi
case "$1 $2" in
  "workspace list") echo '{"id":"cli:workspace:list","result":{"workspaces":[]}}' ;;
  "agent list") echo '{"id":"cli:agent:list","result":{"agents":[` + agents + `]}}' ;;
  *) echo "fake herdr: unexpected command: $*" >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(w.dir, "fake-herdr", "herdr"), []byte(script), 0o755); err != nil {
		w.t.Fatal(err)
	}
}

func TestJobEndForceRemovesTheJobsRecordedWorktreesWithTheSameChecks(t *testing.T) {
	w := newWorld(t)
	checkout, _ := w.origin()
	wt := filepath.Join(w.dir, "home", "wt", dataset)
	for _, args := range [][]string{{dataset}, {dataset, "--name", "b"}} {
		if out := w.worktree("item-1", args...); out.code != 0 {
			t.Fatalf("%+v", out)
		}
	}
	if out := w.worktree("item-2", dataset); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	close := func() result { return w.asThread("job", "end", "item-1", "--force") }

	// An agent's cwd inside the second worktree blocks both removals.
	w.closeHerdr(`{"name":"squatter","pane_id":"p1","cwd":"` + filepath.Join(wt, "item-1-b", "sub") + `"}`)
	out := close()
	if out.code != 5 || !strings.Contains(out.stderr, "agent squatter") {
		t.Errorf("%+v", out)
	}
	for _, leaf := range []string{"item-1", "item-1-b"} {
		if _, err := os.Stat(filepath.Join(wt, leaf)); err != nil {
			t.Errorf("%s removed while in use", leaf)
		}
	}

	// Uncommitted changes make `git worktree remove` refuse.
	w.closeHerdr("")
	dirty := filepath.Join(wt, "item-1-b", "notes.txt")
	if err := os.WriteFile(dirty, []byte("draft\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := close(); out.code != 5 {
		t.Errorf("%+v", out)
	}
	if _, err := os.Stat(dirty); err != nil {
		t.Error("uncommitted change lost")
	}
	if err := os.Remove(dirty); err != nil {
		t.Fatal(err)
	}

	out = close()
	if out.code != 0 || !strings.Contains(out.stdout, "0 left") {
		t.Fatalf("%+v", out)
	}
	for _, leaf := range []string{"item-1", "item-1-b"} {
		if _, err := os.Stat(filepath.Join(wt, leaf)); err == nil {
			t.Errorf("%s not removed", leaf)
		}
		if got := w.git("-C", checkout, "branch", "--list", leaf); got != "" {
			t.Errorf("branch %s not deleted", leaf)
		}
	}
	if _, err := os.Stat(filepath.Join(wt, "item-2")); err != nil {
		t.Error("another job's worktree was removed")
	}
	want := []string{
		filepath.Join(wt, "item-1") + "|" + dataset + "|item-1|item-1|item-1-a|removed",
		filepath.Join(wt, "item-1-b") + "|" + dataset + "|item-1-b|item-1|item-1-a|removed",
		filepath.Join(wt, "item-2") + "|" + dataset + "|item-2|item-2|item-2-a|live",
	}
	if got := w.worktreeRows(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rows = %q, want %q", got, want)
	}
}

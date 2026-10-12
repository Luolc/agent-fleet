package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitIn runs git in `dir` with a fixed identity, failing the test on error.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes `name` in `dir` and commits it.
func commit(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", name)
	gitIn(t, dir, "commit", "--quiet", "-m", name)
}

// behindOrigin is a main checkout cloned from a bare origin, which then
// got one commit more from another clone: the checkout is one behind.
func behindOrigin(t *testing.T) (checkout, other string) {
	t.Helper()
	root := t.TempDir()
	origin, checkout, other := filepath.Join(root, "origin.git"), filepath.Join(root, "dev"), filepath.Join(root, "other")
	gitIn(t, root, "init", "--quiet", "--bare", "--initial-branch=main", origin)
	gitIn(t, root, "clone", "--quiet", origin, other)
	commit(t, other, "a")
	gitIn(t, other, "push", "--quiet", "origin", "main")
	gitIn(t, root, "clone", "--quiet", origin, checkout)
	commit(t, other, "b")
	gitIn(t, other, "push", "--quiet", "origin", "main")
	return checkout, other
}

func TestSyncCheckoutFastForwardsOnlyACleanCheckoutThatIsBehind(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	for name, c := range map[string]struct {
		setup  func(t *testing.T, checkout string)
		reason string // "" when it is fast-forwarded
	}{
		"behind":   {func(*testing.T, string) {}, ""},
		"dirty":    {func(t *testing.T, d string) { _ = os.WriteFile(filepath.Join(d, "a"), []byte("x\n"), 0o644) }, "it has local changes"},
		"diverged": {func(t *testing.T, d string) { commit(t, d, "c") }, "it has diverged from origin/main (ahead by 1, behind by 1)"},
		"branch":   {func(t *testing.T, d string) { gitIn(t, d, "switch", "--quiet", "-c", "topic") }, "it is on topic, not on origin/main's branch"},
		"detached": {func(t *testing.T, d string) { gitIn(t, d, "switch", "--quiet", "--detach") }, "HEAD is detached"},
		"no fetch": {func(t *testing.T, d string) {
			gitIn(t, d, "remote", "set-url", "origin", filepath.Join(d, "missing.git"))
		},
			"`git fetch origin` failed (exit 128)"},
	} {
		checkout, _ := behindOrigin(t)
		before := gitIn(t, checkout, "rev-parse", "HEAD")
		c.setup(t, checkout)
		head := gitIn(t, checkout, "rev-parse", "HEAD")
		note := syncCheckout(checkout)
		after := gitIn(t, checkout, "rev-parse", "HEAD")
		if c.reason == "" {
			if note != "" || after == before || after != gitIn(t, checkout, "rev-parse", "origin/main") {
				t.Errorf("%s: note %q, HEAD %s -> %s", name, note, before, after)
			}
			continue
		}
		if !strings.Contains(note, "was not updated from origin before you started: "+c.reason+".") || after != head {
			t.Errorf("%s: note %q, HEAD %s -> %s", name, note, head, after)
		}
	}
}

func TestSyncCheckoutLeavesACheckoutThatIsUpToDateOrAhead(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	checkout, _ := behindOrigin(t)
	if note := syncCheckout(checkout); note != "" {
		t.Fatalf("behind: %q", note)
	}
	if note := syncCheckout(checkout); note != "" {
		t.Errorf("up to date: %q", note)
	}
	commit(t, checkout, "c")
	if note := syncCheckout(checkout); !strings.Contains(note, "it is ahead of origin/main by 1") {
		t.Errorf("ahead: %q", note)
	}
}

func TestNoteSectionKeepsOnlyTheNotesGiven(t *testing.T) {
	if got := noteSection("", ""); got != "" {
		t.Errorf("no notes: %q", got)
	}
	if got := noteSection("", "A.", "B."); got != "\n## Note from fleet\n\nA.\n\nB.\n" {
		t.Errorf("two notes: %q", got)
	}
}

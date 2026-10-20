package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
		setup  func(t *testing.T, checkout, other string)
		reason string // "" when it is fast-forwarded
	}{
		"behind": {func(*testing.T, string, string) {}, ""},
		"dirty": {func(t *testing.T, d, _ string) { _ = os.WriteFile(filepath.Join(d, "a"), []byte("x\n"), 0o644) },
			"it has local changes"},
		"diverged": {func(t *testing.T, d, _ string) { commit(t, d, "c") },
			"it has diverged from origin/main (ahead by 1, behind by 1)"},
		"branch": {func(t *testing.T, d, _ string) { gitIn(t, d, "switch", "--quiet", "-c", "topic") },
			"it is on topic, not on origin/main's branch"},
		"detached": {func(t *testing.T, d, _ string) { gitIn(t, d, "switch", "--quiet", "--detach") }, "HEAD is detached"},
		"no fetch": {func(t *testing.T, d, _ string) {
			gitIn(t, d, "remote", "set-url", "origin", filepath.Join(d, "missing.git"))
		}, "`git fetch origin` failed (exit 128)"},
		// A file ignored here that origin now tracks: the update would
		// overwrite it.
		"ignored file in the way": {func(t *testing.T, d, other string) {
			commit(t, other, "local.cfg")
			gitIn(t, other, "push", "--quiet", "origin", "main")
			if err := os.WriteFile(filepath.Join(d, ".git", "info", "exclude"), []byte("local.cfg\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(d, "local.cfg"), []byte("local settings\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "`git merge --ff-only` failed, as it does when a file it would bring in is there untracked or ignored"},
	} {
		checkout, other := behindOrigin(t)
		before := gitIn(t, checkout, "rev-parse", "HEAD")
		c.setup(t, checkout, other)
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
		if data, err := os.ReadFile(filepath.Join(checkout, "local.cfg")); err == nil && string(data) != "local settings\n" {
			t.Errorf("%s: the ignored file was overwritten: %q", name, data)
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

// fakeStatuteOnPath puts a statute on PATH that appends its argv and cwd
// to the returned file and exits 3 for `statute upgrade`, `compile`
// otherwise.
func fakeStatuteOnPath(t *testing.T, compile int) string {
	t.Helper()
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	script := "#!/bin/sh\necho \"$* in $(pwd)\" >> " + calls + "\nif [ \"$1\" = upgrade ]; then exit 3; fi\nexit " +
		strconv.Itoa(compile) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "statute"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return calls
}

func TestSyncCheckoutRefreshesStatuteAfterTheFastForwardOrWithout(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	calls := fakeStatuteOnPath(t, 0)
	stderr, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = stderr
	t.Cleanup(func() { os.Stderr = saved })

	// statute.toml comes with the commit the fast-forward brings in.
	checkout, other := behindOrigin(t)
	commit(t, other, "statute.toml")
	gitIn(t, other, "push", "--quiet", "origin", "main")
	if note := syncCheckout(checkout); note != "" {
		t.Fatalf("behind: %q", note)
	}
	want := "upgrade --private in " + checkout + "\ncompile --private-only in " + checkout + "\n"
	if got, _ := os.ReadFile(calls); string(got) != want {
		t.Errorf("behind: statute calls = %q, want %q", got, want)
	}
	out, _ := os.ReadFile(stderr.Name())
	if !strings.Contains(string(out), "warning: `statute upgrade --private` failed (exit 3) in "+checkout) {
		t.Errorf("stderr = %q, want the upgrade's warning", out)
	}

	// A checkout left as it is still gets the refresh.
	_ = os.Remove(calls)
	commit(t, checkout, "c")
	if note := syncCheckout(checkout); !strings.Contains(note, "it is ahead of origin/main by 1") {
		t.Errorf("ahead: %q", note)
	}
	if got, _ := os.ReadFile(calls); string(got) != want {
		t.Errorf("ahead: statute calls = %q, want %q", got, want)
	}
}

// A compile that fails adds a paragraph saying why and how to generate
// the private output, after the one on the checkout when there is one.
func TestSyncCheckoutSaysWhenStatuteDoesNotGenerateThePrivateOutput(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	fakeStatuteOnPath(t, 4)
	checkout, other := behindOrigin(t)
	commit(t, other, "statute.toml")
	gitIn(t, other, "push", "--quiet", "origin", "main")
	statute := "statute did not generate the private output of " + checkout + " when fleet last ran it there: " +
		"`statute upgrade --private` failed (exit 3), then `statute compile --private-only` failed (exit 4). " +
		"The instructions you read there lack the repo's private rules: before anything else, run " +
		"`statute upgrade --private && statute compile --private-only` in " + checkout + ", and when that fails " +
		"too, say so to whoever you report to before you go on."
	if note := syncCheckout(checkout); note != statute {
		t.Errorf("fast-forwarded: %q, want %q", note, statute)
	}
	commit(t, checkout, "c")
	if note := syncCheckout(checkout); !strings.HasPrefix(note, "The checkout "+checkout+" was not updated") ||
		!strings.HasSuffix(note, ".\n\n"+statute) {
		t.Errorf("ahead: %q, want the checkout's paragraph, then %q", note, statute)
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

package cmd

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The end-to-end tests of `worktree` are in cmd/fleet; this one needs a
// shorter statuteTimeout, which only a test in this package can set.
func TestRefreshStatuteKillsAHungStatuteAndOnlyWarns(t *testing.T) {
	dir, bin := t.TempDir(), t.TempDir()
	pidFile := filepath.Join(dir, "child")
	script := "#!/bin/sh\nsleep 30 &\necho $! > " + pidFile + "\nwait\n"
	if err := os.WriteFile(filepath.Join(bin, "statute"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	if err := os.WriteFile(filepath.Join(dir, "statute.toml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	old := statuteTimeout
	statuteTimeout = 300 * time.Millisecond
	t.Cleanup(func() { statuteTimeout = old })
	stderr, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = stderr
	t.Cleanup(func() { os.Stderr = saved })

	start := time.Now()
	refreshStatute(dir)
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("refreshStatute took %v", took)
	}
	out, _ := os.ReadFile(stderr.Name())
	if want := "warning: `statute compile --private-only` in " + dir + " timed out"; !strings.Contains(string(out), want) {
		t.Errorf("stderr = %q, want %q", out, want)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatal("what statute started outlived the timeout")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

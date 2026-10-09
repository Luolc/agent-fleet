package herdr

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Luolc/agent-fleet/internal/exit"
)

// onPath puts a fake herdr script on PATH for the test.
func onPath(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "herdr"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	return dir
}

// gone reports whether pid has exited within two seconds.
func gone(pid int) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func childPid(t *testing.T, file string) int {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func TestDeadlineNamesTheOperationNotTheBody(t *testing.T) {
	dir := onPath(t, "#!/bin/sh\necho $$ > \""+t.TempDir()+"/pid\"\nsleep 30\n")
	_ = dir
	old := callTimeout
	callTimeout = 300 * time.Millisecond
	defer func() { callTimeout = old }()
	h := New(nil)
	_, err := h.Prompt("x-lead", "[FROM: me]\nsecret body 0xB0D7\n")
	var failure *exit.Failure
	if !errors.As(err, &failure) || failure.Code != exit.Environment {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(failure.Message, "herdr agent prompt timed out") || strings.Contains(failure.Message, "0xB0D7") {
		t.Errorf("message = %q", failure.Message)
	}
}

func TestDescendantHoldingThePipesIsKilled(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	onPath(t, "#!/bin/sh\nsleep 30 &\necho $! > \""+pidFile+"\"\necho '{\"result\":{\"type\":\"agent_prompted\"}}'\n")
	old := waitDelay
	waitDelay = 300 * time.Millisecond
	defer func() { waitDelay = old }()
	h := New(nil)
	start := time.Now()
	outcome, err := h.Prompt("x-lead", "hi\n")
	if err != nil || outcome.Kind != Prompted {
		t.Fatalf("outcome = %+v, err = %v", outcome, err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("call took %v", elapsed)
	}
	if !gone(childPid(t, pidFile)) {
		t.Error("the pipe-holding descendant survived the call")
	}
}

func TestNormalCompletionIsUntouched(t *testing.T) {
	onPath(t, "#!/bin/sh\necho '{\"error\":{\"code\":\"agent_not_found\",\"message\":\"no\"}}'\n")
	outcome, err := New(nil).Prompt("nobody", "hi\n")
	if err != nil || outcome.Kind != NotFound {
		t.Errorf("outcome = %+v, err = %v", outcome, err)
	}
}

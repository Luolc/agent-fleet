package gh

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseAndRateLimited(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	for _, c := range []struct {
		name    string
		out     string
		limited bool
		wait    time.Duration
	}{
		{"ok", "HTTP/2.0 200 OK\r\nX-Ratelimit-Remaining: 10\r\n\r\n{}", false, 0},
		{"retry-after", "HTTP/2.0 403 Forbidden\r\nRetry-After: 30\r\n\r\n{\"message\":\"x\"}", true, 30 * time.Second},
		{"primary exhausted", "HTTP/2.0 403 Forbidden\nX-RateLimit-Remaining: 0\nX-RateLimit-Reset: 1000090\n\n{}", true, 91 * time.Second},
		{"secondary, no header", "HTTP/2.0 403 Forbidden\r\n\r\n{\"message\":\"You have exceeded a secondary rate limit\"}", true, 0},
		{"429", "HTTP/2.0 429 Too Many Requests\r\n\r\n{}", true, 0},
		{"403 not a limit", "HTTP/2.0 403 Forbidden\r\n\r\n{\"message\":\"Resource not accessible\"}", false, 0},
	} {
		resp, ok := parse([]byte(c.out))
		if !ok {
			t.Fatalf("%s: not parsed", c.name)
		}
		limited, wait := resp.RateLimited(now)
		if limited != c.limited || wait != c.wait {
			t.Errorf("%s: got %v %v, want %v %v", c.name, limited, wait, c.limited, c.wait)
		}
	}
	if _, ok := parse([]byte("error connecting to api.github.example.test\n")); ok {
		t.Error("output without a status line is no reply")
	}
}

func TestFirstLineHidesTokens(t *testing.T) {
	got := firstLine([]byte("bad credentials ghp_FAKE0123456789 and github_pat_FAKE_01\nsecond line"))
	if got != "bad credentials [redacted] and [redacted]" {
		t.Errorf("got %q", got)
	}
}

func TestGetWithoutAReply(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	if _, err := Get("repos/o/r", CallTimeout); err == nil || errors.As(err, new(*NoResponse)) {
		t.Errorf("gh missing from PATH is no try to repeat: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\necho 'error connecting to api.github.example.test' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	_, err := Get("repos/o/r", CallTimeout)
	var none *NoResponse
	if !errors.As(err, &none) || !strings.Contains(none.Message, "error connecting") {
		t.Errorf("a call that printed no reply is a NoResponse: %v", err)
	}
}

func TestGetPastItsDeadlineIsNoReply(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	start := time.Now()
	_, err := Get("repos/o/r", 200*time.Millisecond)
	var none *NoResponse
	if !errors.As(err, &none) || time.Since(start) > 3*time.Second {
		t.Errorf("a call past its 200 ms deadline is a NoResponse at once, got %v after %v", err, time.Since(start))
	}
}

package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Luolc/agent-fleet/internal/atb"
	"github.com/Luolc/agent-fleet/internal/exit"
)

func TestLinearTryRetriesOnlyAnAtbExit(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	if err := os.MkdirAll(filepath.Join(state, "fleet"), 0o700); err != nil {
		t.Fatal(err)
	}
	pauses := LinearPauses
	LinearPauses = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { LinearPauses = pauses }()
	failure := func(code int, class string) error {
		return &atb.Failure{Failure: exit.Environmentf("atb linear claim EX-1 failed"), Op: "atb linear claim EX-1",
			ExitCode: code, Class: class, Stderr: "error: " + class}
	}
	for name, c := range map[string]struct {
		err      error
		tries    int
		outcomes string
	}{
		"atb exits 1":     {failure(1, "exit 1"), 3, "retried retried gave up"},
		"claim held":      {failure(3, "exit 3"), 1, "held"},
		"timed out":       {failure(-1, "timed out"), 1, "gave up"},
		"not atb's":       {exit.Database(errors.New("locked")), 1, ""},
		"fails then goes": {nil, 2, "retried"},
	} {
		log := newLinearLog(strings.ReplaceAll(name, " ", "-"), "C1/1.0", "thread-c1-1-0")
		tries := 0
		err := log.try(func() error {
			tries++
			if c.err == nil && tries == 1 {
				return failure(1, "exit 1")
			}
			return c.err
		})
		if tries != c.tries || !errors.Is(err, c.err) {
			t.Errorf("%s: %d tries, err %v", name, tries, err)
		}
		path, _ := LinearLogPath(log.scope)
		data, _ := os.ReadFile(path)
		var outcomes []string
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if _, rest, ok := strings.Cut(line, `"outcome":"`); ok {
				outcomes = append(outcomes, strings.TrimSuffix(rest, `"}`))
			}
		}
		if got := strings.Join(outcomes, " "); got != c.outcomes {
			t.Errorf("%s: outcomes %q, want %q", name, got, c.outcomes)
		}
		if info, err := os.Stat(path); err == nil && info.Mode().Perm() != 0o600 {
			t.Errorf("%s: log mode %v", name, info.Mode().Perm())
		}
	}
}

// The steps of a start share one deadline: with every atb call slow (59 s
// to fail, or 60 s to time out), no try begins after LinearBudget, so the
// Linear steps end within the budget and one call.
func TestLinearStepsStopAtTheirDeadline(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	clock := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	now, sleep := linearNow, linearSleep
	linearNow = func() time.Time { return clock }
	linearSleep = func(d time.Duration) { clock = clock.Add(d) }
	defer func() { linearNow, linearSleep = now, sleep }()
	for name, call := range map[string]struct {
		took time.Duration
		code int
	}{"slow exits": {59 * time.Second, 1}, "timeouts": {60 * time.Second, -1}} {
		start := clock
		log := newLinearLog("budget", "C1/1.0", "thread-c1-1-0")
		calls := 0
		var err error
		// Four steps, as a known thread without its ticket has: create,
		// claim, comment, summaries; each would succeed on its third try.
		for step := 0; step < 4 && err == nil; step++ {
			tries := 0
			err = log.try(func() error {
				calls++
				tries++
				clock = clock.Add(call.took)
				if tries < 3 {
					return &atb.Failure{Failure: exit.Environmentf("failed"), Op: "atb linear claim EX-1",
						ExitCode: call.code, Class: "exit"}
				}
				return nil
			})
		}
		if took := clock.Sub(start); err == nil || took > LinearBudget+60*time.Second {
			t.Errorf("%s: %d calls took %v, err %v; want a failure within %v", name, calls, took, err, LinearBudget+60*time.Second)
		}
	}
}

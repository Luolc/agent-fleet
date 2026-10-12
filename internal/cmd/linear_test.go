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
		log := linearLog{scope: strings.ReplaceAll(name, " ", "-"), thread: "C1/1.0", agent: "thread-c1-1-0"}
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

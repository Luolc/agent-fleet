// Linear steps of a thread's session that may fail without stopping the
// work: tried again after a pause, and each failure logged with atb's exit
// code and stderr, so it can be looked into after the hook has exited.

package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Luolc/agent-fleet/internal/atb"
	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
)

// LinearPauses are the pauses before the second and the third try of a
// Linear step atb failed.
var LinearPauses = []time.Duration{3 * time.Second, 7 * time.Second}

// LinearBudget is how long the Linear steps of one start or ending may
// take before no new try begins. A try already running can take atb's 60 s
// past it, so `inbox` reaches its agent inside the hook's 5 minutes: at
// most 2.5 minutes for Linear, 30 s for the fetch, about a minute for the
// agent's start.
var LinearBudget = 90 * time.Second

// linearNow and linearSleep are the clock of the tries, replaced in tests.
var (
	linearNow   = time.Now
	linearSleep = time.Sleep
)

// linearLog is where the failed tries of one thread's Linear steps are
// logged, and when their time is up.
type linearLog struct {
	scope, thread, agent string
	deadline             time.Time
}

// newLinearLog starts the Linear steps' time.
func newLinearLog(scope, thread, agent string) linearLog {
	return linearLog{scope: scope, thread: thread, agent: agent, deadline: linearNow().Add(LinearBudget)}
}

// linearLogEntry is one line of the log, one failed try.
type linearLogEntry struct {
	Time   string `json:"time"`
	Thread string `json:"thread"`
	Agent  string `json:"agent"`
	Step   string `json:"step"`
	Try    int    `json:"try"`
	Exit   int    `json:"exit"`
	Class  string `json:"class"`
	Stderr string `json:"stderr"`
	// Outcome is `retried`, `gave up`, `held` for a claim another agent
	// holds, or `unchecked` for a claim whose last try wrote the claim
	// without finishing its conflict check (atb's exit 5), which the start
	// takes, as it takes one an earlier try wrote.
	Outcome string `json:"outcome"`
}

// LinearLogPath is the scope's Linear log, `<scope>-linear.log` next to
// its ledger.
func LinearLogPath(scope string) (string, error) {
	path, err := db.Path(scope)
	return strings.TrimSuffix(path, ".db") + "-linear.log", err
}

// try runs one Linear step: while atb exits non-zero it is run again after
// each of LinearPauses. A claim another agent holds (exit 3) is final, and
// so is a call that timed out or could not run: atb's minute is spent, and
// the hook has an agent to start. No try begins, and no pause is waited
// out, past the deadline. Each failed try is logged.
func (l linearLog) try(do func() error) error {
	for try := 1; ; try++ {
		if !linearNow().Before(l.deadline) {
			f := &atb.Failure{Op: "the Linear steps", ExitCode: -1, Class: fmt.Sprintf("out of time after %v", LinearBudget),
				Failure: exit.Environmentf("the Linear steps took their %v; the rest are not tried", LinearBudget)}
			l.write(linearLogEntry{Step: f.Op, Try: try, Exit: f.ExitCode, Class: f.Class, Outcome: "gave up"})
			return f
		}
		err := do()
		var failure *atb.Failure
		if err == nil || !errors.As(err, &failure) {
			return err
		}
		outcome := "retried"
		switch {
		case atb.Held(err):
			outcome = "held"
		case try > len(LinearPauses) || failure.ExitCode < 0 || !linearNow().Add(LinearPauses[try-1]).Before(l.deadline):
			outcome = "gave up"
			if atb.Unchecked(err) {
				outcome = "unchecked"
			}
		}
		l.write(linearLogEntry{Step: failure.Op, Try: try, Exit: failure.ExitCode, Class: failure.Class,
			Stderr: failure.Stderr, Outcome: outcome})
		if outcome != "retried" {
			return err
		}
		fmt.Fprintf(os.Stderr, "note: %s failed (%s), trying again in %v\n", failure.Op, failure.Class, LinearPauses[try-1])
		linearSleep(LinearPauses[try-1])
	}
}

// write appends one entry to the log, created 0600. A log that cannot be
// written is said on stderr and stops nothing.
func (l linearLog) write(e linearLogEntry) {
	e.Time, e.Thread, e.Agent = time.Now().UTC().Format(time.RFC3339), l.thread, l.agent
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	path, err := LinearLogPath(l.scope)
	if err == nil {
		var f *os.File
		if f, err = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			_, err = f.Write(append(line, '\n'))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "note: the Linear failure could not be logged: %v\n", err)
	}
}

// fleetError logs a failure of a Linear step that is not atb's own (its
// reply unreadable, the ledger), which no try logged: the hook's stderr
// is not kept. Its message never holds atb's output.
func (l linearLog) fleetError(err error) {
	var failure *atb.Failure
	if !errors.As(err, &failure) {
		l.write(linearLogEntry{Step: "fleet", Exit: -1, Class: "error", Stderr: err.Error(), Outcome: "gave up"})
	}
}

// linearClass is a failed step in a few words, for the thread and the
// agent: the step and atb's exit (`atb linear claim TH-5: exit 1`), or for
// a failure that is not atb's own a pointer to the log.
func linearClass(err error) string {
	var failure *atb.Failure
	if errors.As(err, &failure) {
		return failure.Op + ": " + failure.Class
	}
	return "an error of fleet's own, in the Linear log"
}

// pendingDir is where the summaries of a thread's sessions that could not
// reach Linear wait, one file per session (`<session>.md`, zero-padded so
// they sort oldest first): `<scope>-summaries/<slug>/` next to the ledger.
func pendingDir(scope, thread string) (string, error) {
	path, err := db.Path(scope)
	return filepath.Join(strings.TrimSuffix(path, ".db")+"-summaries", ThreadSlug(thread)), err
}

// keepSummary keeps a session's summary comment (`Session <n> ended` and
// the summary) until it is written to the thread's ticket.
func keepSummary(scope, thread string, session int64, body string) error {
	dir, err := pendingDir(scope, thread)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return exit.IO(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%06d.md", session)), []byte(body), 0o600); err != nil {
		return exit.IO(err)
	}
	return nil
}

// writeKept writes the thread's kept summaries to `ticket`, oldest first,
// each removed once written, so a summary is written once (unless the
// removal fails after Linear took it).
func (l linearLog) writeKept(ticket string) error {
	dir, err := pendingDir(l.scope, l.thread)
	if err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return exit.IO(err)
	}
	sort.Strings(files)
	for _, file := range files {
		if err := l.try(func() error { return atb.Comment(ticket, file) }); err != nil {
			return err
		}
		if err := os.Remove(file); err != nil {
			return exit.IO(err)
		}
		fmt.Fprintf(os.Stdout, "wrote the kept summary %s to %s\n", filepath.Base(file), ticket)
	}
	return nil
}

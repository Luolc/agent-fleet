// Linear steps of a thread's session that may fail without stopping the
// work: tried again after a pause, and each failure logged with atb's exit
// code and stderr, so it can be looked into after the hook has exited.

package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Luolc/agent-fleet/internal/atb"
	"github.com/Luolc/agent-fleet/internal/db"
)

// LinearPauses are the pauses before the second and the third try of a
// Linear step atb failed.
var LinearPauses = []time.Duration{3 * time.Second, 7 * time.Second}

// linearLog is where the failed tries of one thread's Linear steps are
// logged.
type linearLog struct {
	scope, thread, agent string
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
	// Outcome is `retried`, `gave up`, or `held` for a claim another agent
	// holds.
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
// the hook has an agent to start. Each failed try is logged.
func (l linearLog) try(do func() error) error {
	for try := 1; ; try++ {
		err := do()
		var failure *atb.Failure
		if err == nil || !errors.As(err, &failure) {
			return err
		}
		outcome := "retried"
		switch {
		case atb.Held(err):
			outcome = "held"
		case try > len(LinearPauses) || failure.ExitCode < 0:
			outcome = "gave up"
		}
		l.write(linearLogEntry{Step: failure.Op, Try: try, Exit: failure.ExitCode, Class: failure.Class,
			Stderr: failure.Stderr, Outcome: outcome})
		if outcome != "retried" {
			return err
		}
		fmt.Fprintf(os.Stderr, "note: %s failed (%s), trying again in %v\n", failure.Op, failure.Class, LinearPauses[try-1])
		time.Sleep(LinearPauses[try-1])
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

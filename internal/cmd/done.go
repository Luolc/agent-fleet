// `fleet done`: the mandatory completion report.

package cmd

import (
	"fmt"
	"os"

	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
	"github.com/Luolc/agent-fleet/internal/identity"
)

// DoneAbout and DoneLongAbout are the help texts of `done`.
const (
	DoneAbout     = "Report that your task is finished to the agent that spawned you, and end your row"
	DoneLongAbout = "Report that your task is finished to the agent that spawned you, and end your row.\n\n" +
		"Call it from your own pane when your work is done. The report goes to FLEET_PARENT " +
		"through the same path as `fleet send`, with the header; it names the result file " +
		"(as an absolute path) when one is given. When herdr reports the report as delivered, " +
		"your row in the ledger is marked ended, so `fleet status` stops listing you as " +
		"owing work. On any other outcome the row stays live and you may run `done` again.\n\n" +
		"Exit: as `send`; 1 also when FLEET_PARENT is empty (the orchestra and " +
		"human-interface have no parent) or the result file cannot be read."
)

// DoneArgs are the arguments of `done`.
type DoneArgs struct {
	// ResultFile, when set, is the file with the result, named in the
	// report so the parent can read it.
	ResultFile *string
}

// Report is the report's body, before the header.
func Report(agent string, resultFile *string) string {
	if resultFile != nil {
		return fmt.Sprintf("%s is done. Result: %s\n", agent, *resultFile)
	}
	return fmt.Sprintf("%s is done.\n", agent)
}

// Done runs `done`.
func Done(h *herdr.Herdr, args DoneArgs) (exit.Code, error) {
	me, err := identity.FromEnv()
	if err != nil {
		return 0, err
	}
	if me.Parent == "" {
		return 0, exit.Refusedf("FLEET_PARENT is empty: only a lead or a worker reports with `done`")
	}
	var resultFile *string
	if args.ResultFile != nil {
		path, err := canonicalize(*args.ResultFile)
		if err != nil {
			return 0, exit.Refusedf("cannot read %s: %v", *args.ResultFile, err)
		}
		resultFile = &path
	}
	conn, err := db.Open(me.Repo)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	text, err := WithHeader(me.Agent, Report(me.Agent, resultFile))
	if err != nil {
		return 0, err
	}
	code, err := Deliver(h, me.Parent, text)
	if err != nil {
		return 0, err
	}
	if code == exit.Ok {
		res, err := conn.Exec(
			"UPDATE agents SET state = 'ended', ended_at = ?1 WHERE name = ?2 AND state != 'ended'",
			db.Now(), me.Agent)
		if err != nil {
			return 0, exit.Database(err)
		}
		ended, err := res.RowsAffected()
		if err != nil {
			return 0, exit.Database(err)
		}
		if ended == 0 {
			fmt.Fprintf(os.Stderr, "note: the ledger has no live row for %s\n", me.Agent)
		}
	}
	return code, nil
}

// `fleet done`: the mandatory completion report.

package cmd

import (
	"fmt"
	"os"

	"github.com/Luolc/agent-fleet/internal/atb"
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
		"(as an absolute path) when one is given.\n\n" +
		"With --report-file and --issue, the worker report is written to the Linear issue " +
		"first (`atb linear comment`), the issue is released as done, or as abandoned with " +
		"--abandon (`atb linear release`), and only then is the report delivered, naming " +
		"the issue. If an atb step fails, nothing is delivered, the row stays live and " +
		"`done` exits 5 naming the step; the comment may already be written.\n\n" +
		"When herdr reports the report as delivered, " +
		"your row in the ledger is marked ended, so `fleet status` stops listing you as " +
		"owing work. On any other outcome the row stays live and you may run `done` again.\n\n" +
		"Exit: as `send`; 1 also when FLEET_PARENT is empty (the orchestra and " +
		"human-interface have no parent), a result or report file cannot be read, " +
		"--report-file and --issue are not given together, or --abandon is given without them; " +
		"5 also when an atb step fails."
)

// DoneArgs are the arguments of `done`.
type DoneArgs struct {
	// ResultFile, when set, is the file with the result, named in the
	// report so the parent can read it.
	ResultFile *string
	// ReportFile and Issue, when set (always together), are the worker
	// report and the Linear issue it is written to before delivery.
	ReportFile, Issue *string
	// Abandon releases the issue as abandoned instead of done.
	Abandon bool
}

// Report is the report's body, before the header. issue, when set, is
// where the worker report was written.
func Report(agent string, resultFile, issue *string, abandoned bool) string {
	text := agent + " is done."
	if abandoned {
		text = agent + " abandoned the task."
	}
	if issue != nil {
		text += " Report: " + *issue + "."
	}
	if resultFile != nil {
		text += " Result: " + *resultFile
	}
	return text + "\n"
}

// checkReport refuses the report arguments before anything is written.
func checkReport(args DoneArgs) error {
	if (args.ReportFile == nil) != (args.Issue == nil) {
		return exit.Refusedf("--report-file and --issue go together")
	}
	if args.Abandon && args.ReportFile == nil {
		return exit.Refusedf("--abandon needs --report-file and --issue")
	}
	if args.ReportFile != nil {
		if _, err := os.ReadFile(*args.ReportFile); err != nil {
			return exit.Refusedf("cannot read %s: %v", *args.ReportFile, err)
		}
	}
	return nil
}

// writeReport writes the worker report to the issue, then releases it.
func writeReport(args DoneArgs, agent string) error {
	if err := atb.Comment(*args.Issue, *args.ReportFile); err != nil {
		return err
	}
	return atb.Release(*args.Issue, agent, args.Abandon)
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
	if err := checkReport(args); err != nil {
		return 0, err
	}
	conn, err := db.Open(me.Repo)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	text, err := WithHeader(me.Agent, Report(me.Agent, resultFile, args.Issue, args.Abandon))
	if err != nil {
		return 0, err
	}
	if args.ReportFile != nil {
		if err := writeReport(args, me.Agent); err != nil {
			return 0, err
		}
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

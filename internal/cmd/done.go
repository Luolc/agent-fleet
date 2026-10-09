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
	DoneAbout     = "Report that your task is finished to your lead, and end your row (workers only)"
	DoneLongAbout = "Report that your task is finished to your lead, and end your row (workers only).\n\n" +
		"Call it from your own pane when your work is done; a lead ends its job with `fleet job " +
		"end` instead. The report goes to FLEET_PARENT " +
		"through the same path as `fleet send`, with the header; it names the report file " +
		"(as an absolute path) when one is given.\n\n" +
		"When FLEET_ISSUE names your Linear work order, --report-file is required: the " +
		"report is written to the issue first (`atb linear comment`), the issue is released " +
		"as done, or as abandoned with --abandon (`atb linear release`), and only then is " +
		"the report delivered, naming the issue. If an atb step fails, nothing is delivered, " +
		"the row stays live and `done` exits 5 naming the step; the comment may already be " +
		"written.\n\n" +
		"When herdr reports the report as delivered, " +
		"your row in the ledger is marked ended, so `fleet status` stops listing you as " +
		"owing work. On any other outcome the row stays live and you may run `done` again.\n\n" +
		"Exit: as `send`; 1 also when the caller is not a worker, FLEET_PARENT is empty, " +
		"the report file cannot be read, FLEET_ISSUE is set " +
		"and --report-file is not given, or --abandon is given while FLEET_ISSUE is empty; " +
		"5 also when an atb step fails."
)

// DoneArgs are the arguments of `done`.
type DoneArgs struct {
	// ReportFile, when set, is the report, named in the message so the
	// parent can read it, and written to FLEET_ISSUE when that is set.
	ReportFile *string
	// Abandon releases the issue as abandoned instead of done.
	Abandon bool
}

// Report is the report's body, before the header. issue is empty when the
// caller has no work order; reportFile is nil when none was given.
func Report(agent, issue string, reportFile *string, abandoned bool) string {
	text := agent + " is done."
	if abandoned {
		text = agent + " abandoned the task."
	}
	if issue != "" {
		text += " Issue: " + issue + "."
	}
	if reportFile != nil {
		text += " Report: " + *reportFile
	}
	return text + "\n"
}

// reportPath checks the report arguments before anything is written and
// returns the report file as an absolute path, or nil without one.
func reportPath(args DoneArgs, issue string) (*string, error) {
	if args.Abandon && issue == "" {
		return nil, exit.Refusedf("--abandon releases FLEET_ISSUE, which is empty")
	}
	if args.ReportFile == nil {
		if issue != "" {
			return nil, exit.Refusedf("FLEET_ISSUE is %s: --report-file is required", issue)
		}
		return nil, nil
	}
	path, err := canonicalize(*args.ReportFile)
	if err == nil {
		_, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, exit.Refusedf("cannot read %s: %v", *args.ReportFile, err)
	}
	return &path, nil
}

// Done runs `done`.
func Done(h *herdr.Herdr, args DoneArgs) (exit.Code, error) {
	me, err := identity.FromEnv()
	if err != nil {
		return 0, err
	}
	if me.Role != identity.Worker {
		return 0, exit.Refusedf("a %s does not report with `done`; a lead ends its job with `fleet job end`", me.Role)
	}
	if me.Parent == "" {
		return 0, exit.Refusedf("FLEET_PARENT is empty: a worker reports to the lead that spawned it")
	}
	reportFile, err := reportPath(args, me.Issue)
	if err != nil {
		return 0, err
	}
	conn, err := db.Open(me.Target)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	text, err := WithHeader(me.Agent, Report(me.Agent, me.Issue, reportFile, args.Abandon))
	if err != nil {
		return 0, err
	}
	if me.Issue != "" {
		if err := atb.Comment(me.Issue, *reportFile); err != nil {
			return 0, err
		}
		if err := atb.Release(me.Issue, me.Agent, args.Abandon); err != nil {
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

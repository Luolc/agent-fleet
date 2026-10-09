// Command fleet runs and coordinates coding agents through herdr. This file
// is the command-line definition and dispatch.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Luolc/agent-fleet/internal/cliargs"
	"github.com/Luolc/agent-fleet/internal/cmd"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
)

// version is the fleet release version.
const version = "0.0.0"

const longAbout = "Runs and coordinates coding agents through herdr.\n\n" +
	"A thread agent starts a job (`fleet job start`): its lead, which starts workers " +
	"(`fleet spawn`). Every agent fleet starts carries its identity in FLEET_* " +
	"environment variables; messages between agents go through `fleet send`, " +
	"which adds the `[FROM: <agent>]` header. State lives in the ledger of the target, " +
	"$XDG_STATE_HOME/fleet/<target>/fleet.db (~/.local/state when XDG_STATE_HOME is unset; " +
	"the target is FLEET_TARGET, `default` when unset).\n\n" +
	"Exit codes, shared by every command:\n" +
	"  0  ok\n" +
	"  1  usage error or precondition refused (role, cap, resources, empty body)\n" +
	"  2  herdr gave no clear signal (timeout, stalled): the outcome is unknown, do not resend blindly\n" +
	"  3  target blocked (its screen is printed), or spawn stopped at an unknown screen\n" +
	"  4  target not found\n" +
	"  5  environment error (herdr, atb, git or the database failed)"

const sessionHelp = "herdr session to talk to. Inside a herdr pane this is not needed: herdr " +
	"finds the pane's own session. Use it from cron or a plain shell"

const targetHelp = "Target whose ledger to read; the ledger is $XDG_STATE_HOME/fleet/<target>/fleet.db " +
	"(~/.local/state when XDG_STATE_HOME is unset). Defaults to FLEET_TARGET, which every agent has, " +
	"then to `default`"

const topUsage = "Usage: fleet [OPTIONS] <COMMAND>"

var topHelp = longAbout + "\n\n" + topUsage + `

Commands:
  inbox     ` + cmd.InboxAbout + `
  thread    ` + cmd.ThreadAbout + `
  job       ` + cmd.JobAbout + `
  spawn     ` + cmd.SpawnAbout + `
  send      ` + cmd.SendAbout + `
  done      ` + cmd.DoneAbout + `
  status    ` + cmd.StatusAbout + `
  watch     ` + cmd.WatchAbout + `
  worktree  ` + cmd.WorktreeAbout + `
  help      Print this message or the help of the given subcommand(s)

Options:
      --session <NAME>  ` + sessionHelp + `
  -h, --help            Print help
  -V, --version         Print version
`

const sendUsage = "Usage: fleet send [OPTIONS] <TO>"

var sendHelp = cmd.SendLongAbout + "\n\n" + sendUsage + `

Arguments:
  <TO>  Name of the receiving agent, as shown by ` + "`fleet status`" + `

Options:
      --file <PATH>     Read the body from this file instead of stdin
      --session <NAME>  ` + sessionHelp + `
  -h, --help            Print help
`

const inboxUsage = "Usage: fleet inbox [OPTIONS] <EVENT-FILE>"

var inboxHelp = cmd.InboxLongAbout + "\n\n" + inboxUsage + `

Arguments:
  <EVENT-FILE>  The event file the fednet client wrote: one JSON object with msg_id and payload

Options:
      --session <NAME>  ` + sessionHelp + `; the hook runs outside herdr, so without it the
                        target's name is used
  -h, --help            Print help
`

const threadUsage = "Usage: fleet thread <COMMAND>"

var threadHelp = cmd.ThreadAbout + ".\n\n" + threadUsage + `

Commands:
  end          ` + cmd.ThreadEndAbout + `
  set-project  ` + cmd.ThreadSetProjectAbout + `
  relate       ` + cmd.ThreadRelateAbout + `

Options:
  -h, --help  Print help
`

const threadEndUsage = "Usage: fleet thread end [OPTIONS] --summary-file <PATH>"

var threadEndHelp = cmd.ThreadEndLongAbout + "\n\n" + threadEndUsage + `

Options:
      --summary-file <PATH>  The session's summary, written to the thread ticket and given to the next session
      --force                Finish the local cleanup even when a Linear step keeps failing; the steps left are printed
      --session <NAME>       ` + sessionHelp + `
  -h, --help                 Print help
`

const threadSetProjectUsage = "Usage: fleet thread set-project <PROJECT>"

var threadSetProjectHelp = cmd.ThreadSetProjectLongAbout + "\n\n" + threadSetProjectUsage + `

Arguments:
  <PROJECT>  Project name, matched exactly against the ticket's team

Options:
  -h, --help  Print help
`

const threadRelateUsage = "Usage: fleet thread relate <ISSUE>"

var threadRelateHelp = cmd.ThreadRelateLongAbout + "\n\n" + threadRelateUsage + `

Arguments:
  <ISSUE>  The issue to relate the ticket to, such as ABC-12

Options:
  -h, --help  Print help
`

const jobUsage = "Usage: fleet job <COMMAND>"

var jobHelp = cmd.JobAbout + ".\n\n" + jobUsage + `

Commands:
  start  ` + cmd.JobStartAbout + `
  list   ` + cmd.JobListAbout + `
  end    ` + cmd.JobEndAbout + `

Options:
  -h, --help  Print help
`

const jobStartUsage = "Usage: fleet job start [OPTIONS] --task-file <PATH> <JOB>"

var jobStartHelp = cmd.JobStartLongAbout + "\n\n" + jobStartUsage + `

Arguments:
  <JOB>  Job id; the lead is named <job>-lead. Only [a-z0-9-]

Options:
      --task-file <PATH>  File with the lead's task, delivered as its first message (with the header)
      --parent-issue <ISSUE>
                          The job's parent issue, such as ABC-12 (with Linear on; excludes --new-parent)
      --new-parent <TITLE>
                          Create the parent issue with this title first (single-repo jobs with Linear on)
      --repo <REPO>       Directory name under ~/dev: a single-repo job, the lead runs there. Without
                          it the job is cross-repo and the lead runs in ~/cross-repo/<job>/
      --key <KEY>         Dedup key: refused when an open job of the target has the same key
      --model <MODEL>     Model passed to the agent as --model. Default: the agent's own
      --effort <EFFORT>   Effort passed to the agent as --effort. Default: the agent's own
      --session <NAME>    ` + sessionHelp + `
  -h, --help              Print help
`

const jobListUsage = "Usage: fleet job list [OPTIONS]"

var jobListHelp = cmd.JobListLongAbout + "\n\n" + jobListUsage + `

Options:
      --target <TARGET>  ` + targetHelp + `
      --json             Machine-readable output: a JSON array, one object per job
  -h, --help             Print help
`

const jobEndUsage = "Usage: fleet job end [OPTIONS] [JOB]"

var jobEndHelp = cmd.JobEndLongAbout + "\n\n" + jobEndUsage + `

Arguments:
  [JOB]  Job id; required with --force, otherwise it must be your own job (FLEET_JOB)

Options:
      --report-file <PATH>  Your report, written to your work order with atb when FLEET_ISSUE is set,
                            and named in the conclusion (required without --force)
      --abandon             End the job as abandoned instead of done
      --force               Reclaim the job from outside (a thread agent, or no FLEET_ROLE): no report,
                            no Linear step; the cleanup after a failed start or a lost lead
      --session <NAME>      ` + sessionHelp + `
  -h, --help                Print help
`

const spawnUsage = "Usage: fleet spawn [OPTIONS] --task-file <PATH> --cwd <DIR> <NAME>"

var spawnHelp = cmd.SpawnLongAbout + "\n\n" + spawnUsage + `

Arguments:
  <NAME>  Worker name within the job; the worker is named <job>-<NAME>. Only [a-z0-9-]

Options:
      --task-file <PATH>  File with the task, delivered as the worker's first message (with the header)
      --cwd <DIR>         Directory the worker runs in; must exist (typically a path from ` + "`fleet worktree`" + `)
      --model <MODEL>     Model passed to the agent as --model. Default: the agent's own
      --effort <EFFORT>   Effort passed to the agent as --effort. Default: the agent's own
      --session <NAME>    ` + sessionHelp + `
  -h, --help              Print help
`

const doneUsage = "Usage: fleet done [OPTIONS]"

var doneHelp = cmd.DoneLongAbout + "\n\n" + doneUsage + `

Options:
      --report-file <PATH>  Your report, named in the message so the parent can read it, and written
                            to FLEET_ISSUE with atb first when that is set (then it is required)
      --abandon             Release FLEET_ISSUE as abandoned instead of done
      --session <NAME>      ` + sessionHelp + `
  -h, --help                Print help
`

const statusUsage = "Usage: fleet status [OPTIONS]"

var statusHelp = cmd.StatusLongAbout + "\n\n" + statusUsage + `

Options:
      --job <JOB>        Only this job and its agents
      --json             Machine-readable output: {"jobs": [...], "agents": [...]}
      --target <TARGET>  ` + targetHelp + `
      --session <NAME>   ` + sessionHelp + `
  -h, --help             Print help
`

const watchUsage = "Usage: fleet watch [OPTIONS]"

var watchHelp = cmd.WatchLongAbout + "\n\n" + watchUsage + `

Options:
      --target <TARGET>  ` + targetHelp + `
      --session <NAME>   ` + sessionHelp + `
  -h, --help             Print help
`

const worktreeUsage = "Usage: fleet worktree [OPTIONS] <REPO>"

var worktreeHelp = cmd.WorktreeLongAbout + "\n\n" + worktreeUsage + `

Arguments:
  <REPO>  Directory name of the checkout under ~/dev

Options:
      --name <NAME>     Another worktree of the job in this repo: ~/wt/<repo>/<job>-<name>. Only [a-z0-9-]
      --branch <NAME>   Branch to create. Default: <job>, or <job>-<name> with --name
      --session <NAME>  ` + sessionHelp + `
  -h, --help            Print help
`

func main() {
	os.Exit(run(os.Args[1:]))
}

// usageError is a parse failure in clap's words: printed to stderr, exit 1.
type usageError struct {
	message string
	usage   string
}

func (e *usageError) Error() string {
	return fmt.Sprintf("error: %s\n\n%s\n\nFor more information, try '--help'.\n", e.message, e.usage)
}

// flagSet is a FlagSet that reports its own errors through usageError and
// `-h`/`--help` through flag.ErrHelp, with the global `--session`.
func flagSet(name string, session *cliargs.OptString) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Var(session, "session", sessionHelp)
	return fs
}

// run parses the command line, runs the command, and returns the process
// exit code.
func run(args []string) int {
	code, err := dispatch(args)
	if err == nil {
		return int(code)
	}
	var usage *usageError
	if errors.As(err, &usage) {
		fmt.Fprint(os.Stderr, usage.Error())
		return int(exit.Refused)
	}
	var failure *exit.Failure
	if errors.As(err, &failure) {
		fmt.Fprintf(os.Stderr, "fleet: %s\n", failure.Message)
		return int(failure.Code)
	}
	fmt.Fprintf(os.Stderr, "fleet: %s\n", err)
	return int(exit.Environment)
}

func dispatch(args []string) (exit.Code, error) {
	session := cliargs.OptString{Name: "session", Placeholder: "NAME"}
	fs := flagSet("fleet", &session)
	var showVersion bool
	fs.BoolVar(&showVersion, "version", false, "Print version")
	fs.BoolVar(&showVersion, "V", false, "Print version")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(os.Stdout, topHelp)
			return exit.Ok, nil
		}
		return 0, &usageError{err.Error(), topUsage}
	}
	if showVersion {
		fmt.Fprintf(os.Stdout, "fleet %s\n", version)
		return exit.Ok, nil
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return 0, &usageError{"'fleet' requires a subcommand but one was not provided", topUsage}
	}
	switch rest[0] {
	case "help":
		return help(rest[1:])
	case "inbox":
		return runInbox(rest[1:], &session)
	case "thread":
		return runThread(rest[1:], &session)
	case "job":
		return runJob(rest[1:], &session)
	case "send":
		return runSend(rest[1:], &session)
	case "spawn":
		return runSpawn(rest[1:], &session)
	case "done":
		return runDone(rest[1:], &session)
	case "status":
		return runStatus(rest[1:], &session)
	case "watch":
		return runWatch(rest[1:], &session)
	case "worktree":
		return runWorktree(rest[1:], &session)
	default:
		return 0, &usageError{fmt.Sprintf("unrecognized subcommand '%s'", rest[0]), topUsage}
	}
}

// help is clap's implicit `help [COMMAND]` subcommand.
func help(args []string) (exit.Code, error) {
	helps := map[string]string{"inbox": inboxHelp, "thread": threadHelp, "job": jobHelp, "send": sendHelp, "spawn": spawnHelp, "done": doneHelp,
		"status": statusHelp, "watch": watchHelp, "worktree": worktreeHelp, "help": topHelp}
	if len(args) == 0 {
		fmt.Fprint(os.Stdout, topHelp)
		return exit.Ok, nil
	}
	if args[0] == "job" && len(args) > 1 {
		switch args[1] {
		case "start":
			fmt.Fprint(os.Stdout, jobStartHelp)
			return exit.Ok, nil
		case "list":
			fmt.Fprint(os.Stdout, jobListHelp)
			return exit.Ok, nil
		case "end":
			fmt.Fprint(os.Stdout, jobEndHelp)
			return exit.Ok, nil
		}
	}
	if args[0] == "thread" && len(args) > 1 {
		if text, ok := threadHelps[args[1]]; ok {
			fmt.Fprint(os.Stdout, text)
			return exit.Ok, nil
		}
	}
	text, ok := helps[args[0]]
	if !ok {
		return 0, &usageError{fmt.Sprintf("unrecognized subcommand '%s'", args[0]), topUsage}
	}
	fmt.Fprint(os.Stdout, text)
	return exit.Ok, nil
}

// parse parses a command's arguments: help goes to stdout with exit 0,
// any other failure is a usageError. names are the positionals in order, a
// name in brackets ("[JOB]") is optional; required are the flags that must be given (clap: a non-Option `#[arg]`),
// listed after the positionals when missing, as clap lists them.
func parse(fs *flag.FlagSet, args []string, help, usage string, names []string, required ...*cliargs.OptString) ([]string, bool, error) {
	positionals := len(names)
	got, err := cliargs.Parse(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(os.Stdout, help)
			return nil, true, nil
		}
		return nil, false, &usageError{err.Error(), usage}
	}
	var missing []string
	if len(got) < positionals {
		for _, name := range names[len(got):] {
			if !strings.HasPrefix(name, "[") {
				missing = append(missing, "  <"+name+">")
			}
		}
	}
	for _, flag := range required {
		if !flag.Given {
			missing = append(missing, "  --"+flag.Name+" <"+flag.Placeholder+">")
		}
	}
	if len(missing) > 0 {
		return nil, false, &usageError{
			"the following required arguments were not provided:\n" + strings.Join(missing, "\n"), usage}
	}
	if len(got) > positionals {
		return nil, false, &usageError{fmt.Sprintf("unexpected argument '%s' found", got[positionals]), usage}
	}
	return got, false, nil
}

func runSend(args []string, session *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("send", session)
	file := cliargs.OptString{Name: "file", Placeholder: "PATH"}
	fs.Var(&file, "file", "Read the body from this file instead of stdin")
	got, helped, err := parse(fs, args, sendHelp, sendUsage, []string{"TO"})
	if err != nil || helped {
		return exit.Ok, err
	}
	return cmd.Send(herdr.New(session.Ptr()), cmd.SendArgs{To: got[0], File: file.Ptr()})
}

func runInbox(args []string, session *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("inbox", session)
	got, helped, err := parse(fs, args, inboxHelp, inboxUsage, []string{"EVENT-FILE"})
	if err != nil || helped {
		return exit.Ok, err
	}
	return cmd.Inbox(herdr.New(session.Ptr()), cmd.InboxArgs{File: got[0]})
}

var threadHelps = map[string]string{"end": threadEndHelp, "set-project": threadSetProjectHelp, "relate": threadRelateHelp}

// runThread dispatches `fleet thread <COMMAND>`.
func runThread(args []string, session *cliargs.OptString) (exit.Code, error) {
	if len(args) == 0 {
		return 0, &usageError{"'fleet thread' requires a subcommand but one was not provided", threadUsage}
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, threadHelp)
		return exit.Ok, nil
	case "end":
		fs := flagSet("thread end", session)
		summary := cliargs.OptString{Name: "summary-file", Placeholder: "PATH"}
		force := cliargs.Bool{Name: "force"}
		fs.Var(&summary, "summary-file", "The session's summary")
		fs.Var(&force, "force", "Finish the local cleanup even when a Linear step keeps failing")
		_, helped, err := parse(fs, args[1:], threadEndHelp, threadEndUsage, nil, &summary)
		if err != nil || helped {
			return exit.Ok, err
		}
		return cmd.ThreadEnd(herdr.New(session.Ptr()), cmd.ThreadEndArgs{SummaryFile: summary.Value, Force: force.Value})
	case "set-project":
		got, helped, err := parse(flagSet("thread set-project", session), args[1:], threadSetProjectHelp,
			threadSetProjectUsage, []string{"PROJECT"})
		if err != nil || helped {
			return exit.Ok, err
		}
		return cmd.ThreadSetProject(got[0])
	case "relate":
		got, helped, err := parse(flagSet("thread relate", session), args[1:], threadRelateHelp,
			threadRelateUsage, []string{"ISSUE"})
		if err != nil || helped {
			return exit.Ok, err
		}
		return cmd.ThreadRelate(got[0])
	default:
		return 0, &usageError{fmt.Sprintf("unrecognized subcommand '%s'", args[0]), threadUsage}
	}
}

// runJob dispatches `fleet job <COMMAND>`.
func runJob(args []string, session *cliargs.OptString) (exit.Code, error) {
	if len(args) == 0 {
		return 0, &usageError{"'fleet job' requires a subcommand but one was not provided", jobUsage}
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, jobHelp)
		return exit.Ok, nil
	case "start":
		return runJobStart(args[1:], session)
	case "list":
		return runJobList(args[1:], session)
	case "end":
		return runJobEnd(args[1:], session)
	default:
		return 0, &usageError{fmt.Sprintf("unrecognized subcommand '%s'", args[0]), jobUsage}
	}
}

func runJobStart(args []string, session *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("job start", session)
	taskFile := cliargs.OptString{Name: "task-file", Placeholder: "PATH"}
	parentIssue := cliargs.OptString{Name: "parent-issue", Placeholder: "ISSUE"}
	newParent := cliargs.OptString{Name: "new-parent", Placeholder: "TITLE"}
	repo := cliargs.OptString{Name: "repo", Placeholder: "REPO"}
	key := cliargs.OptString{Name: "key", Placeholder: "KEY"}
	model := cliargs.OptString{Name: "model", Placeholder: "MODEL"}
	effort := cliargs.OptString{Name: "effort", Placeholder: "EFFORT"}
	fs.Var(&taskFile, "task-file", "File with the lead's task")
	fs.Var(&parentIssue, "parent-issue", "The job's parent issue")
	fs.Var(&newParent, "new-parent", "Create the parent issue with this title")
	fs.Var(&repo, "repo", "The job's repo under ~/dev")
	fs.Var(&key, "key", "Dedup key")
	fs.Var(&model, "model", "Model passed to the agent as --model")
	fs.Var(&effort, "effort", "Effort passed to the agent as --effort")
	got, helped, err := parse(fs, args, jobStartHelp, jobStartUsage, []string{"JOB"}, &taskFile)
	if err != nil || helped {
		return exit.Ok, err
	}
	return cmd.JobStart(herdr.New(session.Ptr()), cmd.JobStartArgs{
		Job: got[0], TaskFile: taskFile.Value, ParentIssue: parentIssue.Ptr(), NewParent: newParent.Ptr(),
		Repo: repo.Ptr(), Key: key.Ptr(), Model: model.Ptr(), Effort: effort.Ptr()})
}

func runJobList(args []string, session *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("job list", session)
	target := cliargs.OptString{Name: "target", Placeholder: "TARGET"}
	asJSON := cliargs.Bool{Name: "json"}
	fs.Var(&target, "target", "Target whose ledger to read")
	fs.Var(&asJSON, "json", "Machine-readable output")
	_, helped, err := parse(fs, args, jobListHelp, jobListUsage, nil)
	if err != nil || helped {
		return exit.Ok, err
	}
	return cmd.JobList(cmd.JobListArgs{Target: target.Ptr(), JSON: asJSON.Value})
}

func runSpawn(args []string, session *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("spawn", session)
	taskFile := cliargs.OptString{Name: "task-file", Placeholder: "PATH"}
	cwd := cliargs.OptString{Name: "cwd", Placeholder: "DIR"}
	model := cliargs.OptString{Name: "model", Placeholder: "MODEL"}
	effort := cliargs.OptString{Name: "effort", Placeholder: "EFFORT"}
	fs.Var(&taskFile, "task-file", "File with the task")
	fs.Var(&cwd, "cwd", "Directory the worker runs in")
	fs.Var(&model, "model", "Model passed to the agent as --model")
	fs.Var(&effort, "effort", "Effort passed to the agent as --effort")
	got, helped, err := parse(fs, args, spawnHelp, spawnUsage, []string{"NAME"}, &taskFile, &cwd)
	if err != nil || helped {
		return exit.Ok, err
	}
	return cmd.Spawn(herdr.New(session.Ptr()), cmd.SpawnArgs{
		Name: got[0], TaskFile: taskFile.Value, Cwd: cwd.Value, Model: model.Ptr(), Effort: effort.Ptr()})
}

func runDone(args []string, session *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("done", session)
	reportFile := cliargs.OptString{Name: "report-file", Placeholder: "PATH"}
	abandon := cliargs.Bool{Name: "abandon"}
	fs.Var(&reportFile, "report-file", "File with the report")
	fs.Var(&abandon, "abandon", "Release the issue as abandoned")
	_, helped, err := parse(fs, args, doneHelp, doneUsage, nil)
	if err != nil || helped {
		return exit.Ok, err
	}
	return cmd.Done(herdr.New(session.Ptr()), cmd.DoneArgs{
		ReportFile: reportFile.Ptr(), Abandon: abandon.Value})
}

func runStatus(args []string, session *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("status", session)
	job := cliargs.OptString{Name: "job", Placeholder: "JOB"}
	target := cliargs.OptString{Name: "target", Placeholder: "TARGET"}
	asJSON := cliargs.Bool{Name: "json"}
	fs.Var(&job, "job", "Only this job's agents")
	fs.Var(&asJSON, "json", "Machine-readable output")
	fs.Var(&target, "target", "Target whose ledger to read")
	_, helped, err := parse(fs, args, statusHelp, statusUsage, nil)
	if err != nil || helped {
		return exit.Ok, err
	}
	return cmd.Status(herdr.New(session.Ptr()), cmd.StatusArgs{Job: job.Ptr(), JSON: asJSON.Value, Target: target.Ptr()})
}

func runWatch(args []string, session *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("watch", session)
	target := cliargs.OptString{Name: "target", Placeholder: "TARGET"}
	fs.Var(&target, "target", "Target whose ledger to read")
	_, helped, err := parse(fs, args, watchHelp, watchUsage, nil)
	if err != nil || helped {
		return exit.Ok, err
	}
	return cmd.Watch(herdr.New(session.Ptr()), cmd.WatchArgs{Target: target.Ptr()})
}

func runJobEnd(args []string, session *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("job end", session)
	reportFile := cliargs.OptString{Name: "report-file", Placeholder: "PATH"}
	abandon := cliargs.Bool{Name: "abandon"}
	force := cliargs.Bool{Name: "force"}
	fs.Var(&reportFile, "report-file", "File with the report")
	fs.Var(&abandon, "abandon", "End the job as abandoned")
	fs.Var(&force, "force", "Reclaim the job from outside")
	got, helped, err := parse(fs, args, jobEndHelp, jobEndUsage, []string{"[JOB]"})
	if err != nil || helped {
		return exit.Ok, err
	}
	var job *string
	if len(got) == 1 {
		job = &got[0]
	}
	return cmd.JobEnd(herdr.New(session.Ptr()), cmd.JobEndArgs{
		Job: job, ReportFile: reportFile.Ptr(), Abandon: abandon.Value, Force: force.Value})
}

func runWorktree(args []string, session *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("worktree", session)
	name := cliargs.OptString{Name: "name", Placeholder: "NAME"}
	branch := cliargs.OptString{Name: "branch", Placeholder: "NAME"}
	fs.Var(&name, "name", "Another worktree of the job in this repo")
	fs.Var(&branch, "branch", "Branch to create")
	got, helped, err := parse(fs, args, worktreeHelp, worktreeUsage, []string{"REPO"})
	if err != nil || helped {
		return exit.Ok, err
	}
	return cmd.Worktree(cmd.WorktreeArgs{Repo: got[0], Name: name.Ptr(), Branch: branch.Ptr()})
}

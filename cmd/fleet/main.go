// Command fleet runs and coordinates coding agents on a dataset machine
// through herdr. This file is the command-line definition and dispatch.
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

const longAbout = "Runs and coordinates coding agents on a dataset machine through herdr.\n\n" +
	"Every agent is started by fleet and carries its identity in FLEET_* " +
	"environment variables; messages between agents go through `fleet send`, " +
	"which adds the `[FROM: <agent>]` header. State lives in " +
	"$XDG_STATE_HOME/fleet/<repo>/fleet.db (~/.local/state when XDG_STATE_HOME is unset).\n\n" +
	"Exit codes, shared by every command:\n" +
	"  0  ok\n" +
	"  1  usage error or precondition refused (role, cap, resources, empty body)\n" +
	"  2  herdr gave no clear signal (timeout, stalled): the outcome is unknown, do not resend blindly\n" +
	"  3  target blocked (its screen is printed), or spawn stopped at an unknown screen\n" +
	"  4  target not found\n" +
	"  5  environment error (herdr, git or the database failed)"

const sessionHelp = "herdr session to talk to. Inside a herdr pane this is not needed: herdr " +
	"finds the pane's own session. Use it from cron or a plain shell"

const topUsage = "Usage: fleet [OPTIONS] <COMMAND>"

var topHelp = longAbout + "\n\n" + topUsage + `

Commands:
  send      ` + cmd.SendAbout + `
  spawn     ` + cmd.SpawnAbout + `
  done      ` + cmd.DoneAbout + `
  status    ` + cmd.StatusAbout + `
  watch     ` + cmd.WatchAbout + `
  close     ` + cmd.CloseAbout + `
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

const spawnUsage = "Usage: fleet spawn [OPTIONS] --task-file <PATH> <NAME>"

var spawnHelp = cmd.SpawnLongAbout + "\n\n" + spawnUsage + `

Arguments:
  <NAME>  Job id (from the orchestra) or worker name (from a lead). Only [a-z0-9-]

Options:
      --task-file <PATH>  File with the task, delivered as the agent's first message (with the header)
      --branch <NAME>     Branch for the job's worktree (lead only). Default: data/<job>
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
      --job <JOB>       Only this job's agents
      --json            Machine-readable output: a JSON array, one object per agent
      --repo <REPO>     Dataset repo whose ledger to read (` + "`owner/name` or `name`" + `); the ledger is
                        $XDG_STATE_HOME/fleet/<name>/fleet.db (~/.local/state when XDG_STATE_HOME is
                        unset). Defaults to FLEET_REPO, which every agent has
      --session <NAME>  ` + sessionHelp + `
  -h, --help            Print help
`

const watchUsage = "Usage: fleet watch [OPTIONS]"

var watchHelp = cmd.WatchLongAbout + "\n\n" + watchUsage + `

Options:
      --repo <REPO>     Dataset repo whose ledger to read (` + "`owner/name` or `name`" + `); the ledger is
                        $XDG_STATE_HOME/fleet/<name>/fleet.db (~/.local/state when XDG_STATE_HOME is
                        unset). Defaults to FLEET_REPO; cron must pass it
      --session <NAME>  ` + sessionHelp + `
  -h, --help            Print help
`

const closeUsage = "Usage: fleet close [OPTIONS] <JOB>"

var closeHelp = cmd.CloseLongAbout + "\n\n" + closeUsage + `

Arguments:
  <JOB>  Job id, as given to ` + "`fleet spawn`" + `

Options:
      --force           Close even if agents are still recorded as live (the cleanup after a failed spawn)
      --session <NAME>  ` + sessionHelp + `
  -h, --help            Print help
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
	case "close":
		return runClose(rest[1:], &session)
	case "worktree":
		return runWorktree(rest[1:], &session)
	default:
		return 0, &usageError{fmt.Sprintf("unrecognized subcommand '%s'", rest[0]), topUsage}
	}
}

// help is clap's implicit `help [COMMAND]` subcommand.
func help(args []string) (exit.Code, error) {
	helps := map[string]string{"send": sendHelp, "spawn": spawnHelp, "done": doneHelp, "status": statusHelp, "watch": watchHelp, "close": closeHelp, "worktree": worktreeHelp, "help": topHelp}
	if len(args) == 0 {
		fmt.Fprint(os.Stdout, topHelp)
		return exit.Ok, nil
	}
	text, ok := helps[args[0]]
	if !ok {
		return 0, &usageError{fmt.Sprintf("unrecognized subcommand '%s'", args[0]), topUsage}
	}
	fmt.Fprint(os.Stdout, text)
	return exit.Ok, nil
}

// parse parses a command's arguments: help goes to stdout with exit 0,
// any other failure is a usageError. names are the positionals in order;
// required are the flags that must be given (clap: a non-Option `#[arg]`),
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
			missing = append(missing, "  <"+name+">")
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

func runSpawn(args []string, session *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("spawn", session)
	taskFile := cliargs.OptString{Name: "task-file", Placeholder: "PATH"}
	branch := cliargs.OptString{Name: "branch", Placeholder: "NAME"}
	model := cliargs.OptString{Name: "model", Placeholder: "MODEL"}
	effort := cliargs.OptString{Name: "effort", Placeholder: "EFFORT"}
	fs.Var(&taskFile, "task-file", "File with the task")
	fs.Var(&branch, "branch", "Branch for the job's worktree (lead only)")
	fs.Var(&model, "model", "Model passed to the agent as --model")
	fs.Var(&effort, "effort", "Effort passed to the agent as --effort")
	got, helped, err := parse(fs, args, spawnHelp, spawnUsage, []string{"NAME"}, &taskFile)
	if err != nil || helped {
		return exit.Ok, err
	}
	return cmd.Spawn(herdr.New(session.Ptr()), cmd.SpawnArgs{
		Name: got[0], TaskFile: taskFile.Value, Branch: branch.Ptr(), Model: model.Ptr(), Effort: effort.Ptr()})
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
	repo := cliargs.OptString{Name: "repo", Placeholder: "REPO"}
	asJSON := cliargs.Bool{Name: "json"}
	fs.Var(&job, "job", "Only this job's agents")
	fs.Var(&asJSON, "json", "Machine-readable output")
	fs.Var(&repo, "repo", "Dataset repo whose ledger to read")
	_, helped, err := parse(fs, args, statusHelp, statusUsage, nil)
	if err != nil || helped {
		return exit.Ok, err
	}
	return cmd.Status(herdr.New(session.Ptr()), cmd.StatusArgs{Job: job.Ptr(), JSON: asJSON.Value, Repo: repo.Ptr()})
}

func runWatch(args []string, session *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("watch", session)
	repo := cliargs.OptString{Name: "repo", Placeholder: "REPO"}
	fs.Var(&repo, "repo", "Dataset repo whose ledger to read")
	_, helped, err := parse(fs, args, watchHelp, watchUsage, nil)
	if err != nil || helped {
		return exit.Ok, err
	}
	return cmd.Watch(herdr.New(session.Ptr()), cmd.WatchArgs{Repo: repo.Ptr()})
}

func runClose(args []string, session *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("close", session)
	force := cliargs.Bool{Name: "force"}
	fs.Var(&force, "force", "Close even if agents are still recorded as live")
	got, helped, err := parse(fs, args, closeHelp, closeUsage, []string{"JOB"})
	if err != nil || helped {
		return exit.Ok, err
	}
	return cmd.Close(herdr.New(session.Ptr()), cmd.CloseArgs{Job: got[0], Force: force.Value})
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

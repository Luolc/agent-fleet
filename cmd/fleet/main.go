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
	"which adds the `[FROM: <agent>]` header. State lives in ~/scratch/<repo>/fleet.db.\n\n" +
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
  send    ` + cmd.SendAbout + `
  status  ` + cmd.StatusAbout + `
  help    Print this message or the help of the given subcommand(s)

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

const statusUsage = "Usage: fleet status [OPTIONS]"

var statusHelp = cmd.StatusLongAbout + "\n\n" + statusUsage + `

Options:
      --job <JOB>       Only this job's agents
      --json            Machine-readable output: a JSON array, one object per agent
      --repo <REPO>     Dataset repo whose ledger to read (` + "`owner/name` or `name`" + `); the ledger is
                        ~/scratch/<name>/fleet.db. Defaults to FLEET_REPO, which every agent has
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
	case "status":
		return runStatus(rest[1:], &session)
	default:
		return 0, &usageError{fmt.Sprintf("unrecognized subcommand '%s'", rest[0]), topUsage}
	}
}

// help is clap's implicit `help [COMMAND]` subcommand.
func help(args []string) (exit.Code, error) {
	helps := map[string]string{"send": sendHelp, "status": statusHelp, "help": topHelp}
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
// any other failure is a usageError.
func parse(fs *flag.FlagSet, args []string, help, usage string, positionals int, names ...string) ([]string, bool, error) {
	got, err := cliargs.Parse(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(os.Stdout, help)
			return nil, true, nil
		}
		return nil, false, &usageError{err.Error(), usage}
	}
	if len(got) < positionals {
		missing := make([]string, 0, positionals-len(got))
		for _, name := range names[len(got):] {
			missing = append(missing, "  <"+name+">")
		}
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
	got, helped, err := parse(fs, args, sendHelp, sendUsage, 1, "TO")
	if err != nil || helped {
		return exit.Ok, err
	}
	return cmd.Send(herdr.New(session.Ptr()), cmd.SendArgs{To: got[0], File: file.Ptr()})
}

func runStatus(args []string, session *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("status", session)
	job := cliargs.OptString{Name: "job", Placeholder: "JOB"}
	repo := cliargs.OptString{Name: "repo", Placeholder: "REPO"}
	asJSON := cliargs.Bool{Name: "json"}
	fs.Var(&job, "job", "Only this job's agents")
	fs.Var(&asJSON, "json", "Machine-readable output")
	fs.Var(&repo, "repo", "Dataset repo whose ledger to read")
	_, helped, err := parse(fs, args, statusHelp, statusUsage, 0)
	if err != nil || helped {
		return exit.Ok, err
	}
	return cmd.Status(herdr.New(session.Ptr()), cmd.StatusArgs{Job: job.Ptr(), JSON: asJSON.Value, Repo: repo.Ptr()})
}

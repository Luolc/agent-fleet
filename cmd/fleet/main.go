// Command fleet runs and coordinates coding agents through herdr. This file
// is the command-line definition and dispatch.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/Luolc/agent-fleet/internal/atb"
	"github.com/Luolc/agent-fleet/internal/cliargs"
	"github.com/Luolc/agent-fleet/internal/cmd"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
	"github.com/Luolc/agent-fleet/internal/identity"
)

// version is the fleet release version.
const version = "0.2.9"

const longAbout = "Runs and coordinates coding agents through herdr.\n\n" +
	"A thread agent starts a job (`fleet job start`): its lead, which starts workers " +
	"(`fleet spawn`). Every agent fleet starts carries its identity in FLEET_* " +
	"environment variables; messages between agents go through `fleet send`, " +
	"which adds the `[FROM: <agent>]` header. A scope is one fleet: the herdr session " +
	"fleet-<scope>, the ledger $XDG_STATE_HOME/fleet/<scope>.db (~/.local/state when " +
	"XDG_STATE_HOME is unset) and the settings $XDG_CONFIG_HOME/fleet/<scope>.json. The " +
	"scope is --scope, else FLEET_SCOPE (which every agent has), else `main`.\n\n" +
	"Exit codes, shared by every command:\n" +
	"  0  ok\n" +
	"  1  usage error or precondition refused (role, cap, resources, empty body)\n" +
	"  2  herdr gave no clear signal (timeout, stalled): the outcome is unknown, do not resend blindly\n" +
	"  3  target blocked (its screen is printed), or spawn stopped at an unknown screen\n" +
	"  4  target not found\n" +
	"  5  environment error (herdr, atb, git or the database failed)"

const scopeHelp = "Scope to act in: its ledger, settings and herdr session fleet-<NAME>. " +
	"Default: FLEET_SCOPE, which every agent has, then `main`. Use it from a timer or a plain shell"

const topUsage = "Usage: fleet [OPTIONS] <COMMAND>"

var topHelp = longAbout + "\n\n" + topUsage + `

Commands:
  inbox      ` + cmd.InboxAbout + `
  thread     ` + cmd.ThreadAbout + `
  job        ` + cmd.JobAbout + `
  ask-human  ` + cmd.AskHumanAbout + `
  ask-thread ` + cmd.AskThreadAbout + `
  spawn      ` + cmd.SpawnAbout + `
  send       ` + cmd.SendAbout + `
  done       ` + cmd.DoneAbout + `
  worker     ` + cmd.WorkerAbout + `
  status     ` + cmd.StatusAbout + `
  report     ` + cmd.ReportAbout + `
  watch      ` + cmd.WatchAbout + `
  worktree   ` + cmd.WorktreeAbout + `
  prompt     ` + cmd.PromptAbout + `
  help       Print this message or the help of the given subcommand(s)

Options:
      --scope <NAME>    ` + scopeHelp + `
  -h, --help            Print help
  -V, --version         Print version
`

const sendUsage = "Usage: fleet send [OPTIONS] <TO>"

var sendHelp = cmd.SendLongAbout + "\n\n" + sendUsage + `

Arguments:
  <TO>  Name of the receiving agent, as shown by ` + "`fleet status`" + `

Options:
      --file <PATH>     Read the body from this file instead of stdin
      --scope <NAME>    ` + scopeHelp + `
  -h, --help            Print help
`

const inboxUsage = "Usage: fleet inbox [OPTIONS] <EVENT-FILE>"

var inboxHelp = cmd.InboxLongAbout + "\n\n" + inboxUsage + `

Arguments:
  <EVENT-FILE>  The event file the fednet client wrote: one JSON object with msg_id and payload

Options:
  -h, --help    Print help
`

const threadUsage = "Usage: fleet thread <COMMAND>"

var threadHelp = cmd.ThreadAbout + ".\n\n" + threadUsage + `

Commands:
  post         ` + cmd.ThreadPostAbout + `
  progress     ` + cmd.ThreadProgressAbout + `
  end          ` + cmd.ThreadEndAbout + `
  set-project  ` + cmd.ThreadSetProjectAbout + `
  relate       ` + cmd.ThreadRelateAbout + `
  handoff      ` + cmd.ThreadHandoffAbout + `
  list         ` + cmd.ThreadListAbout + `

Options:
  -h, --help  Print help
`

const threadPostUsage = "Usage: fleet thread post [OPTIONS] --body-file <PATH>\n       fleet thread post [OPTIONS] --deliver --thread <THREAD> --body-file <PATH>"

var threadPostHelp = cmd.ThreadPostLongAbout + "\n\n" + threadPostUsage + `

Options:
      --body-file <PATH>  The text to post, standard Markdown, at most 100 KiB
      --attach <PATH>     A file to upload with the text, passed to fednet as -file; repeatable
      --deliver           Post to the thread --thread names and deliver the text to the machine that owns it
      --thread <THREAD>   With --deliver, the thread to post to: CHANNEL/TS, or a Slack link to a message in it
      --scope <NAME>      ` + scopeHelp + `
  -h, --help              Print help
`

const threadProgressUsage = "Usage: fleet thread progress [OPTIONS] --title <TEXT> [--item <TEXT:STATE>]...\n       fleet thread progress [OPTIONS] --done [--title <TEXT>] [--item <TEXT:STATE>]..."

var threadProgressHelp = cmd.ThreadProgressLongAbout + "\n\n" + threadProgressUsage + `

Options:
      --title <TEXT>        The card's status now, about 10 to 20 characters
      --item <TEXT:STATE>   An item of the card, STATE one of doing, done, error; the whole card each call
      --done                Complete the card; --title and --item, when given, are the closed card's wording
      --scope <NAME>        ` + scopeHelp + `
  -h, --help                Print help
`

const threadEndUsage = "Usage: fleet thread end [OPTIONS] --summary-file <PATH>"

var threadEndHelp = cmd.ThreadEndLongAbout + "\n\n" + threadEndUsage + `

Options:
      --summary-file <PATH>  The session's summary, written to the thread ticket and given to the next session
      --force                Finish the local cleanup even when a Linear step keeps failing; the steps left are printed
      --asked-to-end         End even with a question pending or a job open; only when the people in the thread asked you to end, or fleet watch reclaims the session
      --scope <NAME>         ` + scopeHelp + `
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

const threadHandoffUsage = "Usage: fleet thread handoff [OPTIONS] (--repo <REPO> | --initiative <INITIATIVE> | --channel <CHANNEL>) --file <PATH> [--mention <USER>]..."

var threadHandoffHelp = cmd.ThreadHandoffLongAbout + "\n\n" + threadHandoffUsage + `

Options:
      --repo <REPO>              The repo whose channel takes the work: <repo prefix><REPO>
      --initiative <INITIATIVE>  The initiative whose channel takes the work: <initiative prefix><INITIATIVE>
      --channel <CHANNEL>        The channel that takes the work, by its name without the #
      --file <PATH>              The task: what to do and why, the people's words with their thread key and ts
      --mention <USER>           A Slack user id to mention in the new thread, when that person asked for the handoff; repeatable
      --scope <NAME>             ` + scopeHelp + `
  -h, --help                     Print help
`

const threadListUsage = "Usage: fleet thread list [OPTIONS] --channel <CHANNEL>"

var threadListHelp = cmd.ThreadListLongAbout + "\n\n" + threadListUsage + `

Options:
      --channel <CHANNEL>  The channel, by its name without the #
      --limit <N>          At most N threads, the newest; fednet's default without it
      --scope <NAME>       ` + scopeHelp + `
  -h, --help               Print help
`

const askHumanUsage = "Usage: fleet ask-human [OPTIONS] (--card <PATH> [--for <ID>] | --close <ID> [--note <TEXT>])"

var askHumanHelp = cmd.AskHumanLongAbout + "\n\n" + askHumanUsage + `

Options:
      --card <PATH>     The question as a card's JSON, posted to your thread with fednet
      --for <ID>        The lead's question, pending in your thread, the card asks for
      --close <ID>      Take back the card of your pending question <ID>
      --note <TEXT>     With --close, a note the card shows
      --file <PATH>     Deprecated: a plain-text question, posted as a card with no options (a lead's: as ask-thread)
      --approval        Refused: approval cards are not supported yet
      --scope <NAME>    ` + scopeHelp + `
  -h, --help            Print help
`

const askThreadUsage = "Usage: fleet ask-thread [OPTIONS] --card <PATH>"

var askThreadHelp = cmd.AskThreadLongAbout + "\n\n" + askThreadUsage + `

Options:
      --card <PATH>     The question as a card's JSON, delivered to your home thread's agent
      --scope <NAME>    ` + scopeHelp + `
  -h, --help            Print help
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
      --repo <REPO>       Directory name of the main checkout (under ~/dev by default; for x-repo-<I>
                          without one, ~/x-repo/<I>): a single-repo job, the lead runs there. Without
                          it the job is cross-repo and the lead runs in <job>/ of the initiative's
                          checkout (~/x-repo/<I>/<job>/ by default). Required without FLEET_ROLE
      --key <KEY>         Dedup key: refused when an open job of the scope has the same key
      --model <MODEL>     Model passed to the agent as --model. Default: the scope's models.lead,
                          else the agent's own
      --effort <EFFORT>   Effort passed to the agent as --effort. Default: the scope's models.lead,
                          else the agent's own
      --light             Skip the load check (memory is still checked) for a lead that only reads;
                          its first message tells it not to build, test or run anything CPU-heavy
      --scope <NAME>      ` + scopeHelp + `
  -h, --help              Print help
`

const jobListUsage = "Usage: fleet job list [OPTIONS]"

var jobListHelp = cmd.JobListLongAbout + "\n\n" + jobListUsage + `

Options:
      --all              Every open job of the scope, not only those of your thread's channel
      --json             Machine-readable output: a JSON array, one object per job
      --scope <NAME>     ` + scopeHelp + `
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
      --close-parent <OUTCOME>
                            Close the job's parent issue as done or abandoned; without it the
                            parent returns to the state it had before the job claimed it, except
                            that a parent the job created (--new-parent) is closed as done when
                            the job ends done
      --force               Reclaim the job from outside (a thread agent, or no FLEET_ROLE): no report,
                            no Linear step; the cleanup after a failed start or a lost lead
      --scope <NAME>        ` + scopeHelp + `
  -h, --help                Print help
`

const workerUsage = "Usage: fleet worker <COMMAND>"

var workerHelp = cmd.WorkerAbout + ".\n\n" + workerUsage + `

Commands:
  end  ` + cmd.WorkerEndAbout + `

Options:
  -h, --help  Print help
`

const workerEndUsage = "Usage: fleet worker end [OPTIONS] <NAME>"

var workerEndHelp = cmd.WorkerEndLongAbout + "\n\n" + workerEndUsage + `

Arguments:
  <NAME>  The worker's full name, <job>-<name>, as ` + "`fleet status`" + ` shows it

Options:
      --report-file <PATH>  Written to the worker's work order before it is released as abandoned;
                            without it, a fixed line naming you
      --scope <NAME>        ` + scopeHelp + `
  -h, --help                Print help
`

const spawnUsage = "Usage: fleet spawn [OPTIONS] --task-file <PATH> --cwd <DIR> <NAME>"

var spawnHelp = cmd.SpawnLongAbout + "\n\n" + spawnUsage + `

Arguments:
  <NAME>  Worker name within the job; the worker is named <job>-<NAME>. Only [a-z0-9-]

Options:
      --task-file <PATH>  File with the task, delivered as the worker's first message (with the header)
      --cwd <DIR>         Directory the worker runs in; must exist (typically a path from ` + "`fleet worktree`" + `)
      --model <MODEL>     Model passed to the agent as --model. Default: the scope's models.worker,
                          else the agent's own
      --effort <EFFORT>   Effort passed to the agent as --effort. Default: the scope's models.worker,
                          else the agent's own
      --light             Skip the load check (memory is still checked) for a worker that only reads;
                          its first message tells it not to build, test or run anything CPU-heavy
      --scope <NAME>      ` + scopeHelp + `
  -h, --help              Print help
`

const doneUsage = "Usage: fleet done [OPTIONS]"

var doneHelp = cmd.DoneLongAbout + "\n\n" + doneUsage + `

Options:
      --report-file <PATH>  Your report, named in the message so the parent can read it, and written
                            to FLEET_ISSUE with atb first when that is set (then it is required)
      --abandon             Release FLEET_ISSUE as abandoned instead of done
      --scope <NAME>        ` + scopeHelp + `
  -h, --help                Print help
`

const statusUsage = "Usage: fleet status [OPTIONS]"

var statusHelp = cmd.StatusLongAbout + "\n\n" + statusUsage + `

Options:
      --job <JOB>        Only this job and its agents
      --json             Machine-readable output: {"jobs": [...], "agents": [...]}
      --scope <NAME>     ` + scopeHelp + `
  -h, --help             Print help
`

const reportUsage = "Usage: fleet report [OPTIONS]"

var reportHelp = cmd.ReportLongAbout + "\n\n" + reportUsage + `

Options:
      --json             Machine-readable output: {"jobs": [...], "threads": [...], "total": {...}}
      --scope <NAME>     ` + scopeHelp + `
  -h, --help             Print help
`

const watchUsage = "Usage: fleet watch [OPTIONS]"

var watchHelp = cmd.WatchLongAbout + "\n\n" + watchUsage + `

Options:
      --scope <NAME>     ` + scopeHelp + `
  -h, --help             Print help
`

const worktreeUsage = "Usage: fleet worktree [OPTIONS] <REPO>"

var worktreeHelp = cmd.WorktreeLongAbout + "\n\n" + worktreeUsage + `

Arguments:
  <REPO>  Directory name of the main checkout (under ~/dev by default), or an initiative's repo

Options:
      --name <NAME>     Another worktree of the job in this repo: <repo>/<job>-<name> (under ~/wt by default). Only [a-z0-9-]
      --branch <NAME>   Branch to create. Default: <job>, or <job>-<name> with --name
      --detach <REF>    Check out <REF> (a commit, such as a PR's head SHA) detached, no branch; not with --branch
      --scope <NAME>    ` + scopeHelp + `
  -h, --help            Print help
`

const promptUsage = "Usage: fleet prompt <COMMAND>"

var promptHelp = cmd.PromptAbout + ".\n\n" + promptUsage + `

Commands:
  show  ` + cmd.PromptShowAbout + `

Options:
  -h, --help  Print help
`

const promptShowUsage = "Usage: fleet prompt show [OPTIONS] [NAME]"

var promptShowHelp = cmd.PromptShowLongAbout + "\n\n" + promptShowUsage + `

Arguments:
  [NAME]  One of common, thread, lead, worker, or watch:<name> for a watch's prompt; without it, all of them

Options:
      --builtin         Only fleet's built-in text of NAME, byte for byte
      --scope <NAME>    ` + scopeHelp + `
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
// `-h`/`--help` through flag.ErrHelp, with the global `--scope`.
func flagSet(name string, scope *cliargs.OptString) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Var(scope, "scope", scopeHelp)
	return fs
}

// scoped applies --scope, when given, as FLEET_SCOPE for the command, and
// returns herdr for the scope's session.
func scoped(scope *cliargs.OptString) (*herdr.Herdr, error) {
	if scope.Given {
		if _, err := identity.CheckScope(scope.Value); err != nil {
			return nil, err
		}
		if err := os.Setenv("FLEET_SCOPE", scope.Value); err != nil {
			return nil, exit.Environmentf("cannot set FLEET_SCOPE: %v", err)
		}
	}
	name, err := identity.Scope()
	if err != nil {
		return nil, err
	}
	session := identity.Session(name)
	return herdr.New(&session), nil
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
		atb.WriteStderr(os.Stderr, err)
		return int(failure.Code)
	}
	fmt.Fprintf(os.Stderr, "fleet: %s\n", err)
	return int(exit.Environment)
}

func dispatch(args []string) (exit.Code, error) {
	scope := cliargs.OptString{Name: "scope", Placeholder: "NAME"}
	fs := flagSet("fleet", &scope)
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
		return runInbox(rest[1:], &scope)
	case "thread":
		return runThread(rest[1:], &scope)
	case "job":
		return runJob(rest[1:], &scope)
	case "ask-human":
		return runAskHuman(rest[1:], &scope)
	case "ask-thread":
		return runAskThread(rest[1:], &scope)
	case "send":
		return runSend(rest[1:], &scope)
	case "spawn":
		return runSpawn(rest[1:], &scope)
	case "done":
		return runDone(rest[1:], &scope)
	case "worker":
		return runWorker(rest[1:], &scope)
	case "status":
		return runStatus(rest[1:], &scope)
	case "report":
		return runReport(rest[1:], &scope)
	case "watch":
		return runWatch(rest[1:], &scope)
	case "worktree":
		return runWorktree(rest[1:], &scope)
	case "prompt":
		return runPrompt(rest[1:], &scope)
	default:
		return 0, &usageError{fmt.Sprintf("unrecognized subcommand '%s'", rest[0]), topUsage}
	}
}

// help is clap's implicit `help [COMMAND]` subcommand.
func help(args []string) (exit.Code, error) {
	helps := map[string]string{"inbox": inboxHelp, "thread": threadHelp, "job": jobHelp, "ask-human": askHumanHelp, "ask-thread": askThreadHelp, "send": sendHelp, "spawn": spawnHelp, "done": doneHelp, "worker": workerHelp,
		"status": statusHelp, "report": reportHelp, "watch": watchHelp, "worktree": worktreeHelp, "prompt": promptHelp, "help": topHelp}
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
	if args[0] == "worker" && len(args) > 1 && args[1] == "end" {
		fmt.Fprint(os.Stdout, workerEndHelp)
		return exit.Ok, nil
	}
	if args[0] == "prompt" && len(args) > 1 && args[1] == "show" {
		fmt.Fprint(os.Stdout, promptShowHelp)
		return exit.Ok, nil
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

func runSend(args []string, scope *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("send", scope)
	file := cliargs.OptString{Name: "file", Placeholder: "PATH"}
	fs.Var(&file, "file", "Read the body from this file instead of stdin")
	got, helped, err := parse(fs, args, sendHelp, sendUsage, []string{"TO"})
	if err != nil || helped {
		return exit.Ok, err
	}
	h, err := scoped(scope)
	if err != nil {
		return 0, err
	}
	return cmd.Send(h, cmd.SendArgs{To: got[0], File: file.Ptr()})
}

func runInbox(args []string, scope *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("inbox", scope)
	got, helped, err := parse(fs, args, inboxHelp, inboxUsage, []string{"EVENT-FILE"})
	if err != nil || helped {
		return exit.Ok, err
	}
	if scope.Given {
		return 0, exit.Refusedf("--scope does not apply to inbox: the scope comes from the message")
	}
	return cmd.Inbox(cmd.InboxArgs{File: got[0]})
}

var threadHelps = map[string]string{"post": threadPostHelp, "progress": threadProgressHelp, "end": threadEndHelp,
	"set-project": threadSetProjectHelp, "relate": threadRelateHelp, "handoff": threadHandoffHelp, "list": threadListHelp}

// runThread dispatches `fleet thread <COMMAND>`.
func runThread(args []string, scope *cliargs.OptString) (exit.Code, error) {
	if len(args) == 0 {
		return 0, &usageError{"'fleet thread' requires a subcommand but one was not provided", threadUsage}
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, threadHelp)
		return exit.Ok, nil
	case "post":
		return runThreadPost(args[1:], scope)
	case "progress":
		return runThreadProgress(args[1:], scope)
	case "end":
		fs := flagSet("thread end", scope)
		summary := cliargs.OptString{Name: "summary-file", Placeholder: "PATH"}
		force := cliargs.Bool{Name: "force"}
		fs.Var(&summary, "summary-file", "The session's summary")
		fs.Var(&force, "force", "Finish the local cleanup even when a Linear step keeps failing")
		asked := cliargs.Bool{Name: "asked-to-end"}
		fs.Var(&asked, "asked-to-end", "End even with a question pending or a job open")
		_, helped, err := parse(fs, args[1:], threadEndHelp, threadEndUsage, nil, &summary)
		if err != nil || helped {
			return exit.Ok, err
		}
		h, err := scoped(scope)
		if err != nil {
			return 0, err
		}
		return cmd.ThreadEnd(h, cmd.ThreadEndArgs{SummaryFile: summary.Value, Force: force.Value, AskedToEnd: asked.Value})
	case "set-project":
		got, helped, err := parse(flagSet("thread set-project", scope), args[1:], threadSetProjectHelp,
			threadSetProjectUsage, []string{"PROJECT"})
		if err != nil || helped {
			return exit.Ok, err
		}
		if _, err := scoped(scope); err != nil {
			return 0, err
		}
		return cmd.ThreadSetProject(got[0])
	case "relate":
		got, helped, err := parse(flagSet("thread relate", scope), args[1:], threadRelateHelp,
			threadRelateUsage, []string{"ISSUE"})
		if err != nil || helped {
			return exit.Ok, err
		}
		if _, err := scoped(scope); err != nil {
			return 0, err
		}
		return cmd.ThreadRelate(got[0])
	case "handoff":
		return runThreadHandoff(args[1:], scope)
	case "list":
		return runThreadList(args[1:], scope)
	default:
		return 0, &usageError{fmt.Sprintf("unrecognized subcommand '%s'", args[0]), threadUsage}
	}
}

func runThreadHandoff(args []string, scope *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("thread handoff", scope)
	repo := cliargs.OptString{Name: "repo", Placeholder: "REPO"}
	initiative := cliargs.OptString{Name: "initiative", Placeholder: "INITIATIVE"}
	channel := cliargs.OptString{Name: "channel", Placeholder: "CHANNEL"}
	file := cliargs.OptString{Name: "file", Placeholder: "PATH"}
	fs.Var(&repo, "repo", "The repo whose channel takes the work")
	fs.Var(&initiative, "initiative", "The initiative whose channel takes the work")
	fs.Var(&channel, "channel", "The channel that takes the work")
	fs.Var(&file, "file", "The task")
	var mention cliargs.Strings
	fs.Var(&mention, "mention", "A Slack user id to mention in the new thread")
	_, helped, err := parse(fs, args, threadHandoffHelp, threadHandoffUsage, nil, &file)
	if err != nil || helped {
		return exit.Ok, err
	}
	if given := btoi(repo.Given) + btoi(initiative.Given) + btoi(channel.Given); given != 1 {
		return 0, &usageError{"exactly one of --repo <REPO>, --initiative <INITIATIVE> and --channel <CHANNEL> is required",
			threadHandoffUsage}
	}
	h, err := scoped(scope)
	if err != nil {
		return 0, err
	}
	return cmd.ThreadHandoff(h, cmd.ThreadHandoffArgs{Repo: repo.Value, Initiative: initiative.Value, Channel: channel.Value,
		File: file.Value, Mentions: mention.Values})
}

func runThreadList(args []string, scope *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("thread list", scope)
	channel := cliargs.OptString{Name: "channel", Placeholder: "CHANNEL"}
	limit := cliargs.OptString{Name: "limit", Placeholder: "N"}
	fs.Var(&channel, "channel", "The channel, by its name")
	fs.Var(&limit, "limit", "At most N threads")
	_, helped, err := parse(fs, args, threadListHelp, threadListUsage, nil, &channel)
	if err != nil || helped {
		return exit.Ok, err
	}
	n := 0
	if limit.Given {
		if n, err = strconv.Atoi(limit.Value); err != nil || n < 1 {
			return 0, &usageError{fmt.Sprintf("--limit %q is not a positive number", limit.Value), threadListUsage}
		}
	}
	if _, err := scoped(scope); err != nil {
		return 0, err
	}
	return cmd.ThreadList(cmd.ThreadListArgs{Channel: channel.Value, Limit: n})
}

func runThreadPost(args []string, scope *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("thread post", scope)
	body := cliargs.OptString{Name: "body-file", Placeholder: "PATH"}
	var attach cliargs.Strings
	deliver := cliargs.Bool{Name: "deliver"}
	thread := cliargs.OptString{Name: "thread", Placeholder: "THREAD"}
	fs.Var(&body, "body-file", "The text to post")
	fs.Var(&attach, "attach", "A file to upload with the text")
	fs.Var(&deliver, "deliver", "Post to another thread and deliver the text to the machine that owns it")
	fs.Var(&thread, "thread", "With --deliver, the thread to post to")
	_, helped, err := parse(fs, args, threadPostHelp, threadPostUsage, nil, &body)
	if err != nil || helped {
		return exit.Ok, err
	}
	if deliver.Value != thread.Given {
		return 0, &usageError{"--deliver and --thread <THREAD> go together", threadPostUsage}
	}
	if deliver.Value && len(attach.Values) > 0 {
		return 0, &usageError{"--deliver takes no --attach", threadPostUsage}
	}
	key, ok := cmd.ThreadKeyOf(thread.Value)
	if thread.Given && !ok {
		return 0, &usageError{fmt.Sprintf("--thread %q is neither a thread key CHANNEL/TS, such as C012AB3CD/1700000000.000100, "+
			"nor a Slack link to a message, such as https://<workspace>.slack.com/archives/C012AB3CD/p1700000000000100", thread.Value),
			threadPostUsage}
	}
	if _, err := scoped(scope); err != nil {
		return 0, err
	}
	return cmd.ThreadPost(cmd.ThreadPostArgs{BodyFile: body.Value, Attach: attach.Values, Thread: key})
}

func runThreadProgress(args []string, scope *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("thread progress", scope)
	title := cliargs.OptString{Name: "title", Placeholder: "TEXT"}
	var items cliargs.Strings
	done := cliargs.Bool{Name: "done"}
	fs.Var(&title, "title", "The card's status now")
	fs.Var(&items, "item", "An item of the card, <text>:<state>")
	fs.Var(&done, "done", "Complete the card")
	_, helped, err := parse(fs, args, threadProgressHelp, threadProgressUsage, nil)
	if err != nil || helped {
		return exit.Ok, err
	}
	if _, err := scoped(scope); err != nil {
		return 0, err
	}
	return cmd.ThreadProgress(cmd.ThreadProgressArgs{Title: title.Ptr(), Items: items.Values, Done: done.Value})
}

func runAskHuman(args []string, scope *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("ask-human", scope)
	card := cliargs.OptString{Name: "card", Placeholder: "PATH"}
	forID := cliargs.OptString{Name: "for", Placeholder: "ID"}
	closeID := cliargs.OptString{Name: "close", Placeholder: "ID"}
	note := cliargs.OptString{Name: "note", Placeholder: "TEXT"}
	file := cliargs.OptString{Name: "file", Placeholder: "PATH"}
	approval := cliargs.Bool{Name: "approval"}
	fs.Var(&card, "card", "The question as a card's JSON")
	fs.Var(&forID, "for", "The lead's question the card asks for")
	fs.Var(&closeID, "close", "Take back this question's card")
	fs.Var(&note, "note", "A note the taken-back card shows")
	fs.Var(&file, "file", "Deprecated: a plain-text question")
	fs.Var(&approval, "approval", "Ask for an approval card (not supported yet)")
	_, helped, err := parse(fs, args, askHumanHelp, askHumanUsage, nil)
	if err != nil || helped {
		return exit.Ok, err
	}
	switch {
	case !approval.Value && btoi(card.Given)+btoi(closeID.Given)+btoi(file.Given) != 1:
		return 0, &usageError{"exactly one of --card <PATH> and --close <ID> is required", askHumanUsage}
	case forID.Given && !card.Given:
		return 0, &usageError{"--for <ID> goes with --card <PATH>", askHumanUsage}
	case note.Given && !closeID.Given:
		return 0, &usageError{"--note <TEXT> goes with --close <ID>", askHumanUsage}
	}
	a := cmd.AskHumanArgs{Card: card.Value, Note: note.Value, File: file.Value, Approval: approval.Value}
	for _, id := range []struct {
		flag *cliargs.OptString
		to   *int64
	}{{&forID, &a.For}, {&closeID, &a.Close}} {
		if !id.flag.Given {
			continue
		}
		n, err := strconv.ParseInt(id.flag.Value, 10, 64)
		if err != nil || n <= 0 {
			return 0, &usageError{fmt.Sprintf("--%s: %q is not a question id", id.flag.Name, id.flag.Value), askHumanUsage}
		}
		*id.to = n
	}
	h, err := scoped(scope)
	if err != nil {
		return 0, err
	}
	return cmd.AskHuman(h, a)
}

// btoi is 1 for true.
func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func runAskThread(args []string, scope *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("ask-thread", scope)
	card := cliargs.OptString{Name: "card", Placeholder: "PATH"}
	fs.Var(&card, "card", "The question as a card's JSON")
	_, helped, err := parse(fs, args, askThreadHelp, askThreadUsage, nil, &card)
	if err != nil || helped {
		return exit.Ok, err
	}
	h, err := scoped(scope)
	if err != nil {
		return 0, err
	}
	return cmd.AskThread(h, cmd.AskThreadArgs{Card: card.Value})
}

// runJob dispatches `fleet job <COMMAND>`.
func runJob(args []string, scope *cliargs.OptString) (exit.Code, error) {
	if len(args) == 0 {
		return 0, &usageError{"'fleet job' requires a subcommand but one was not provided", jobUsage}
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, jobHelp)
		return exit.Ok, nil
	case "start":
		return runJobStart(args[1:], scope)
	case "list":
		return runJobList(args[1:], scope)
	case "end":
		return runJobEnd(args[1:], scope)
	default:
		return 0, &usageError{fmt.Sprintf("unrecognized subcommand '%s'", args[0]), jobUsage}
	}
}

func runJobStart(args []string, scope *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("job start", scope)
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
	fs.Var(&repo, "repo", "The directory name of the job's main checkout")
	fs.Var(&key, "key", "Dedup key")
	fs.Var(&model, "model", "Model passed to the agent as --model")
	fs.Var(&effort, "effort", "Effort passed to the agent as --effort")
	light := cliargs.Bool{Name: "light"}
	fs.Var(&light, "light", "Skip the load check for a lead that only reads")
	got, helped, err := parse(fs, args, jobStartHelp, jobStartUsage, []string{"JOB"}, &taskFile)
	if err != nil || helped {
		return exit.Ok, err
	}
	h, err := scoped(scope)
	if err != nil {
		return 0, err
	}
	return cmd.JobStart(h, cmd.JobStartArgs{
		Job: got[0], TaskFile: taskFile.Value, ParentIssue: parentIssue.Ptr(), NewParent: newParent.Ptr(),
		Repo: repo.Ptr(), Key: key.Ptr(), Model: model.Ptr(), Effort: effort.Ptr(), Light: light.Value})
}

func runJobList(args []string, scope *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("job list", scope)
	all := cliargs.Bool{Name: "all"}
	asJSON := cliargs.Bool{Name: "json"}
	fs.Var(&all, "all", "Every open job of the scope")
	fs.Var(&asJSON, "json", "Machine-readable output")
	_, helped, err := parse(fs, args, jobListHelp, jobListUsage, nil)
	if err != nil || helped {
		return exit.Ok, err
	}
	if _, err := scoped(scope); err != nil {
		return 0, err
	}
	return cmd.JobList(cmd.JobListArgs{All: all.Value, JSON: asJSON.Value})
}

func runSpawn(args []string, scope *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("spawn", scope)
	taskFile := cliargs.OptString{Name: "task-file", Placeholder: "PATH"}
	cwd := cliargs.OptString{Name: "cwd", Placeholder: "DIR"}
	model := cliargs.OptString{Name: "model", Placeholder: "MODEL"}
	effort := cliargs.OptString{Name: "effort", Placeholder: "EFFORT"}
	fs.Var(&taskFile, "task-file", "File with the task")
	fs.Var(&cwd, "cwd", "Directory the worker runs in")
	fs.Var(&model, "model", "Model passed to the agent as --model")
	fs.Var(&effort, "effort", "Effort passed to the agent as --effort")
	light := cliargs.Bool{Name: "light"}
	fs.Var(&light, "light", "Skip the load check for a worker that only reads")
	got, helped, err := parse(fs, args, spawnHelp, spawnUsage, []string{"NAME"}, &taskFile, &cwd)
	if err != nil || helped {
		return exit.Ok, err
	}
	h, err := scoped(scope)
	if err != nil {
		return 0, err
	}
	return cmd.Spawn(h, cmd.SpawnArgs{
		Name: got[0], TaskFile: taskFile.Value, Cwd: cwd.Value, Model: model.Ptr(), Effort: effort.Ptr(),
		Light: light.Value})
}

func runDone(args []string, scope *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("done", scope)
	reportFile := cliargs.OptString{Name: "report-file", Placeholder: "PATH"}
	abandon := cliargs.Bool{Name: "abandon"}
	fs.Var(&reportFile, "report-file", "File with the report")
	fs.Var(&abandon, "abandon", "Release the issue as abandoned")
	_, helped, err := parse(fs, args, doneHelp, doneUsage, nil)
	if err != nil || helped {
		return exit.Ok, err
	}
	h, err := scoped(scope)
	if err != nil {
		return 0, err
	}
	return cmd.Done(h, cmd.DoneArgs{
		ReportFile: reportFile.Ptr(), Abandon: abandon.Value})
}

func runStatus(args []string, scope *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("status", scope)
	job := cliargs.OptString{Name: "job", Placeholder: "JOB"}
	asJSON := cliargs.Bool{Name: "json"}
	fs.Var(&job, "job", "Only this job's agents")
	fs.Var(&asJSON, "json", "Machine-readable output")
	_, helped, err := parse(fs, args, statusHelp, statusUsage, nil)
	if err != nil || helped {
		return exit.Ok, err
	}
	h, err := scoped(scope)
	if err != nil {
		return 0, err
	}
	return cmd.Status(h, cmd.StatusArgs{Job: job.Ptr(), JSON: asJSON.Value})
}

func runReport(args []string, scope *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("report", scope)
	asJSON := cliargs.Bool{Name: "json"}
	fs.Var(&asJSON, "json", "Machine-readable output")
	_, helped, err := parse(fs, args, reportHelp, reportUsage, nil)
	if err != nil || helped {
		return exit.Ok, err
	}
	if _, err := scoped(scope); err != nil {
		return 0, err
	}
	return cmd.AttentionReport(cmd.ReportArgs{JSON: asJSON.Value})
}

func runWatch(args []string, scope *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("watch", scope)
	_, helped, err := parse(fs, args, watchHelp, watchUsage, nil)
	if err != nil || helped {
		return exit.Ok, err
	}
	h, err := scoped(scope)
	if err != nil {
		return 0, err
	}
	return cmd.Watch(h)
}

func runJobEnd(args []string, scope *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("job end", scope)
	reportFile := cliargs.OptString{Name: "report-file", Placeholder: "PATH"}
	abandon := cliargs.Bool{Name: "abandon"}
	closeParent := cliargs.OptString{Name: "close-parent", Placeholder: "OUTCOME"}
	force := cliargs.Bool{Name: "force"}
	fs.Var(&reportFile, "report-file", "File with the report")
	fs.Var(&abandon, "abandon", "End the job as abandoned")
	fs.Var(&closeParent, "close-parent", "Close the parent issue as done or abandoned")
	fs.Var(&force, "force", "Reclaim the job from outside")
	got, helped, err := parse(fs, args, jobEndHelp, jobEndUsage, []string{"[JOB]"})
	if err != nil || helped {
		return exit.Ok, err
	}
	var job *string
	if len(got) == 1 {
		job = &got[0]
	}
	h, err := scoped(scope)
	if err != nil {
		return 0, err
	}
	return cmd.JobEnd(h, cmd.JobEndArgs{
		Job: job, ReportFile: reportFile.Ptr(), Abandon: abandon.Value,
		CloseParent: closeParent.Ptr(), Force: force.Value})
}

// runWorker dispatches `fleet worker <COMMAND>`.
func runWorker(args []string, scope *cliargs.OptString) (exit.Code, error) {
	if len(args) == 0 {
		return 0, &usageError{"'fleet worker' requires a subcommand but one was not provided", workerUsage}
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, workerHelp)
		return exit.Ok, nil
	case "end":
		return runWorkerEnd(args[1:], scope)
	default:
		return 0, &usageError{fmt.Sprintf("unrecognized subcommand '%s'", args[0]), workerUsage}
	}
}

func runWorkerEnd(args []string, scope *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("worker end", scope)
	reportFile := cliargs.OptString{Name: "report-file", Placeholder: "PATH"}
	fs.Var(&reportFile, "report-file", "File with the report")
	got, helped, err := parse(fs, args, workerEndHelp, workerEndUsage, []string{"NAME"})
	if err != nil || helped {
		return exit.Ok, err
	}
	h, err := scoped(scope)
	if err != nil {
		return 0, err
	}
	return cmd.WorkerEnd(h, cmd.WorkerEndArgs{Name: got[0], ReportFile: reportFile.Ptr()})
}

func runWorktree(args []string, scope *cliargs.OptString) (exit.Code, error) {
	fs := flagSet("worktree", scope)
	name := cliargs.OptString{Name: "name", Placeholder: "NAME"}
	branch := cliargs.OptString{Name: "branch", Placeholder: "NAME"}
	detach := cliargs.OptString{Name: "detach", Placeholder: "REF"}
	fs.Var(&name, "name", "Another worktree of the job in this repo")
	fs.Var(&branch, "branch", "Branch to create")
	fs.Var(&detach, "detach", "Check out a commit detached")
	got, helped, err := parse(fs, args, worktreeHelp, worktreeUsage, []string{"REPO"})
	if err != nil || helped {
		return exit.Ok, err
	}
	if _, err := scoped(scope); err != nil {
		return 0, err
	}
	return cmd.Worktree(cmd.WorktreeArgs{Repo: got[0], Name: name.Ptr(), Branch: branch.Ptr(), Detach: detach.Ptr()})
}

// runPrompt dispatches `fleet prompt <COMMAND>`.
func runPrompt(args []string, scope *cliargs.OptString) (exit.Code, error) {
	if len(args) == 0 {
		return 0, &usageError{"'fleet prompt' requires a subcommand but one was not provided", promptUsage}
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, promptHelp)
		return exit.Ok, nil
	case "show":
		fs := flagSet("prompt show", scope)
		builtin := cliargs.Bool{Name: "builtin"}
		fs.Var(&builtin, "builtin", "Only fleet's built-in text of NAME")
		got, helped, err := parse(fs, args[1:], promptShowHelp, promptShowUsage, []string{"[NAME]"})
		if err != nil || helped {
			return exit.Ok, err
		}
		var name *string
		if len(got) == 1 {
			name = &got[0]
		}
		if _, err := scoped(scope); err != nil {
			return 0, err
		}
		return cmd.PromptShow(os.Stdout, os.Stderr, cmd.PromptShowArgs{Name: name, Builtin: builtin.Value})
	default:
		return 0, &usageError{fmt.Sprintf("unrecognized subcommand '%s'", args[0]), promptUsage}
	}
}

// `fleet spawn`: a lead starts a worker in its job and hands it its task.

package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Luolc/agent-fleet/internal/atb"
	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
	"github.com/Luolc/agent-fleet/internal/identity"
)

// SpawnAbout and SpawnLongAbout are the help texts of `spawn`.
const (
	SpawnAbout     = "Start a worker in your job (lead only) and hand it a task"
	SpawnLongAbout = "Start a worker in your job (lead only) and hand it a task.\n\n" +
		"The worker is named `<job>-<NAME>` and runs in --cwd, an existing directory: typically " +
		"a worktree from `fleet worktree` (`--detach <ref>` for a reviewer's checkout of a PR " +
		"head), which several workers may share. When --cwd is in a worktree for which the " +
		"ledger records that statute did not generate the private output (`fleet worktree` " +
		"exited 6), the worker's first message says so, why, and how to generate it. A job is started " +
		"by a thread agent with `fleet job start`; workers cannot spawn.\n\n" +
		"Settings come from .fleet/config.json in the main checkout of the job's repo: " +
		"`max_agents_per_job` (default 16), `resource_check` (default true) and `linear` " +
		"({\"team\": ..., \"project\": ...}; absent: Linear is off). A cross-repo job reads " +
		".fleet/config.json in its initiative checkout, except `linear`: Linear " +
		"is on exactly when the job has a parent issue, " +
		"whose team and project the work order goes to.\n\n" +
		"Checks, all before anything is created: the task file is readable and not empty; " +
		"--cwd is a directory; the job is open in the ledger; the config is valid; with Linear " +
		"on, the task gives a title; the name uses only [a-z0-9-], is " +
		"not `watch`, and is not taken in the ledger or in herdr; the job's workspace exists; " +
		"the job holds fewer than max_agents_per_job live agents including the lead, and its " +
		"workspace fewer than that many panes (`herdr pane list`; a pane a gone or ended agent left " +
		"counts until it is closed); with " +
		"resource_check, the 1-minute load average is below the CPU count times the scope's " +
		"`resources.load_multiplier` (default 1; skipped with --light) " +
		"and available memory is at least `resources.min_available_mib` (default 2048) plus " +
		"`resources.agent_mib` (default 512), the new agent's share, in MiB; " +
		"last, for a cross-repo job, the parent's team and project are " +
		"read from Linear.\n\n" +
		"Then the worker's row is reserved as `starting`, in one transaction with the name and " +
		"cap checks, so two spawns cannot both pass them. With Linear on, the work order is " +
		"created next, before anything else: `atb linear create` under the job's parent issue, " +
		"titled with the task's title (as `fleet job start --help` says) and described by the task file, written onto the row, then `atb linear " +
		"claim` in the worker's name. The worker gets FLEET_ISSUE and its task starts with a " +
		"line naming the work order's URL. A claim whose conflict check atb (0.4.0 or later) did " +
		"not finish, its exit 5, counts as claimed, with a note on stderr: the work order was just " +
		"created and no other agent can have claimed it. If an atb step fails, nothing else is " +
		"created and the row is ended, so the name is free and `job end` does not wait for it; " +
		"an issue created is listed, and the ended row keeps it. The last line says what is left: " +
		"no work order fleet heard of (a create that failed may still have made one, held by " +
		"nobody), one claimed by nobody (cancel it in Linear), or a claim atb did not confirm " +
		"(timed out, an exit other than 5), which `fleet worker end <name> --issue <ISSUE>` releases.\n\n" +
		"Then a tab is made in the job's workspace " +
		"with the FLEET_* variables set, and the agent --agent names is started; the pane is " +
		"renamed after the worker. Claude Code, the default, is started with `herdr agent start " +
		"--kind claude` and fixed arguments (permission prompts skipped, AskUserQuestion " +
		"disallowed, Remote Control off, plus --model and --effort when given or set in the " +
		"scope's `models.worker`), and its folder-trust prompt is accepted. Pi (`--agent pi`) is " +
		"started with `herdr agent start --kind pi` and `--model <MODEL>`, `--thinking <EFFORT>` " +
		"when --effort is given, and `--approve`, which trusts --cwd for this run, so no key is " +
		"pressed; the scope's `models.worker` is not used, and --model is required (without it " +
		"Pi would fall back to its own default model). Any screen that is not the input box stops " +
		"the spawn. Finally the task is delivered as `fleet send` would, and the row becomes " +
		"active.\n\n" +
		"--light is for a worker that only reads and does I/O (research, a review that runs no " +
		"build or test): the ledger marks it light, `fleet status` flags it, and its first message " +
		"tells it not to compile, run test suites or start anything CPU-heavy, and to stop and say " +
		"so when the work needs that. fleet does not enforce it.\n\n" +
		"There is no rollback and no retry. When a herdr step fails (the tab, the start, the " +
		"delivery), the " +
		"output lists what exists; the worker's row stays `starting`, and the cleanup is the " +
		"lead's call (`fleet job end <job> --force` from outside ends every row of the job).\n\n" +
		"Exit: 0 when the task was delivered; 1 when a check refuses (an --agent other than claude or " +
		"pi, or pi without --model, among them); 2/3/4 as `send` for the " +
		"delivery; 3 when the worker stops at a screen other than its input box (the screen is " +
		"printed); 5 when atb, herdr or the database fails, including a failed start. A failed " +
		"atb step is followed by atb's stderr with Linear credentials redacted."
)

// SpawnArgs are the arguments of `spawn`.
type SpawnArgs struct {
	// Name is the worker's name within the job. Only [a-z0-9-].
	Name string
	// TaskFile is the file with the task, delivered as the worker's first
	// message (with the header).
	TaskFile string
	// Cwd is the directory the worker runs in; it must exist.
	Cwd string
	// Agent is the kind of agent started: "claude" (also when empty) or
	// "pi".
	Agent string
	// Model is passed to the agent as --model. Default for Claude Code:
	// the scope's `models.worker.model`, else the agent's own; required for
	// Pi.
	Model *string
	// Effort is passed to the agent as --effort (Pi: --thinking). Default
	// for Claude Code: the scope's `models.worker.effort`, else the agent's
	// own; for Pi, Pi's own.
	Effort *string
	// Light skips the load check; the worker is told to only read.
	Light bool
}

// spawnChecked is what the checks before anything is created established.
type spawnChecked struct {
	me        *identity.Identity
	id        *identity.Identity
	job       *jobRow
	sc        *config.Scope
	cfg       *config.Config
	prompt    *promptText
	linear    *linear
	body      string
	task      string
	title     string
	cwd       string
	kind      string
	workspace string
	// panes is how many panes the job's workspace has; 0 until herdr is
	// read.
	panes int
	light bool
	// statuteNote tells the worker when the ledger records that statute
	// failed in the worktree that holds --cwd.
	statuteNote string
}

// spawnChecks runs every check in order: the caller and the names, the
// task file, --cwd, the ledger, the config and Linear, herdr's view, the
// resources. `conn` is open on success.
func spawnChecks(h *herdr.Herdr, args SpawnArgs) (*spawnChecked, *sql.DB, error) {
	me, err := identity.FromEnv()
	if err != nil {
		return nil, nil, err
	}
	if me.Role != identity.Lead {
		return nil, nil, exit.Refusedf("a %s cannot spawn; only a lead starts workers "+
			"(a thread agent starts a job with `fleet job start`)", me.Role)
	}
	if err := CheckName(args.Name); err != nil {
		return nil, nil, err
	}
	kind := args.Agent
	if kind == "" {
		kind = KindClaude
	}
	if err := CheckKind(kind); err != nil {
		return nil, nil, err
	}
	if kind == KindPi && args.Model == nil {
		return nil, nil, exit.Refusedf("--agent pi needs --model: without it Pi falls back to its own default model")
	}
	if me.Job == "" {
		return nil, nil, exit.Refusedf("FLEET_JOB is not set")
	}
	c := &spawnChecked{me: me, id: &identity.Identity{Agent: me.Job + "-" + args.Name, Role: identity.Worker,
		Parent: me.Agent, Scope: me.Scope, Job: me.Job}, light: args.Light, kind: kind}
	if err := CheckAgentName(c.id.Agent); err != nil {
		return nil, nil, err
	}
	if c.body, c.task, err = taskFile(args.TaskFile, me.Agent); err != nil {
		return nil, nil, err
	}
	if c.cwd, err = canonicalize(args.Cwd); err != nil {
		return nil, nil, exit.Refusedf("cannot use --cwd %s: %v", args.Cwd, err)
	}
	if info, err := os.Stat(c.cwd); err != nil || !info.IsDir() {
		return nil, nil, exit.Refusedf("--cwd %s is not a directory", args.Cwd)
	}
	conn, err := db.Open(me.Scope)
	if err != nil {
		return nil, nil, err
	}
	if err := c.ledgerAndHerdr(h, conn); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return c, conn, nil
}

// ledgerAndHerdr is the ledger checks, the config and the worker prompt,
// herdr's view and the resources.
func (c *spawnChecked) ledgerAndHerdr(h *herdr.Herdr, conn *sql.DB) error {
	var err error
	if c.job, err = openJob(conn, c.me.Job); err != nil {
		return err
	}
	if c.job == nil {
		return exit.Refusedf("job %s is not open in scope %s", c.me.Job, c.me.Scope)
	}
	if c.statuteNote, err = worktreeStatuteNote(conn, c.cwd); err != nil {
		return err
	}
	if c.sc, err = config.LoadScope(c.me.Scope); err != nil {
		return err
	}
	if c.prompt, err = loadPrompt(c.sc, "worker"); err != nil {
		return err
	}
	if c.cfg, err = jobConfig(c.job.Repo, c.job.LeadCwd); err != nil {
		return err
	}
	c.linear = jobLinear(c.cfg, c.job.Repo, c.job.ParentIssue)
	if c.linear != nil {
		if c.job.ParentIssue == "" {
			return exit.Refusedf("job %s has no parent issue in the ledger, so its worker cannot get a work order; "+
				"it was started before %s set linear", c.me.Job, config.Path(c.job.LeadCwd))
		}
		if c.title = WorkOrderTitle(c.body); c.title == "" {
			return exit.Refusedf("the task file gives no title for the work order: start it with `# <title>`")
		}
	}
	if err := c.dedup(conn); err != nil {
		return err
	}
	if err := herdrHasAgent(h, c.id.Agent); err != nil {
		return err
	}
	workspaces, err := WorkspacesLabelled(h, c.me.Job)
	if err != nil {
		return err
	}
	if len(workspaces) != 1 {
		return exit.Environmentf("expected one workspace labelled %s, herdr has %d", c.me.Job, len(workspaces))
	}
	c.workspace = workspaces[0]
	if c.panes, err = workspacePanes(h, c.workspace); err != nil {
		return err
	}
	if err := c.dedup(conn); err != nil {
		return err
	}
	if err := resources(c.cfg, c.sc, c.light); err != nil {
		return err
	}
	return c.linear.resolve()
}

// dedup is the checks on the worker's name and the job's cap: the name is
// free, and both the job's live rows and the panes of its workspace are
// under the cap, so panes that ended rows left behind count too. Run
// before herdr is asked (no panes read yet), after the panes are read, and
// again inside the reservation.
func (c *spawnChecked) dedup(conn querier) error {
	if err := liveNameTaken(conn, c.id.Agent); err != nil {
		return err
	}
	var live int64
	if err := conn.QueryRow("SELECT count(*) FROM agents WHERE job = ?1 AND state != 'ended'", c.me.Job).Scan(&live); err != nil {
		return exit.Database(err)
	}
	if limit := int64(c.cfg.MaxAgentsPerJob); max(live, int64(c.panes)) >= limit {
		return exit.Refusedf("job %s already has %d live agents and %d panes in its workspace; "+
			"the cap is %d including the lead, on the larger of the two", c.me.Job, live, c.panes, limit)
	}
	return nil
}

// linearFailed ends a spawn whose Linear steps failed. Nothing is in herdr
// yet, so the reserved row is ended: the name is free again and `job end`
// is not held up by a worker that never started. The ended row keeps the
// work order `issue` (empty when none was created), and the hint says
// what is left on it.
func (c *spawnChecked) linearFailed(conn *sql.DB, err error, issue string, created []string, hint string) (exit.Code, error) {
	if endErr := endRow(conn, c.id.Agent); endErr != nil {
		return startFailed(c.id.Agent, errors.Join(err, endErr), 0, created, hint)
	}
	created[0] = fmt.Sprintf("ledger row %s, now ended: nothing was started and the name is free", c.id.Agent)
	hint = "find out why from the error above first; " + c.orderLeft(err, issue)
	return startFailed(c.id.Agent, err, 0, created, hint)
}

// orderLeft says what a spawn stopped by `err` left of the worker's work
// order `issue`: none fleet heard of, though a create that failed may
// have made one; created and not claimed, held by nobody; or a claim atb
// did not confirm, which may be on it.
func (c *spawnChecked) orderLeft(err error, issue string) string {
	var failure *atb.Failure
	switch {
	case issue == "":
		return fmt.Sprintf("atb linear create reported no work order; if it made one under %s anyway, nobody "+
			"holds it: cancel it in Linear", c.job.ParentIssue)
	case !errors.As(err, &failure) || !strings.HasPrefix(failure.Op, "atb linear claim "):
		return fmt.Sprintf("the work order %s is claimed by nobody, cancel it in Linear", issue)
	}
	return fmt.Sprintf("atb did not confirm the claim of %[1]s on the work order %[2]s, so it may be there: "+
		"`fleet worker end %[1]s --issue %[2]s` releases it as abandoned, or exits 5 after atb's exit 4 when "+
		"%[1]s holds nothing there, and then nobody does: cancel %[2]s in Linear", c.id.Agent, issue)
}

// Spawn runs `spawn`.
func Spawn(h *herdr.Herdr, args SpawnArgs) (exit.Code, error) {
	c, conn, err := spawnChecks(h, args)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	hint := fmt.Sprintf("find out why from the error above first; the cleanup is the lead's call, and `fleet job end %s --force` from outside ends every row of the job", c.me.Job)
	// The worker's row is reserved in one transaction with the checks, so
	// two spawns of one name, or past the cap, cannot both pass.
	if err := reserve(conn, c.dedup, func(q querier) error {
		return insertStarting(q, c.id, c.cwd, c.task, c.job.ParentIssue, c.light)
	}); err != nil {
		return 0, err
	}
	created := []string{fmt.Sprintf("ledger row %s (state starting)", c.id.Agent)}
	issue, err := workOrder(conn, c.linear, c.job.ParentIssue, c.title, c.task, c.id, c.me.Agent,
		scopeOf(c.job.Repo, c.me.Job), &created)
	if err != nil {
		return c.linearFailed(conn, err, issue.Identifier, created, hint)
	}
	c.id.Issue = issue.Identifier
	c.id.Scratch = jobScratch(c.sc, c.job.Repo, c.job.LeadCwd)
	place, err := CreateTab(h, c.workspace, args.Name, c.cwd, c.id)
	if err != nil {
		return startFailed(c.id.Agent, err, 0, created, hint)
	}
	created = append(created, fmt.Sprintf("tab %s (%s)", args.Name, place.TabID))
	body := c.prompt.fill(roleSlots(c.sc, c.id, c.cwd, c.job.Repo, c.job.LeadCwd, c.cfg.MaxAgentsPerJob, issue,
		c.job.ParentIssue)) +
		noteSection(c.statuteNote, lightNote(c.light)) + taskSection(issue.URL, c.body)
	model, effort := args.Model, args.Effort
	if c.kind == KindClaude {
		model, effort = orDefault(model, c.sc.Models.Worker.Model), orDefault(effort, c.sc.Models.Worker.Effort)
	}
	code, err := startAndDeliver(h, conn, c.id, place, c.cwd, c.kind, model, effort, c.me.Agent, body, &created)
	if err != nil || code != exit.Ok {
		return startFailed(c.id.Agent, err, code, created, hint)
	}
	fmt.Fprintf(os.Stdout, "started %s in job %s (%s)\n", c.id.Agent, c.me.Job, c.cwd)
	return code, nil
}

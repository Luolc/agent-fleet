// `fleet spawn`: a lead starts a worker in its job and hands it its task.

package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"os"

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
		"head), which several workers may share. A job is started " +
		"by a thread agent with `fleet job start`; workers cannot spawn.\n\n" +
		"Settings come from .fleet/config.json in the main checkout of the job's repo: " +
		"`max_agents_per_job` (default 16), `resource_check` (default true) and `linear` " +
		"({\"team\": ..., \"project\": ...}; absent: Linear is off). A cross-repo job reads " +
		".fleet/config.json in its initiative checkout, except `linear`: Linear " +
		"is on exactly when the job has a parent issue, " +
		"whose team and project the work order goes to.\n\n" +
		"Checks, all before anything is created: the task file is readable and not empty; " +
		"--cwd is a directory; the job is open in the ledger; the config is valid; with Linear " +
		"on, the task's first non-empty line gives a title; the name uses only [a-z0-9-], is " +
		"not `watch`, and is not taken in the ledger or in herdr; the job's workspace exists; " +
		"the job holds fewer than max_agents_per_job live agents including the lead; with " +
		"resource_check, the 1-minute load average is below the CPU count (skipped with --light) " +
		"and available memory is above 2 GiB; last, for a cross-repo job, the parent's team and project are " +
		"read from Linear.\n\n" +
		"Then the worker's row is reserved as `starting`, in one transaction with the name and " +
		"cap checks, so two spawns cannot both pass them. With Linear on, the work order is " +
		"created next, before anything else: `atb linear create` under the job's parent issue, " +
		"titled with the task's first non-empty line without leading `#` (at most 80 " +
		"characters) and described by the task file, written onto the row, then `atb linear " +
		"claim` in the worker's name. The worker gets FLEET_ISSUE and its task starts with a " +
		"line naming the work order's URL. If an atb step fails, nothing else is created and the " +
		"row is ended, so the name is free and `job end` does not wait for it; an issue created " +
		"but not claimed is listed, and the ended row keeps it.\n\n" +
		"Then a tab is made in the job's workspace " +
		"with the FLEET_* variables set, and Claude Code (the only supported agent) is started " +
		"with `herdr agent start --kind claude` and fixed arguments (permission prompts " +
		"skipped, AskUserQuestion disallowed, Remote Control off, plus --model and --effort " +
		"when given or set in the scope's `models.worker`); the pane is renamed after the worker. " +
		"Its folder-trust prompt is accepted; any other screen that is not the input box stops the spawn. Finally the task " +
		"is delivered as `fleet send` would, and the row becomes active.\n\n" +
		"--light is for a worker that only reads and does I/O (research, a review that runs no " +
		"build or test): the ledger marks it light, `fleet status` flags it, and its first message " +
		"tells it not to compile, run test suites or start anything CPU-heavy, and to stop and say " +
		"so when the work needs that. fleet does not enforce it.\n\n" +
		"There is no rollback and no retry. When a herdr step fails (the tab, the start, the " +
		"delivery), the " +
		"output lists what exists; the worker's row stays `starting`, and the cleanup is the " +
		"lead's call (`fleet job end <job> --force` from outside ends every row of the job).\n\n" +
		"Exit: 0 when the task was delivered; 1 when a check refuses; 2/3/4 as `send` for the " +
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
	// Model is passed to the agent as --model. Default: the scope's
	// `models.worker.model`, else the agent's own.
	Model *string
	// Effort is passed to the agent as --effort. Default: the scope's
	// `models.worker.effort`, else the agent's own.
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
	workspace string
	light     bool
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
	if me.Job == "" {
		return nil, nil, exit.Refusedf("FLEET_JOB is not set")
	}
	c := &spawnChecked{me: me, id: &identity.Identity{Agent: me.Job + "-" + args.Name, Role: identity.Worker,
		Parent: me.Agent, Scope: me.Scope, Job: me.Job}, light: args.Light}
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
			return exit.Refusedf("the task file's first non-empty line gives no title for the work order")
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
	if err := resources(c.cfg, c.light); err != nil {
		return err
	}
	return c.linear.resolve()
}

// dedup is the ledger checks on the worker: its name is free and the job
// is under its cap. Run once before herdr is asked, and again inside the
// reservation.
func (c *spawnChecked) dedup(conn querier) error {
	if err := liveNameTaken(conn, c.id.Agent); err != nil {
		return err
	}
	var live int64
	if err := conn.QueryRow("SELECT count(*) FROM agents WHERE job = ?1 AND state != 'ended'", c.me.Job).Scan(&live); err != nil {
		return exit.Database(err)
	}
	if limit := int64(c.cfg.MaxAgentsPerJob); live >= limit {
		return exit.Refusedf("job %s already has %d live agents; the cap is %d including the lead",
			c.me.Job, live, limit)
	}
	return nil
}

// linearFailed ends a spawn whose Linear steps failed. Nothing is in herdr
// yet and the work order, if created, is not claimed, so the reserved row
// is ended: the name is free again and `job end` is not held up by a
// worker that never started. The ended row keeps the work order.
func (c *spawnChecked) linearFailed(conn *sql.DB, err error, created []string, hint string) (exit.Code, error) {
	if endErr := endRow(conn, c.id.Agent); endErr != nil {
		return startFailed(c.id.Agent, errors.Join(err, endErr), 0, created, hint)
	}
	created[0] = fmt.Sprintf("ledger row %s, now ended: nothing was started and the name is free", c.id.Agent)
	hint = "find out why from the error above first"
	if len(created) > 1 {
		hint += "; the work order is claimed by nobody, cancel it in Linear"
	}
	return startFailed(c.id.Agent, err, 0, created, hint)
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
		return c.linearFailed(conn, err, created, hint)
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
		noteSection(lightNote(c.light)) + taskSection(issue.URL, c.body)
	code, err := startAndDeliver(h, conn, c.id, place, c.cwd, orDefault(args.Model, c.sc.Models.Worker.Model),
		orDefault(args.Effort, c.sc.Models.Worker.Effort), c.me.Agent, body, &created)
	if err != nil || code != exit.Ok {
		return startFailed(c.id.Agent, err, code, created, hint)
	}
	fmt.Fprintf(os.Stdout, "started %s in job %s (%s)\n", c.id.Agent, c.me.Job, c.cwd)
	return code, nil
}

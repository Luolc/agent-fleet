// `fleet spawn`: a lead starts a worker in its job and hands it its task.

package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

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
		"a worktree from `fleet worktree`, which several workers may share. A job is started " +
		"by a thread agent with `fleet job start`; workers cannot spawn.\n\n" +
		"Settings come from .fleet/config.json in ~/dev/<repo> of the job's repo: " +
		"`max_agents_per_job` (default 4), `resource_check` (default true) and `linear` " +
		"({\"team\": ..., \"project\": ...}; absent: Linear is off). A cross-repo job reads no " +
		"config: the defaults apply, and Linear is on exactly when the job has a parent issue, " +
		"whose team and project the work order goes to.\n\n" +
		"Checks, all before anything is created: the task file is readable and not empty; " +
		"--cwd is a directory; the job is open in the ledger; the config is valid; with Linear " +
		"on, the task's first non-empty line gives a title; the name uses only [a-z0-9-], is " +
		"not `cron`, and is not taken in the ledger or in herdr; the job's workspace exists; " +
		"the job holds fewer than max_agents_per_job live agents including the lead; with " +
		"resource_check, the 1-minute load average is below the CPU count and available " +
		"memory is above 2 GiB; last, for a cross-repo job, the parent's team and project are " +
		"read from Linear.\n\n" +
		"With Linear on, the work order is created next, before anything else: `atb linear " +
		"create` under the job's parent issue, titled with the task's first non-empty line " +
		"without leading `#` (at most 80 characters) and described by the task file, then " +
		"`atb linear claim` in the worker's name. The worker gets FLEET_ISSUE and its task " +
		"starts with a line naming the work order's URL. If an atb step fails, nothing else is " +
		"created; an issue created but not claimed is listed.\n\n" +
		"Then the worker's row is written as `starting`, a tab is made in the job's workspace " +
		"with the FLEET_* variables set, and Claude Code (the only supported agent) is started " +
		"with `herdr agent start --kind claude` and fixed arguments (permission prompts " +
		"skipped, AskUserQuestion disallowed, Remote Control off, plus --model and --effort " +
		"when given); the pane is renamed after the worker. Its folder-trust prompt is " +
		"accepted; any other screen that is not the input box stops the spawn. Finally the task " +
		"is delivered as `fleet send` would, and the row becomes active.\n\n" +
		"There is no rollback and no retry. When a step fails after something was created, the " +
		"output lists what exists; the worker's row stays `starting`, and the cleanup is the " +
		"lead's call (`fleet close <job> --force` ends every row of the job).\n\n" +
		"Exit: 0 when the task was delivered; 1 when a check refuses; 2/3/4 as `send` for the " +
		"delivery; 3 when the worker stops at a screen other than its input box (the screen is " +
		"printed); 5 when atb, herdr or the database fails, including a failed start."
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
	// Model is passed to the agent as --model. Default: the agent's own.
	Model *string
	// Effort is passed to the agent as --effort. Default: the agent's own.
	Effort *string
}

// spawnChecked is what the checks before anything is created established.
type spawnChecked struct {
	me        *identity.Identity
	id        *identity.Identity
	job       *jobRow
	cfg       *config.Config
	linear    *linear
	body      string
	task      string
	title     string
	cwd       string
	workspace string
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
		Parent: me.Agent, Target: me.Target, Job: me.Job}}
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
	conn, err := db.Open(me.Target)
	if err != nil {
		return nil, nil, err
	}
	if err := c.ledgerAndHerdr(h, conn); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return c, conn, nil
}

// ledgerAndHerdr is the ledger checks, the config, herdr's view and the
// resources.
func (c *spawnChecked) ledgerAndHerdr(h *herdr.Herdr, conn *sql.DB) error {
	var err error
	if c.job, err = openJob(conn, c.me.Job); err != nil {
		return err
	}
	if c.job == nil {
		return exit.Refusedf("job %s is not open in target %s", c.me.Job, c.me.Target)
	}
	home, err := Home()
	if err != nil {
		return err
	}
	if c.cfg, err = jobConfig(home, c.job.Repo); err != nil {
		return err
	}
	c.linear = jobLinear(c.cfg, c.job.Repo, c.job.ParentIssue)
	if c.linear != nil {
		if c.job.ParentIssue == "" {
			return exit.Refusedf("job %s has no parent issue in the ledger, so its worker cannot get a work order; "+
				"it was started before %s set linear", c.me.Job, config.Path(filepath.Join(home, "dev", c.job.Repo)))
		}
		if c.title = WorkOrderTitle(c.body); c.title == "" {
			return exit.Refusedf("the task file's first non-empty line gives no title for the work order")
		}
	}
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
	if err := resources(c.cfg); err != nil {
		return err
	}
	return c.linear.resolve()
}

// Spawn runs `spawn`.
func Spawn(h *herdr.Herdr, args SpawnArgs) (exit.Code, error) {
	c, conn, err := spawnChecks(h, args)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	var created []string
	hint := fmt.Sprintf("the cleanup is the lead's call; `fleet close %s --force` ends every row of the job", c.me.Job)
	issue, err := workOrder(c.linear, c.job.ParentIssue, c.title, c.task, c.id.Agent, c.me.Agent,
		scopeOf(c.job.Repo, c.me.Job), &created)
	if err != nil {
		return startFailed(c.id.Agent, err, 0, created, "")
	}
	c.id.Issue = issue.Identifier
	if err := insertStarting(conn, c.id, c.cwd, c.task, c.job.ParentIssue); err != nil {
		return startFailed(c.id.Agent, err, 0, created, "")
	}
	created = append(created, fmt.Sprintf("ledger row %s (state starting)", c.id.Agent))
	place, err := CreateTab(h, c.workspace, args.Name, c.cwd, c.id)
	if err != nil {
		return startFailed(c.id.Agent, err, 0, created, hint)
	}
	created = append(created, fmt.Sprintf("tab %s (%s)", args.Name, place.TabID))
	code, err := startAndDeliver(h, conn, c.id, place, args.Model, args.Effort, c.body, issue.URL, &created)
	if err != nil || code != exit.Ok {
		return startFailed(c.id.Agent, err, code, created, hint)
	}
	fmt.Fprintf(os.Stdout, "started %s in job %s (%s)\n", c.id.Agent, c.me.Job, c.cwd)
	return code, nil
}

// `fleet job start` and `fleet job list`: a thread agent starts a job (its
// lead) and lists the open jobs.

package cmd

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Luolc/agent-fleet/internal/atb"
	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
	"github.com/Luolc/agent-fleet/internal/identity"
)

// JobAbout, JobStartAbout, JobStartLongAbout, JobListAbout and
// JobListLongAbout are the help texts of `job` and its subcommands.
const (
	JobAbout          = "Start a job, or list the open jobs"
	JobStartAbout     = "Start a job: its lead, in a workspace named after the job (thread agents only)"
	JobStartLongAbout = "Start a job: its lead, in a workspace named after the job (thread agents only).\n\n" +
		"<JOB> is the job id; the lead is named `<job>-lead`. With --repo the job is a " +
		"single-repo job and the lead runs in ~/dev/<repo>, which must exist (read-only by " +
		"convention: the lead plans and spawns, it does not edit there). Without --repo it is " +
		"a cross-repo job and the lead runs in ~/cross-repo/<job>/, which is created. No " +
		"worktree is made; agents open theirs with `fleet worktree`.\n\n" +
		"Settings: a single-repo job reads .fleet/config.json in ~/dev/<repo> (`max_agents_per_job`, " +
		"`resource_check`, `linear`; see `fleet spawn --help`). A cross-repo job reads no " +
		"config: the defaults apply, and Linear is on exactly when --parent-issue is given; " +
		"the work orders then go to the parent's team and project, read from Linear.\n\n" +
		"Linear on: exactly one of --parent-issue <ISSUE> and --new-parent <TITLE> is required; " +
		"--new-parent (single-repo jobs only) creates the parent in the repo's team and project " +
		"first, described by the task file. Linear off: both are refused and the job is a " +
		"ledger row with no work orders.\n\n" +
		"Checks, all before anything is created: the task file is readable and not empty; " +
		"the names are valid (`<job>-lead` has at most 32 characters); the config is valid; " +
		"with Linear on, the task's first non-empty line gives a title; no open job in this " +
		"target has the same name or, with --key, the same key (the job is named); no live " +
		"lead has the same parent issue (the lead is named: talk to it instead); the lead's " +
		"name is free in the ledger and in herdr; no workspace is labelled after the job and, " +
		"for a cross-repo job, ~/cross-repo/<job>/ does not exist; with resource_check, the " +
		"1-minute load average is below the CPU count and available memory is above 2 GiB; " +
		"last, for a cross-repo job, the parent's team and project are read from Linear.\n\n" +
		"Then the job is reserved: the job's row (open) and the lead's row (`starting`) are " +
		"written in one transaction with the dedup checks, so two starts on the same name, " +
		"key or parent cannot both pass. With Linear on, next and before anything else: the " +
		"parent is created (--new-parent) and written onto both rows at once; the parent is " +
		"claimed in the lead's name (`atb linear claim`, so Linear holds the same lock as the " +
		"ledger); the lead's work order is created under the parent, titled with the task's " +
		"first non-empty line without leading `#` (at most 80 characters) and described by " +
		"the task file, written onto the lead's row, and claimed in the lead's name. If an atb " +
		"step fails, nothing else is created; what was created is listed, with the cleanup " +
		"command, and the rows keep the identifiers written so far.\n\n" +
		"Then the cross-repo directory is made, the workspace is created with the FLEET_* " +
		"variables set (FLEET_TARGET, FLEET_JOB, FLEET_ISSUE), Claude Code is started as `fleet " +
		"spawn` starts a worker, the task is delivered with the work order's URL, and the " +
		"lead's row becomes active.\n\n" +
		"There is no rollback and no retry. When a step fails after the reservation, the " +
		"output lists what exists and the cleanup command `fleet job end <job> --force`.\n\n" +
		"Exit: 0 when the task was delivered; 1 when a check refuses; 2/3/4 as `send` for the " +
		"delivery; 3 when the lead stops at a screen other than its input box (the screen is " +
		"printed); 5 when atb, herdr or the database fails, including a failed start."
	JobListAbout     = "List the open jobs of a target (read-only)"
	JobListLongAbout = "List the open jobs of a target (read-only).\n\n" +
		"One line per open job, oldest first, tab-separated: job, parent issue, key, repo, " +
		"lead, workers. An empty field is `-`; repo is `-` for a cross-repo job; lead is " +
		"`<job>-lead` while its row is live, else `-`; workers are the live workers' names " +
		"joined with commas. Keys never contain tabs or newlines. Nothing is printed when no " +
		"job is open. --json prints a JSON array instead, one object per job with job, " +
		"parent_issue, key, repo, lead_cwd, lead (null when none is live), workers and " +
		"started_at.\n\n" +
		"Exit: 0; 1 when the target is not a directory name; 5 when the database fails or " +
		"the ledger does not exist."
)

// JobStartArgs are the arguments of `job start`.
type JobStartArgs struct {
	// Job is the job id. Only [a-z0-9-].
	Job string
	// TaskFile is the file with the lead's task.
	TaskFile string
	// ParentIssue is the job's parent issue; NewParent is the title of a
	// parent to create. At most one is set.
	ParentIssue *string
	NewParent   *string
	// Repo, when set, is the directory name of the job's repo under ~/dev.
	Repo *string
	// Key, when set, is the job's dedup key.
	Key *string
	// Model and Effort are passed to the agent. Default: the agent's own.
	Model  *string
	Effort *string
}

// JobListArgs are the arguments of `job list`.
type JobListArgs struct {
	// Target, when set, replaces FLEET_TARGET.
	Target *string
	// JSON asks for machine-readable output.
	JSON bool
}

// jobRow is an open job as the ledger records it.
type jobRow struct {
	Job         string
	ParentIssue string
	Key         string
	Repo        string
	LeadCwd     string
	HomeThread  string
	StartedAt   int64
}

// openJob is the open job named `job`, or nil.
func openJob(conn *sql.DB, job string) (*jobRow, error) {
	var r jobRow
	err := conn.QueryRow("SELECT job, parent_issue, key, repo, lead_cwd, home_thread, started_at FROM jobs "+
		"WHERE job = ?1 AND state = 'open'", job).Scan(
		&r.Job, &r.ParentIssue, &r.Key, &r.Repo, &r.LeadCwd, &r.HomeThread, &r.StartedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, exit.Database(err)
	}
	return &r, nil
}

// openJobs are the open jobs, oldest first.
func openJobs(conn *sql.DB) ([]jobRow, error) {
	rows, err := conn.Query("SELECT job, parent_issue, key, repo, lead_cwd, home_thread, started_at FROM jobs " +
		"WHERE state = 'open' ORDER BY id")
	if err != nil {
		return nil, exit.Database(err)
	}
	defer rows.Close()
	var jobs []jobRow
	for rows.Next() {
		var r jobRow
		if err := rows.Scan(&r.Job, &r.ParentIssue, &r.Key, &r.Repo, &r.LeadCwd, &r.HomeThread, &r.StartedAt); err != nil {
			return nil, exit.Database(err)
		}
		jobs = append(jobs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, exit.Database(err)
	}
	return jobs, nil
}

// jobChecked is what `job start`'s checks established.
type jobChecked struct {
	me     *identity.Identity
	id     *identity.Identity
	home   string
	repo   string
	cwd    string
	key    string
	cfg    *config.Config
	linear *linear
	body   string
	task   string
	title  string
	parent string
}

// jobStartChecks runs every check in order: the caller and the names, the
// task file, the flags, the repo and the config, the ledger, herdr's view,
// the resources. `conn` is open on success.
func jobStartChecks(h *herdr.Herdr, args JobStartArgs) (*jobChecked, *sql.DB, error) {
	me, err := identity.FromEnv()
	if err != nil {
		return nil, nil, err
	}
	if me.Role != identity.Thread {
		return nil, nil, exit.Refusedf("a %s cannot start a job; only a thread agent does", me.Role)
	}
	if err := CheckName(args.Job); err != nil {
		return nil, nil, err
	}
	c := &jobChecked{me: me, id: &identity.Identity{Agent: args.Job + "-lead", Role: identity.Lead,
		Parent: me.Agent, Target: me.Target, Job: args.Job}}
	if err := CheckAgentName(c.id.Agent); err != nil {
		return nil, nil, err
	}
	if c.body, c.task, err = taskFile(args.TaskFile, me.Agent); err != nil {
		return nil, nil, err
	}
	if err := c.flags(args); err != nil {
		return nil, nil, err
	}
	if err := c.repoAndLinear(args); err != nil {
		return nil, nil, err
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

// flags checks what needs no file or ledger: the parent flags, --key and
// --repo as text.
func (c *jobChecked) flags(args JobStartArgs) error {
	if args.ParentIssue != nil && args.NewParent != nil {
		return exit.Refusedf("--parent-issue and --new-parent exclude each other")
	}
	if args.ParentIssue != nil {
		if err := atb.CheckIdentifier(*args.ParentIssue); err != nil {
			return err
		}
		c.parent = *args.ParentIssue
	}
	if args.NewParent != nil && strings.TrimSpace(*args.NewParent) == "" {
		return exit.Refusedf("--new-parent needs a title")
	}
	if args.Key != nil {
		if *args.Key == "" || strings.ContainsAny(*args.Key, "\t\n\r") {
			return exit.Refusedf("--key must not be empty or contain tabs or newlines")
		}
		c.key = *args.Key
	}
	if args.Repo != nil {
		if err := CheckRepo(*args.Repo); err != nil {
			return err
		}
		c.repo = *args.Repo
	}
	return nil
}

// repoAndLinear finds where the lead runs, loads the config and decides
// the job's Linear settings against the parent flags.
func (c *jobChecked) repoAndLinear(args JobStartArgs) error {
	var err error
	if c.home, err = Home(); err != nil {
		return err
	}
	if c.repo != "" {
		c.cwd = filepath.Join(c.home, "dev", c.repo)
		if info, err := os.Stat(c.cwd); err != nil || !info.IsDir() {
			return exit.Refusedf("no checkout at %s", c.cwd)
		}
	} else {
		c.cwd = filepath.Join(c.home, "cross-repo", args.Job)
		if args.NewParent != nil {
			return exit.Refusedf("--new-parent is for single-repo jobs; a cross-repo job has no project " +
				"to create the parent in, so create it first and pass --parent-issue")
		}
	}
	if c.cfg, err = jobConfig(c.home, c.repo); err != nil {
		return err
	}
	c.linear = jobLinear(c.cfg, c.repo, c.parent)
	switch {
	case c.linear != nil && args.ParentIssue == nil && args.NewParent == nil:
		return exit.Refusedf("--parent-issue or --new-parent is required: %s sets linear, so the lead "+
			"gets a work order under the job's parent issue", config.Path(c.cwd))
	case c.linear == nil && (args.ParentIssue != nil || args.NewParent != nil):
		return exit.Refusedf("--parent-issue and --new-parent are for repos that use Linear; %s sets no linear",
			config.Path(c.cwd))
	case c.linear != nil:
		if c.title = WorkOrderTitle(c.body); c.title == "" {
			return exit.Refusedf("the task file's first non-empty line gives no title for the work order")
		}
	}
	return nil
}

// dedup is the dedup checks in the ledger: the job's name, its key and its
// parent issue, then the lead's name. Run once before the Linear query,
// so a refusal needs no Linear, and again inside the reservation.
func (c *jobChecked) dedup(conn querier) error {
	job := c.id.Job
	var one int
	err := conn.QueryRow("SELECT 1 FROM jobs WHERE job = ?1 AND state = 'open'", job).Scan(&one)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return exit.Database(err)
	}
	if err == nil {
		return exit.Refusedf("job %s is already open in target %s; if it is left over, clean up with "+
			"`fleet job end %s --force`", job, c.me.Target, job)
	}
	if c.key != "" {
		var other string
		err := conn.QueryRow("SELECT job FROM jobs WHERE key = ?1 AND state = 'open'", c.key).Scan(&other)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return exit.Database(err)
		}
		if err == nil {
			return exit.Refusedf("job %s is open with the same key %q", other, c.key)
		}
	}
	if c.parent != "" {
		var lead string
		err := conn.QueryRow("SELECT name FROM agents WHERE role = 'lead' AND parent_issue = ?1 AND state != 'ended' "+
			"ORDER BY id LIMIT 1", c.parent).Scan(&lead)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return exit.Database(err)
		}
		if err == nil {
			return exit.Refusedf("%s is live on parent issue %s; talk to it with `fleet send %s` instead of "+
				"starting another job", lead, c.parent, lead)
		}
	}
	return liveNameTaken(conn, c.id.Agent)
}

// ledgerAndHerdr is the dedup checks in the ledger, then herdr's view, then
// the resources, then the parent's team and project.
func (c *jobChecked) ledgerAndHerdr(h *herdr.Herdr, conn *sql.DB) error {
	job := c.id.Job
	if err := c.dedup(conn); err != nil {
		return err
	}
	if err := herdrHasAgent(h, c.id.Agent); err != nil {
		return err
	}
	workspaces, err := WorkspacesLabelled(h, job)
	if err != nil {
		return err
	}
	if len(workspaces) != 0 || c.repo == "" && exists(c.cwd) {
		return exit.Refusedf("job %s already has a workspace or the directory %s; if it is left over, "+
			"clean up with `fleet job end %s --force`", job, c.cwd, job)
	}
	if err := resources(c.cfg); err != nil {
		return err
	}
	return c.linear.resolve()
}

// reserve writes the job row (open) and the lead's row (`starting`) in one
// immediate transaction with the dedup checks, so two starts on the same
// parent, name or key cannot both pass.
func (c *jobChecked) reserve(conn *sql.DB) error {
	return reserve(conn, c.dedup, func(q querier) error {
		if _, err := q.Exec(
			"INSERT INTO jobs (job, parent_issue, key, repo, lead_cwd, state, started_at) VALUES (?1, ?2, ?3, ?4, ?5, 'open', ?6)",
			c.id.Job, c.parent, c.key, c.repo, c.cwd, db.Now()); err != nil {
			return exit.Database(err)
		}
		return insertStarting(q, c.id, c.cwd, c.task, c.parent)
	})
}

// bindParent writes the parent issue onto the reserved job row and lead
// row. The lead row's partial unique index refuses it when another live
// lead already holds the parent.
func (c *jobChecked) bindParent(conn querier) error {
	if _, err := conn.Exec("UPDATE jobs SET parent_issue = ?1 WHERE job = ?2 AND state = 'open'", c.parent, c.id.Job); err != nil {
		return exit.Database(err)
	}
	if _, err := conn.Exec("UPDATE agents SET parent_issue = ?1 WHERE name = ?2 AND state != 'ended'",
		c.parent, c.id.Agent); err != nil {
		return exit.Database(err)
	}
	return nil
}

// linearSteps creates the parent when asked and binds it to the reserved
// rows at once, claims it for the lead, and makes the lead's work order.
// Returns the work order (zero without Linear).
func (c *jobChecked) linearSteps(conn querier, args JobStartArgs, created *[]string) (atb.Issue, error) {
	if c.linear == nil {
		return atb.Issue{}, nil
	}
	scope := scopeOf(c.repo, c.id.Job)
	if args.NewParent != nil {
		parent, err := atb.Create(c.linear.team, c.linear.project, "", *args.NewParent, c.task)
		if err != nil {
			return atb.Issue{}, err
		}
		c.parent = parent.Identifier
		*created = append(*created, fmt.Sprintf("parent issue %s (%s)", parent.Identifier, parent.URL))
		if err := c.bindParent(conn); err != nil {
			return atb.Issue{}, err
		}
	}
	if err := atb.Claim(c.parent, c.id.Agent, c.me.Agent, scope); err != nil {
		return atb.Issue{}, err
	}
	*created = append(*created, fmt.Sprintf("parent issue %s claimed by %s", c.parent, c.id.Agent))
	return workOrder(conn, c.linear, c.parent, c.title, c.task, c.id.Agent, c.me.Agent, scope, created)
}

// JobStart runs `job start`.
func JobStart(h *herdr.Herdr, args JobStartArgs) (exit.Code, error) {
	c, conn, err := jobStartChecks(h, args)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	job := c.id.Job
	hint := fmt.Sprintf("clean up with: fleet job end %s --force", job)
	if err := c.reserve(conn); err != nil {
		return 0, err
	}
	created := []string{fmt.Sprintf("job %s (open)", job), fmt.Sprintf("ledger row %s (state starting)", c.id.Agent)}
	issue, err := c.linearSteps(conn, args, &created)
	if err != nil {
		return startFailed(c.id.Agent, err, 0, created, hint)
	}
	c.id.Issue = issue.Identifier
	if c.repo == "" {
		if err := os.MkdirAll(c.cwd, 0o777); err != nil {
			return startFailed(c.id.Agent, exit.IO(err), 0, created, hint)
		}
		created = append(created, "directory "+c.cwd)
	}
	place, err := CreateWorkspace(h, job, "lead", c.cwd, c.id)
	if place.WorkspaceID != "" {
		created = append(created, fmt.Sprintf("workspace %s (%s)", job, place.WorkspaceID))
	}
	if err != nil {
		return startFailed(c.id.Agent, err, 0, created, hint)
	}
	code, err := startAndDeliver(h, conn, c.id, place, args.Model, args.Effort, c.body, issue.URL, &created)
	if err != nil || code != exit.Ok {
		return startFailed(c.id.Agent, err, code, created, hint)
	}
	fmt.Fprintf(os.Stdout, "started %s in job %s (%s)\n", c.id.Agent, job, c.cwd)
	return code, nil
}

// jobLine is one job as `job list` prints it.
type jobLine struct {
	Job         string   `json:"job"`
	ParentIssue string   `json:"parent_issue"`
	Key         string   `json:"key"`
	Repo        string   `json:"repo"`
	LeadCwd     string   `json:"lead_cwd"`
	Lead        *string  `json:"lead"`
	Workers     []string `json:"workers"`
	StartedAt   int64    `json:"started_at"`
}

// jobLines joins the open jobs with their live lead and workers.
func jobLines(conn *sql.DB) ([]jobLine, error) {
	jobs, err := openJobs(conn)
	if err != nil {
		return nil, err
	}
	lines := make([]jobLine, 0, len(jobs))
	for _, j := range jobs {
		l := jobLine{Job: j.Job, ParentIssue: j.ParentIssue, Key: j.Key, Repo: j.Repo, LeadCwd: j.LeadCwd,
			Workers: []string{}, StartedAt: j.StartedAt}
		rows, err := conn.Query("SELECT name, role FROM agents WHERE job = ?1 AND state != 'ended' ORDER BY id", j.Job)
		if err != nil {
			return nil, exit.Database(err)
		}
		for rows.Next() {
			var name, role string
			if err := rows.Scan(&name, &role); err != nil {
				rows.Close()
				return nil, exit.Database(err)
			}
			if role == "lead" && l.Lead == nil {
				l.Lead = &name
			} else if role == "worker" {
				l.Workers = append(l.Workers, name)
			}
		}
		if err := rows.Close(); err != nil {
			return nil, exit.Database(err)
		}
		if err := rows.Err(); err != nil {
			return nil, exit.Database(err)
		}
		lines = append(lines, l)
	}
	return lines, nil
}

// JobList runs `job list`.
func JobList(args JobListArgs) (exit.Code, error) {
	conn, err := OpenLedger(args.Target)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	lines, err := jobLines(conn)
	if err != nil {
		return 0, err
	}
	if args.JSON {
		var buf bytes.Buffer
		encoder := json.NewEncoder(&buf)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(lines); err != nil {
			return 0, exit.Environmentf("json: %v", err)
		}
		fmt.Fprint(os.Stdout, buf.String())
		return exit.Ok, nil
	}
	for _, l := range lines {
		lead := "-"
		if l.Lead != nil {
			lead = *l.Lead
		}
		workers := "-"
		if len(l.Workers) > 0 {
			workers = strings.Join(l.Workers, ",")
		}
		fmt.Fprintln(os.Stdout, strings.Join([]string{l.Job, orDash(l.ParentIssue), orDash(l.Key), orDash(l.Repo), lead, workers}, "\t"))
	}
	return exit.Ok, nil
}

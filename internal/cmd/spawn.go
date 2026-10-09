// `fleet spawn`: start a lead or a worker and hand it its task.
//
// The building blocks (make a workspace or a tab with an identity, start
// the agent in it and get it to its input box) are exported so
// `human-interface` can be started the same way.

package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
	"github.com/Luolc/agent-fleet/internal/identity"
)

// SpawnAbout and SpawnLongAbout are the help texts of `spawn`.
const (
	SpawnAbout     = "Start a lead (from the orchestra) or a worker (from a lead) and hand it a task"
	SpawnLongAbout = "Start a lead (from the orchestra) or a worker (from a lead) and hand it a task.\n\n" +
		"Your role decides what is started. The orchestra starts a lead: <NAME> is the job id, the " +
		"agent is named `<job>-lead`, and --branch is valid only here. A lead starts a worker in " +
		"its own job, named `<job>-<NAME>`. Workers and human-interface cannot spawn.\n\n" +
		"Checks, all before anything is created: the task file is readable and not empty; the " +
		"name uses only [a-z0-9-], is not `cron`, and is not taken in the ledger or in herdr; a " +
		"new job has no workspace or worktree yet; a job holds at most 4 live agents including " +
		"the lead; the 1-minute load average is below the CPU count and available memory is above " +
		"2 GiB.\n\n" +
		"For a lead, ~/dev/<repo> then runs `git fetch origin`, and origin/HEAD must resolve; " +
		"the job branches from it, never from the local HEAD.\n\n" +
		"Then the agent's row is written as `starting`. A lead gets the worktree " +
		"~/wt/<repo>/<job> on a new branch from origin/HEAD and a workspace named after the " +
		"job, whose first tab is the lead's; a worker gets a new tab in that workspace. Both run " +
		"in the job's worktree with the FLEET_* variables set. The agent is Claude Code, currently the only supported " +
		"agent, started through `herdr agent start --kind claude` with fixed arguments (permission prompts skipped, " +
		"AskUserQuestion disallowed, Remote Control off, plus --model and --effort when given) " +
		"and the pane is renamed after the agent. If Claude Code shows its folder-trust prompt, " +
		"the folder is trusted; any other screen that is not the agent's input box stops the spawn. " +
		"Finally the task file is delivered as `fleet send` would, and the row becomes live.\n\n" +
		"There is no rollback and no retry. When a step fails after something was created, the " +
		"output lists what exists. For a lead it adds the cleanup command `fleet close <job> " +
		"--force`; a lead whose worker failed reports the failure to the orchestra, which decides " +
		"on the cleanup (the worker's row stays `starting` meanwhile). A " +
		"gateway that refuses the session (machine over its session cap) is a failed start.\n\n" +
		"Exit: 0 when the task was delivered; 1 when a check refuses; 2/3/4 as `send` for the " +
		"delivery; 3 when the agent stops at a screen other than its input box (the screen is " +
		"printed); 5 when git, herdr or the database fails, including a failed start."
)

// SpawnArgs are the arguments of `spawn`.
type SpawnArgs struct {
	// Name is the job id (from the orchestra) or worker name (from a lead).
	// Only [a-z0-9-].
	Name string
	// TaskFile is the file with the task, delivered as the agent's first
	// message (with the header).
	TaskFile string
	// Branch is the branch for the job's worktree (lead only). Default:
	// data/<job>.
	Branch *string
	// Model is passed to the agent as --model. Default: the agent's own.
	Model *string
	// Effort is passed to the agent as --effort. Default: the agent's own.
	Effort *string
}

// jobCap is the agents per job workspace, counting the lead.
const jobCap int64 = 4
const minAvailableKiB uint64 = 2 * 1024 * 1024

// startTimeoutMS is how long `herdr agent start` may take, in ms.
const startTimeoutMS = "30000"

// Place is where an agent runs: the ids herdr gave the new workspace or tab.
type Place struct {
	WorkspaceID string
	TabID       string
	PaneID      string
}

// ClaudeArgs are the arguments after `--` in `herdr agent start --kind claude`.
func ClaudeArgs(model, effort *string) []string {
	args := []string{
		"--dangerously-skip-permissions",
		"--disallowedTools",
		"AskUserQuestion",
		"--settings",
		`{"remoteControlAtStartup":false}`,
	}
	if model != nil {
		args = append(args, "--model", *model)
	}
	if effort != nil {
		args = append(args, "--effort", *effort)
	}
	return args
}

func envFlags(id *identity.Identity) []string {
	var flags []string
	for _, pair := range id.EnvPairs() {
		flags = append(flags, "--env", pair[0]+"="+pair[1])
	}
	return flags
}

func placeFrom(result []byte) (Place, error) {
	root, _ := herdr.Lookup(result, "root_pane").(map[string]any)
	field := func(name string) (string, error) {
		value, ok := root[name].(string)
		if !ok {
			return "", exit.Environmentf("herdr reply has no root_pane.%s", name)
		}
		return value, nil
	}
	var place Place
	var err error
	if place.WorkspaceID, err = field("workspace_id"); err != nil {
		return Place{}, err
	}
	if place.TabID, err = field("tab_id"); err != nil {
		return Place{}, err
	}
	if place.PaneID, err = field("pane_id"); err != nil {
		return Place{}, err
	}
	return place, nil
}

// CreateWorkspace makes a new workspace labelled `label`, its first tab
// labelled `tabLabel`, running in `cwd` with `id`'s FLEET_* variables.
// Not focused.
func CreateWorkspace(h *herdr.Herdr, label, tabLabel, cwd string, id *identity.Identity) (Place, error) {
	args := []string{"workspace", "create", "--label", label, "--no-focus", "--cwd", cwd}
	args = append(args, envFlags(id)...)
	result, err := h.CallOK(args...)
	if err != nil {
		return Place{}, err
	}
	place, err := placeFrom(result)
	if err != nil {
		return Place{}, err
	}
	if _, err := h.CallOK("tab", "rename", place.TabID, tabLabel); err != nil {
		return Place{}, err
	}
	return place, nil
}

// CreateTab makes a new tab labelled `label` in `workspaceID`, running in
// `cwd` with `id`'s FLEET_* variables. Not focused.
func CreateTab(h *herdr.Herdr, workspaceID, label, cwd string, id *identity.Identity) (Place, error) {
	args := []string{"tab", "create", "--workspace", workspaceID, "--label", label, "--no-focus", "--cwd", cwd}
	args = append(args, envFlags(id)...)
	result, err := h.CallOK(args...)
	if err != nil {
		return Place{}, err
	}
	return placeFrom(result)
}

// StartAgent starts the agent (Claude Code) as `name` in `paneID`, renames
// the pane, and gets the agent to its input box: the folder-trust prompt is
// accepted, any other screen is a failure with exit 3 that carries the
// screen. A start that herdr reports as failed (the gateway refusing the
// session ends up here, as a timeout) is exit 5 with the pane's text; it
// is not retried.
func StartAgent(h *herdr.Herdr, name, paneID string, model, effort *string) error {
	args := []string{"agent", "start", name, "--kind", "claude", "--pane", paneID,
		"--timeout", startTimeoutMS, "--"}
	args = append(args, ClaudeArgs(model, effort)...)
	reply, err := h.Call(args...)
	if err != nil {
		return err
	}
	switch {
	case reply.Error == nil:
	// The agent stopped at a prompt during startup; the screen decides below.
	case reply.Error.Code == "agent_not_ready":
	default:
		pane := paneText(h, paneID)
		return exit.Environmentf("agent %s did not start: herdr: %s: %s\npane %s:\n%s",
			name, reply.Error.Code, reply.Error.Message, paneID, pane)
	}
	if _, err := h.CallOK("pane", "rename", paneID, name); err != nil {
		return err
	}

	err = reachInputBox(
		func() (string, error) { return h.Screen(name) },
		func() (string, error) {
			agent, err := h.CallOK("agent", "get", name)
			if err != nil {
				return "", err
			}
			inner, _ := herdr.Lookup(agent, "agent").(map[string]any)
			status, _ := inner["agent_status"].(string)
			return status, nil
		},
		func(keys []string) error {
			send := append([]string{"agent", "send-keys", name}, keys...)
			_, err := h.CallOK(send...)
			return err
		},
		10*time.Second,
		250*time.Millisecond,
	)
	var failure *exit.Failure
	if errors.As(err, &failure) && failure.Code == exit.Blocked {
		return exit.New(exit.Blocked, fmt.Sprintf("%s %s", name, failure.Message))
	}
	return err
}

// reachInputBox gets a started agent to its input box. Ready takes both
// the input box on `screen` (herdr reports a ❯ menu as idle too) and
// herdr's `status` idle or done (done: idle after a turn); herdr can still
// say blocked after the redraw, and a prompt is then refused as
// agent_blocked. The folder-trust prompt is answered through `keys`, up to
// 3 times in case a key press is lost, polling every `poll` for at most
// `wait` after each. Anything else is exit 3 with the screen.
func reachInputBox(
	screen func() (string, error),
	status func() (string, error),
	keys func([]string) error,
	wait time.Duration,
	poll time.Duration,
) error {
	settled := func(status string) bool { return status == "idle" || status == "done" }
	text, err := screen()
	if err != nil {
		return err
	}
	for range 3 {
		trust := TrustKeys(text)
		if trust == nil {
			break
		}
		if err := keys(trust); err != nil {
			return err
		}
		if text, err = pollInputBox(screen, status, wait, poll); err != nil {
			return err
		}
	}
	state, err := status()
	if err != nil {
		return err
	}
	if IsInputBox(text) && settled(state) {
		return nil
	}
	return exit.New(exit.Blocked,
		fmt.Sprintf("is not ready at its input box (herdr status %q); its screen:\n%s", state, text))
}

// pollInputBox reads the screen every `poll` until it shows the input box
// with herdr settled, or `wait` has passed; the last screen is returned.
func pollInputBox(screen, status func() (string, error), wait, poll time.Duration) (string, error) {
	deadline := time.Now().Add(wait)
	for {
		time.Sleep(poll)
		text, err := screen()
		if err != nil {
			return "", err
		}
		if IsInputBox(text) {
			state, err := status()
			if err != nil {
				return "", err
			}
			if state == "idle" || state == "done" {
				return text, nil
			}
		}
		if !time.Now().Before(deadline) {
			return text, nil
		}
	}
}

// paneText is the pane's recent text (plain text, like `agent read`), for
// a pane whose agent did not start and so cannot be read by agent name.
func paneText(h *herdr.Herdr, paneID string) string {
	out, _, err := h.Run("pane", "read", paneID, "--source", "recent")
	if err != nil {
		return "(could not read the pane)"
	}
	return string(out)
}

// TrustKeys are the keys that trust the folder when `screen` is Claude
// Code's folder-trust prompt (2.1.292: "Quick safety check", the cancel
// option listed first and highlighted with ❯). nil for any other screen,
// or when the highlight cannot be placed.
func TrustKeys(screen string) []string {
	if !strings.Contains(screen, "Quick safety check") {
		return nil
	}
	highlighted := ""
	found := false
	for _, line := range lines(screen) {
		line = strings.TrimLeftFunc(line, unicode.IsSpace)
		if strings.HasPrefix(line, "❯") {
			highlighted, found = line, true
			break
		}
	}
	if !found {
		return nil
	}
	switch {
	case strings.Contains(highlighted, "Yes, I trust this folder"):
		return []string{"enter"}
	case strings.Contains(highlighted, "No, ") && strings.Contains(screen, "Yes, I trust this folder"):
		return []string{"down", "enter"}
	default:
		return nil
	}
}

// IsInputBox is whether `screen` shows Claude Code's input box: a rule
// line, then the line with the ❯ prompt, then a rule line. herdr alone is
// not enough here: it also reports a ❯ menu (for example the theme picker)
// as idle.
func IsInputBox(screen string) bool {
	rule := func(line string) bool {
		line = strings.TrimSpace(line)
		return utf8.RuneCountInString(line) >= 10 && strings.Trim(line, "─") == ""
	}
	all := lines(screen)
	for i := 0; i+3 <= len(all); i++ {
		w := all[i : i+3]
		if rule(w[0]) && strings.HasPrefix(strings.TrimLeftFunc(w[1], unicode.IsSpace), "❯") && rule(w[2]) {
			return true
		}
	}
	return false
}

// lines splits as Rust's `lines()`: at "\n" or "\r\n", with no empty line
// after a final newline.
func lines(text string) []string {
	if text == "" {
		return nil
	}
	parts := strings.Split(text, "\n")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	for i, part := range parts {
		parts[i] = strings.TrimSuffix(part, "\r")
	}
	return parts
}

// CheckResources refuses unless the 1-minute load average is below `cpus`
// and MemAvailable is above 2 GiB. Inputs are the texts of /proc/loadavg
// and /proc/meminfo.
func CheckResources(loadavg string, cpus int, meminfo string) error {
	fields := strings.Fields(loadavg)
	if len(fields) == 0 {
		return exit.Environmentf("cannot parse /proc/loadavg")
	}
	load, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return exit.Environmentf("cannot parse /proc/loadavg")
	}
	// The first `MemAvailable:` line decides, as the source's find_map: a
	// first line that does not parse is the error, not a reason to read on.
	var availableKiB uint64
	found := false
	for _, line := range lines(meminfo) {
		rest, ok := strings.CutPrefix(line, "MemAvailable:")
		if !ok {
			continue
		}
		if values := strings.Fields(rest); len(values) > 0 {
			if value, err := strconv.ParseUint(values[0], 10, 64); err == nil {
				availableKiB, found = value, true
			}
		}
		break
	}
	if !found {
		return exit.Environmentf("cannot find MemAvailable in /proc/meminfo")
	}
	if load >= float64(cpus) {
		return exit.Refusedf("the 1-minute load average is %s, not below the %d CPUs; try again later",
			strconv.FormatFloat(load, 'f', -1, 64), cpus)
	}
	if availableKiB <= minAvailableKiB {
		return exit.Refusedf("available memory is %d MiB, not above 2048 MiB; try again later",
			availableKiB/1024)
	}
	return nil
}

// CheckName refuses a name that is empty, has characters outside
// [a-z0-9-], or is `cron`.
func CheckName(name string) error {
	ok := name != ""
	for _, c := range name {
		ok = ok && (c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-')
	}
	if !ok {
		return exit.Refusedf("name %q must use only [a-z0-9-]", name)
	}
	if name == "cron" {
		return exit.Refusedf("the name `cron` is reserved")
	}
	return nil
}

// CheckAgentName is CheckName for a whole agent name, plus herdr's own
// rule (0.9.3): it starts with a letter and has at most 32 characters.
func CheckAgentName(name string) error {
	if err := CheckName(name); err != nil {
		return err
	}
	if name == "" || name[0] < 'a' || name[0] > 'z' || len(name) > 32 {
		return exit.Refusedf(
			"agent name %q must start with a letter and have at most 32 characters (herdr's rule)", name)
	}
	return nil
}

// Home is the caller's home directory, from HOME.
func Home() (string, error) {
	home, ok := os.LookupEnv("HOME")
	if !ok {
		return "", exit.Environmentf("HOME is not set")
	}
	return home, nil
}

// RepoName is the name part of `owner/name`; refused when empty.
func RepoName(repo string) (string, error) {
	name := repo
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		name = repo[i+1:]
	}
	if name == "" {
		return "", exit.Refusedf("FLEET_REPO is not set: this pane was not started by fleet")
	}
	return name, nil
}

// Git runs git and returns its stdout; a failure is exit 5 with git's
// stderr.
func Git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return "", exit.Environmentf("cannot run git: %v", err)
	}
	if err != nil {
		return "", exit.Environmentf("git %s failed: %s", strings.Join(args, " "),
			strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// originHead fetches origin in `checkout` and returns the commit
// origin/HEAD points to. Exit 5 when the fetch fails or origin/HEAD is not
// set.
func originHead(checkout string) (string, error) {
	if _, err := Git("-C", checkout, "fetch", "--quiet", "origin"); err != nil {
		return "", err
	}
	sha, err := Git("-C", checkout, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/HEAD^{commit}")
	if err != nil {
		return "", exit.Environmentf(
			"origin/HEAD is not set in %s; a job branches from it, not from the local HEAD "+
				"(`git -C %s remote set-head origin --auto` sets it)", checkout, checkout)
	}
	return strings.TrimSpace(sha), nil
}

// WorkspacesLabelled is `herdr workspace list`, filtered to the workspaces
// labelled `label`.
func WorkspacesLabelled(h *herdr.Herdr, label string) ([]string, error) {
	list, err := h.CallOK("workspace", "list")
	if err != nil {
		return nil, err
	}
	workspaces, _ := herdr.Lookup(list, "workspaces").([]any)
	ids := []string{}
	for _, entry := range workspaces {
		w, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if got, ok := w["label"].(string); !ok || got != label {
			continue
		}
		if id, ok := w["workspace_id"].(string); ok {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// HerdrAgentList is `herdr agent list`: every agent herdr knows in this
// session, as herdr printed them.
func HerdrAgentList(h *herdr.Herdr) ([]any, error) {
	list, err := h.CallOK("agent", "list")
	if err != nil {
		return nil, err
	}
	agents, _ := herdr.Lookup(list, "agents").([]any)
	return agents, nil
}

// plan is what is to be started, decided from the caller's role.
type plan struct {
	agent    string
	role     identity.Role
	job      string
	tabLabel string
	// branch is set for a lead only: the branch of the new worktree.
	branch *string
}

func planSpawn(me *identity.Identity, args SpawnArgs) (plan, error) {
	if err := CheckName(args.Name); err != nil {
		return plan{}, err
	}
	switch me.Role {
	case identity.Orchestra:
		agent := args.Name + "-lead"
		if err := CheckAgentName(agent); err != nil {
			return plan{}, err
		}
		branch := "data/" + args.Name
		if args.Branch != nil {
			branch = *args.Branch
		}
		return plan{agent: agent, role: identity.Lead, job: args.Name, tabLabel: "lead", branch: &branch}, nil
	case identity.Lead:
		if args.Branch != nil {
			return plan{}, exit.Refusedf("--branch is for leads only; a worker uses its job's worktree")
		}
		if me.Job == "" {
			return plan{}, exit.Refusedf("FLEET_JOB is not set")
		}
		agent := me.Job + "-" + args.Name
		if err := CheckAgentName(agent); err != nil {
			return plan{}, err
		}
		return plan{agent: agent, role: identity.Worker, job: me.Job, tabLabel: args.Name}, nil
	default:
		return plan{}, exit.Refusedf("a %s cannot spawn; only the orchestra (leads) and leads (workers) can",
			me.Role)
	}
}

func liveNameTaken(conn *sql.DB, name string) (bool, error) {
	var one int
	err := conn.QueryRow("SELECT 1 FROM agents WHERE name = ?1 AND state != 'ended'", name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, exit.Database(err)
	}
	return true, nil
}

// checked is what the checks before anything is created established:
// the caller, the plan, the task (with its header, and its canonical
// path), the places, and the commit a lead branches from.
type checked struct {
	me         *identity.Identity
	p          plan
	text       string
	task       string
	worktree   string
	checkout   string
	base       string
	workspaces []string
}

// spawnChecks runs every check, in the source's order: the plan, the task
// file, the ledger, herdr's view, the resources, then origin/HEAD for a
// lead. `conn` is open on success.
func spawnChecks(h *herdr.Herdr, args SpawnArgs) (*checked, *sql.DB, error) {
	me, err := identity.FromEnv()
	if err != nil {
		return nil, nil, err
	}
	p, err := planSpawn(me, args)
	if err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(args.TaskFile)
	if err != nil {
		return nil, nil, exit.Refusedf("cannot read %s: %v", args.TaskFile, err)
	}
	text, err := WithHeader(me.Agent, string(data))
	if err != nil {
		return nil, nil, err
	}
	task, err := canonicalize(args.TaskFile)
	if err != nil {
		return nil, nil, exit.IO(err)
	}
	repo, err := RepoName(me.Repo)
	if err != nil {
		return nil, nil, err
	}
	home, err := Home()
	if err != nil {
		return nil, nil, err
	}
	conn, err := db.Open(me.Repo)
	if err != nil {
		return nil, nil, err
	}
	c := &checked{me: me, p: p, text: text, task: task, checkout: filepath.Join(home, "dev", repo)}
	if err := c.ledgerAndHerdr(h, conn, filepath.Join(home, "wt", repo, p.job)); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return c, conn, nil
}

// ledgerAndHerdr is the ledger checks, then herdr's view, then the
// resources and origin/HEAD; `leadWorktree` is the worktree a lead gets.
func (c *checked) ledgerAndHerdr(h *herdr.Herdr, conn *sql.DB, leadWorktree string) error {
	me, p := c.me, c.p
	taken, err := liveNameTaken(conn, p.agent)
	if err != nil {
		return err
	}
	if taken {
		return exit.Refusedf("%s is already live in the ledger; names are never reused while live", p.agent)
	}
	// Ledger checks first, then herdr's view.
	if p.role == identity.Lead {
		c.worktree = leadWorktree
	} else if c.worktree, err = workerWorktree(conn, me.Agent, p.job); err != nil {
		return err
	}
	agents, err := HerdrAgentList(h)
	if err != nil {
		return err
	}
	for _, entry := range agents {
		a, _ := entry.(map[string]any)
		if name, _ := a["name"].(string); name == p.agent {
			return exit.Refusedf("herdr already has an agent named %s", p.agent)
		}
	}
	if c.workspaces, err = WorkspacesLabelled(h, p.job); err != nil {
		return err
	}
	switch {
	case p.role == identity.Lead && (len(c.workspaces) != 0 || exists(c.worktree)):
		return exit.Refusedf("job %s already has a workspace or the worktree %s; "+
			"if it is left over, clean up with `fleet close %s --force`", p.job, c.worktree, p.job)
	case p.role == identity.Worker && len(c.workspaces) != 1:
		return exit.Environmentf("expected one workspace labelled %s, herdr has %d", p.job, len(c.workspaces))
	}
	loadavg, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return exit.IO(err)
	}
	meminfo, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return exit.IO(err)
	}
	// TODO(port): available_parallelism also honors a cgroup CPU quota;
	// NumCPU honors only the affinity mask.
	if err := CheckResources(string(loadavg), runtime.NumCPU(), string(meminfo)); err != nil {
		return err
	}
	// Only a lead makes a branch; a worker uses its job's worktree.
	if p.role == identity.Lead {
		if c.base, err = originHead(c.checkout); err != nil {
			return err
		}
	}
	return nil
}

// workerWorktree is the job's worktree from the lead's live row, once the
// job is under its cap.
func workerWorktree(conn *sql.DB, lead, job string) (string, error) {
	var found sql.NullString
	err := conn.QueryRow("SELECT worktree FROM agents WHERE name = ?1 AND state != 'ended'", lead).Scan(&found)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", exit.Database(err)
	}
	if !found.Valid || found.String == "" {
		return "", exit.Refusedf("%s has no live row with a worktree", lead)
	}
	var live int64
	if err := conn.QueryRow("SELECT count(*) FROM agents WHERE job = ?1 AND state != 'ended'", job).Scan(&live); err != nil {
		return "", exit.Database(err)
	}
	if live >= jobCap {
		return "", exit.Refusedf("job %s already has %d live agents; the cap is %d including the lead",
			job, live, jobCap)
	}
	return found.String, nil
}

// spawnCreate is everything after the row is written: the worktree and
// workspace (lead) or tab (worker), the agent, the delivery and the
// `active` row. Every step appends what it made to `created`.
func spawnCreate(h *herdr.Herdr, conn *sql.DB, c *checked, args SpawnArgs, created *[]string) (exit.Code, error) {
	p := c.p
	id := &identity.Identity{Agent: p.agent, Role: p.role, Parent: c.me.Agent, Repo: c.me.Repo, Job: p.job}
	var place Place
	var err error
	if p.branch != nil {
		if _, err := Git("-C", c.checkout, "worktree", "add", c.worktree, "--no-track", "-b", *p.branch, c.base); err != nil {
			return 0, err
		}
		*created = append(*created, fmt.Sprintf("worktree %s on branch %s", c.worktree, *p.branch))
		// BUG(port): CreateWorkspace renames the first tab after the
		// workspace exists; when the rename fails (reproduce: `herdr tab
		// rename` refused) the workspace is not in `created`, so the
		// report below omits it although `fleet close <job> --force` must
		// remove it.
		if place, err = CreateWorkspace(h, p.job, p.tabLabel, c.worktree, id); err != nil {
			return 0, err
		}
		*created = append(*created, fmt.Sprintf("workspace %s (%s)", p.job, place.WorkspaceID))
	} else {
		if place, err = CreateTab(h, c.workspaces[0], p.tabLabel, c.worktree, id); err != nil {
			return 0, err
		}
		*created = append(*created, fmt.Sprintf("tab %s (%s)", p.tabLabel, place.TabID))
	}
	if _, err := conn.Exec("UPDATE agents SET pane_id = ?1 WHERE name = ?2 AND state != 'ended'",
		place.PaneID, p.agent); err != nil {
		return 0, exit.Database(err)
	}
	if err := StartAgent(h, p.agent, place.PaneID, args.Model, args.Effort); err != nil {
		return 0, err
	}
	*created = append(*created, fmt.Sprintf("agent %s in pane %s", p.agent, place.PaneID))
	code, err := Deliver(h, p.agent, c.text)
	if err != nil {
		return 0, err
	}
	// BUG(port): an unclear delivery (exit 2: herdr timed out or reported
	// agent_prompt_stalled; reproduce with a fake agent that never enters
	// working) still makes the row `active`, although the task may not
	// have arrived; only agent_not_found leaves it `starting`.
	if code != exit.NotFound {
		if _, err := conn.Exec("UPDATE agents SET state = 'active' WHERE name = ?1 AND state = 'starting'",
			p.agent); err != nil {
			return 0, exit.Database(err)
		}
	}
	return code, nil
}

// Spawn runs `spawn`.
func Spawn(h *herdr.Herdr, args SpawnArgs) (exit.Code, error) {
	c, conn, err := spawnChecks(h, args)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	p := c.p
	if _, err := conn.Exec(
		"INSERT INTO agents (name, role, job, worktree, parent, report_to, task, state, started_at) "+
			"VALUES (?1, ?2, ?3, ?4, ?5, ?5, ?6, 'starting', ?7)",
		p.agent, p.role.String(), p.job, c.worktree, c.me.Agent, c.task, db.Now()); err != nil {
		return 0, exit.Database(err)
	}
	created := []string{fmt.Sprintf("ledger row %s (state starting)", p.agent)}
	code, err := spawnCreate(h, conn, c, args, &created)

	if err != nil || code != exit.Ok {
		if err != nil {
			fmt.Fprintf(os.Stderr, "fleet: %s\n", err)
		}
		fmt.Fprintf(os.Stderr, "spawn of %s did not complete; created so far:\n", p.agent)
		for _, item := range created {
			fmt.Fprintf(os.Stderr, "  - %s\n", item)
		}
		if p.role == identity.Lead {
			fmt.Fprintf(os.Stderr, "clean up with: fleet close %s --force\n", p.job)
		} else {
			fmt.Fprintln(os.Stderr, "report this failure to the orchestra (`fleet send orchestra`); "+
				"the cleanup is the orchestra's call")
		}
		var failure *exit.Failure
		if errors.As(err, &failure) {
			return failure.Code, nil
		}
		if err != nil {
			return exit.Environment, nil
		}
		return code, nil
	}
	fmt.Fprintf(os.Stdout, "started %s in job %s (%s)\n", p.agent, p.job, c.worktree)
	return code, nil
}

// canonicalize is `fs::canonicalize`: absolute, symlinks resolved.
func canonicalize(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return filepath.Abs(resolved)
}

// exists is `Path::exists`: false on any error.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

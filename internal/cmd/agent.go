// The building blocks `job start` and `spawn` share: names, the task
// file, the repo's config, the work order, places in herdr, starting Claude
// Code and getting it to its input box, and the machine's resources.

package cmd

import (
	"context"
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

	"github.com/Luolc/agent-fleet/internal/atb"
	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
	"github.com/Luolc/agent-fleet/internal/identity"
)

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
// Not focused. The workspace exists once this returns a Place, even when
// the tab rename failed.
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
		return place, err
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

// lines splits at "\n" or "\r\n", with no empty line after a final
// newline.
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
	// The first `MemAvailable:` line decides: a first line that does not
	// parse is the error, not a reason to read on.
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

// machineResources is CheckResources on this machine; a variable so a
// test can stand in for the machine. `runtime.NumCPU` honors the affinity
// mask, not a cgroup CPU quota: in a container with a quota the load may
// be compared with more CPUs than the quota allows.
var machineResources = func() error {
	loadavg, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return exit.IO(err)
	}
	meminfo, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return exit.IO(err)
	}
	return CheckResources(string(loadavg), runtime.NumCPU(), string(meminfo))
}

// resources is machineResources, unless the config turns the check off.
func resources(cfg *config.Config) error {
	if !cfg.ResourceCheck {
		return nil
	}
	return machineResources()
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

// CheckRepo refuses what is not a directory name under ~/dev.
func CheckRepo(repo string) error {
	if repo == "" || repo == "." || repo == ".." || strings.Contains(repo, "/") {
		return exit.Refusedf("<repo> %q must be a directory name under ~/dev", repo)
	}
	return nil
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
			"origin/HEAD is not set in %s; a branch starts from it, not from the local HEAD "+
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

// herdrHasAgent refuses when herdr already has an agent named `name`.
func herdrHasAgent(h *herdr.Herdr, name string) error {
	agents, err := HerdrAgentList(h)
	if err != nil {
		return err
	}
	for _, entry := range agents {
		a, _ := entry.(map[string]any)
		if got, _ := a["name"].(string); got == name {
			return exit.Refusedf("herdr already has an agent named %s", name)
		}
	}
	return nil
}

func liveNameTaken(conn querier, name string) error {
	var one int
	err := conn.QueryRow("SELECT 1 FROM agents WHERE name = ?1 AND state != 'ended'", name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return exit.Database(err)
	}
	return exit.Refusedf("%s is already live in the ledger; names are never reused while live", name)
}

// taskFile reads the task: refused when unreadable or empty (as the
// header check would refuse it). Returns the body and the canonical path.
func taskFile(path, sender string) (body, canonical string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", exit.Refusedf("cannot read %s: %v", path, err)
	}
	if _, err := WithHeader(sender, string(data)); err != nil {
		return "", "", err
	}
	canonical, err = canonicalize(path)
	if err != nil {
		return "", "", exit.IO(err)
	}
	return string(data), canonical, nil
}

// WorkOrderTitle is the title of a work order for `task`: its first
// non-empty line without leading `#` and spaces, cut to 80 characters.
func WorkOrderTitle(task string) string {
	for _, line := range lines(task) {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		if line == "" {
			continue
		}
		if runes := []rune(line); len(runes) > 80 {
			line = strings.TrimSpace(string(runes[:80]))
		}
		return line
	}
	return ""
}

// linear is where a job's work orders go; nil when the job does not use
// Linear. For a cross-repo job the team and project are empty until
// `resolve` reads them from the parent issue.
type linear struct{ team, project, parent string }

// jobLinear decides whether a job uses Linear. A single-repo job takes its
// team and project from `.fleet/config.json` of ~/dev/<repo>; a cross-repo
// job uses Linear exactly when it has a parent issue, whose team and
// project `resolve` reads from Linear once every other check has passed.
func jobLinear(cfg *config.Config, repo, parentIssue string) *linear {
	if repo != "" {
		if cfg.Linear == nil {
			return nil
		}
		return &linear{team: cfg.Linear.Team, project: cfg.Linear.Project}
	}
	if parentIssue == "" {
		return nil
	}
	return &linear{parent: parentIssue}
}

// resolve reads the team and project from the parent issue when they are
// not known yet. The last check of a start: it is the only one that needs
// Linear, so every refusal comes before it.
func (l *linear) resolve() error {
	if l == nil || l.team != "" {
		return nil
	}
	team, project, err := atb.TeamProject(l.parent)
	if err != nil {
		return err
	}
	l.team, l.project = team, project
	return nil
}

// jobConfig is the config a job runs under: `.fleet/config.json` of
// ~/dev/<repo>, or the defaults for a cross-repo job.
func jobConfig(home, repo string) (*config.Config, error) {
	if repo == "" {
		return config.Default(), nil
	}
	return config.Load(filepath.Join(home, "dev", repo))
}

// scopeOf is the claim scope of a job's work orders: `<repo>: job <job>`,
// or `cross-repo: job <job>`.
func scopeOf(repo, job string) string {
	if repo == "" {
		repo = "cross-repo"
	}
	return repo + ": job " + job
}

// workOrder creates `agent`'s work order under `parent` in the agent's
// name and claims it, when the job uses Linear; otherwise it returns the
// zero Issue. An issue that was created is in `created`, claimed or not.
func workOrder(l *linear, parent, title, task, agent, source, scope string, created *[]string) (atb.Issue, error) {
	if l == nil {
		return atb.Issue{}, nil
	}
	issue, err := atb.Create(l.team, l.project, parent, title, task)
	if err != nil {
		return atb.Issue{}, err
	}
	*created = append(*created, fmt.Sprintf("work order %s (%s), not claimed", issue.Identifier, issue.URL))
	if err := atb.Claim(issue.Identifier, agent, source, scope); err != nil {
		return atb.Issue{}, err
	}
	(*created)[len(*created)-1] = fmt.Sprintf("work order %s (%s), claimed by %s",
		issue.Identifier, issue.URL, agent)
	return issue, nil
}

// querier is what the checks and the reservation run on: the ledger, or
// the immediate transaction that reserves the new rows.
type querier interface {
	QueryRow(query string, args ...any) *sql.Row
	Exec(query string, args ...any) (sql.Result, error)
}

// reserve runs `checks` and then `inserts` in one immediate transaction,
// so two starts cannot both pass the checks: the second waits for the
// first to commit and then sees its rows. The unique indexes of the
// ledger are the guarantee behind it.
func reserve(conn *sql.DB, checks func(q querier) error, inserts func(q querier) error) error {
	tx, err := conn.BeginTx(context.Background(), nil)
	if err != nil {
		return exit.Database(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := checks(tx); err != nil {
		return err
	}
	if err := inserts(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return exit.Database(err)
	}
	return nil
}

// insertStarting writes the new agent's row as `starting`.
func insertStarting(conn querier, id *identity.Identity, cwd, task, parentIssue string) error {
	if _, err := conn.Exec(
		"INSERT INTO agents (name, role, job, cwd, parent, report_to, task, state, started_at, issue, parent_issue) "+
			"VALUES (?1, ?2, ?3, ?4, ?5, ?5, ?6, 'starting', ?7, ?8, ?9)",
		id.Agent, id.Role.String(), id.Job, cwd, id.Parent, task, db.Now(), id.Issue, parentIssue); err != nil {
		return exit.Database(err)
	}
	return nil
}

// setIssue records the agent's work order on its reserved row.
func setIssue(conn querier, agent, issue, parentIssue string) error {
	if _, err := conn.Exec("UPDATE agents SET issue = ?1, parent_issue = ?2 WHERE name = ?3 AND state != 'ended'",
		issue, parentIssue, agent); err != nil {
		return exit.Database(err)
	}
	return nil
}

// startAndDeliver starts the agent in `place`, delivers the task with the
// header and the work order's URL, and marks the row active. Every step
// appends what it made to `created`. The row is marked active only when
// herdr reported the task delivered, so a row still `starting` means the
// agent may not have its task.
func startAndDeliver(h *herdr.Herdr, conn *sql.DB, id *identity.Identity, place Place, model, effort *string,
	body, url string, created *[]string) (exit.Code, error) {
	if _, err := conn.Exec("UPDATE agents SET pane_id = ?1 WHERE name = ?2 AND state != 'ended'",
		place.PaneID, id.Agent); err != nil {
		return 0, exit.Database(err)
	}
	if err := StartAgent(h, id.Agent, place.PaneID, model, effort); err != nil {
		return 0, err
	}
	*created = append(*created, fmt.Sprintf("agent %s in pane %s", id.Agent, place.PaneID))
	if url != "" {
		body = "Work order: " + url + "\n\n" + body
	}
	text, err := WithHeader(id.Parent, body)
	if err != nil {
		return 0, err
	}
	code, err := Deliver(h, id.Agent, text)
	if err != nil || code != exit.Ok {
		return code, err
	}
	if _, err := conn.Exec("UPDATE agents SET state = 'active' WHERE name = ?1 AND state = 'starting'",
		id.Agent); err != nil {
		return 0, exit.Database(err)
	}
	return exit.Ok, nil
}

// startFailed prints why a start stopped and what it created, then the
// cleanup hint, and returns the exit code.
func startFailed(agent string, err error, code exit.Code, created []string, hint string) (exit.Code, error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet: %s\n", err)
	}
	if len(created) > 0 {
		fmt.Fprintf(os.Stderr, "start of %s did not complete; created so far:\n", agent)
		for _, item := range created {
			fmt.Fprintf(os.Stderr, "  - %s\n", item)
		}
		if hint != "" {
			fmt.Fprintln(os.Stderr, hint)
		}
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

// canonicalize is the absolute path with symlinks resolved.
func canonicalize(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return filepath.Abs(resolved)
}

// exists is whether a path exists: false on any error.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

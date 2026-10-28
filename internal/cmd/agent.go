// The building blocks `job start` and `spawn` share: names, the task
// file, the repo's config, the work order, places in herdr, starting the
// agent (Claude Code, or Pi for a worker) and getting it to its input box,
// the built-in prompts of the lead and the worker, and the machine's
// resources.

package cmd

import (
	"context"
	"database/sql"
	_ "embed" // the built-in prompts
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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

// PiArgs are the arguments after `--` in `herdr agent start --kind pi`:
// --model and --thinking (the effort) when given, and --approve, which
// trusts the directory fleet chose for the agent for this run, so Pi asks
// no project-trust question.
func PiArgs(model, effort *string) []string {
	var args []string
	if model != nil {
		args = append(args, "--model", *model)
	}
	if effort != nil {
		args = append(args, "--thinking", *effort)
	}
	return append(args, "--approve")
}

// The agent runtimes fleet starts, by herdr's `--kind`.
const (
	KindClaude = "claude"
	KindPi     = "pi"
)

// agentKind is what fleet knows of one agent runtime: how it is started
// and how its screen reads.
type agentKind struct {
	// args are the arguments after `--` in `herdr agent start`.
	args func(model, effort *string) []string
	// inputBox is whether a screen shows the agent ready for a message.
	inputBox func(screen string) bool
	// trustKey is TrustKey for the runtime's folder-trust dialog; nil when
	// the runtime is started with its directory trusted, so fleet presses
	// no key at all.
	trustKey func(screen, cwd string) (string, error)
	// filter is the screen without what moves while the agent is stuck,
	// for watch's screen hash.
	filter func(screen string) string
	// readFirst is set when herdr reports no blocked state for the
	// runtime: a delivery reads the screen first and stops unless it shows
	// the input box.
	readFirst bool
}

var kinds = map[string]agentKind{
	KindClaude: {args: ClaudeArgs, inputBox: IsInputBox, trustKey: TrustKey, filter: FilterClaudeScreen},
	KindPi:     {args: PiArgs, inputBox: IsPiInputBox, filter: FilterPiScreen, readFirst: true},
}

// kindOf is the entry for herdr's kind `name`: Claude Code's for a kind
// fleet does not start, or none given, as before fleet told kinds apart.
func kindOf(name string) agentKind {
	if k, ok := kinds[name]; ok {
		return k
	}
	return kinds[KindClaude]
}

// CheckKind refuses (exit 1) a kind fleet does not start.
func CheckKind(name string) error {
	if _, ok := kinds[name]; !ok {
		return exit.Refusedf("--agent %s: fleet starts only %s and %s", name, KindClaude, KindPi)
	}
	return nil
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

// startAttempts bounds the starts of one agent, and startPause is the wait
// before the second. Each failed start costs herdr's whole start timeout,
// so two attempts keep a start run from a Bash tool within its 2 minutes.
const (
	startAttempts = 2
	startPause    = 3 * time.Second
)

// StartAgent starts the agent of kind `kind` as `name` in `paneID`,
// renames the pane, and gets the agent to its input box: Claude Code's
// folder-trust dialog for `cwd`, the directory fleet chose for it, is
// accepted (Pi is started with that directory trusted); any other screen
// is a failure with exit 3 that carries the screen. A start that
// herdr reports as failed (an agent that exits at once, as when a gateway
// refuses the session or an update has removed claude for a moment, ends up
// here, as a timeout) is exit 5 with the pane's text. A start that failed
// with no agent left in the pane is made once more in the same pane; an
// exit 3 screen is not.
func StartAgent(h *herdr.Herdr, name, paneID, cwd, kind string, model, effort *string) error {
	start := func() error { return startOnce(h, name, paneID, cwd, kind, model, effort) }
	return startAgain(h, name, paneID, start, start)
}

// ResumeAgent is SettleAgent for an agent an earlier run started in
// `paneID`, with StartAgent's second start (Claude Code with no model or
// effort, as a thread agent has) when it exits before its input box.
func ResumeAgent(h *herdr.Herdr, name, paneID, cwd string) error {
	return startAgain(h, name, paneID,
		func() error { return SettleAgent(h, name, paneID, cwd) },
		func() error { return startOnce(h, name, paneID, cwd, KindClaude, nil, nil) })
}

// startAgain is retryStart on herdr, with a note on stderr before the
// second start.
func startAgain(h *herdr.Herdr, name, paneID string, first, again func() error) error {
	return retryStart(first, again,
		func() (bool, error) { return paneHasNoAgent(h, paneID) },
		func(err error, attempt int) {
			fmt.Fprintf(os.Stderr, "note: %s did not start (attempt %d of %d) and the pane has no agent; "+
				"starting it again in %v: %v\n", name, attempt, startAttempts, startPause, err)
			time.Sleep(startPause)
		},
	)
}

// retryStart runs `first`, then `again` until a start succeeds,
// startAttempts times in all at most. A failure is retried only when it is
// not an exit 3 screen and `gone` says the agent's process has left the
// pane, so the pane is a shell again and a new start cannot run beside the
// old agent; `pause` comes before each retry. The last failure is
// returned.
func retryStart(first, again func() error, gone func() (bool, error), pause func(err error, attempt int)) error {
	start := first
	for attempt := 1; ; attempt++ {
		err := start()
		var failure *exit.Failure
		if err == nil || attempt == startAttempts || errors.As(err, &failure) && failure.Code == exit.Blocked {
			return err
		}
		empty, goneErr := gone()
		if goneErr != nil || !empty {
			return err
		}
		pause(err, attempt)
		start = again
	}
}

// paneHasNoAgent is whether herdr sees no agent process in the pane: its
// `agent` is null once the process has exited, and its kind ("claude",
// "pi") while it runs, even before it is ready.
func paneHasNoAgent(h *herdr.Herdr, paneID string) (bool, error) {
	pane, err := h.CallOK("pane", "get", paneID)
	if err != nil {
		return false, err
	}
	inner, ok := herdr.Lookup(pane, "pane").(map[string]any)
	if !ok {
		return false, exit.Environmentf("herdr reply has no pane")
	}
	return inner["agent"] == nil, nil
}

// startOnce is one `herdr agent start` and, when it got that far, the
// settling of the agent.
func startOnce(h *herdr.Herdr, name, paneID, cwd, kind string, model, effort *string) error {
	args := []string{"agent", "start", name, "--kind", kind, "--pane", paneID,
		"--timeout", startTimeoutMS, "--"}
	args = append(args, kindOf(kind).args(model, effort)...)
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
	return settleKind(h, name, paneID, cwd, kindOf(kind))
}

// SettleAgent renames the pane after the started Claude Code `name` and
// gets it to its input box, as StartAgent does after the start; also what
// ResumeAgent runs first. `cwd` is the agent's directory, the only
// one whose trust dialog is accepted.
func SettleAgent(h *herdr.Herdr, name, paneID, cwd string) error {
	return settleKind(h, name, paneID, cwd, kinds[KindClaude])
}

// settleKind is SettleAgent for an agent of kind `k`.
func settleKind(h *herdr.Herdr, name, paneID, cwd string, k agentKind) error {
	if _, err := h.CallOK("pane", "rename", paneID, name); err != nil {
		return err
	}
	return settle(h, name, cwd, k)
}

// settle gets the agent `name`, of kind `k`, to its input box through
// reachInputBox; exit 3 names the agent.
func settle(h *herdr.Herdr, name, cwd string, k agentKind) error {
	err := reachInputBox(
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
		func(key string) error {
			_, err := h.CallOK("agent", "send-keys", name, key)
			return err
		},
		cwd,
		k,
		10*time.Second,
		250*time.Millisecond,
	)
	var failure *exit.Failure
	if errors.As(err, &failure) && failure.Code == exit.Blocked {
		return exit.New(exit.Blocked, fmt.Sprintf("%s %s", name, failure.Message))
	}
	return err
}

// maxTrustKeys bounds the keys pressed at the folder-trust dialog: two
// get through it, the rest allow for lost presses.
const maxTrustKeys = 6

// reachInputBox gets a started agent of kind `k` to its input box. Ready
// takes both the input box on `screen` (herdr reports a ❯ menu as idle
// too, and Pi at any screen that is not working) and herdr's `status` idle
// or done (done: idle after a turn); herdr can still say blocked after the
// redraw, and a prompt is then refused as agent_blocked. The folder-trust
// dialog for `cwd` is answered one key at a time through `key`, each
// chosen from where the cursor is on the screen just read, and the screen
// read again after each (polling every `poll` for at most `wait`); a
// dialog for any other directory is left alone. A kind with no dialog to
// answer gets no key: its screen is polled the same way until the input
// box shows, as herdr may report it idle before its first frame. Anything
// else is exit 3 with the screen.
func reachInputBox(
	screen func() (string, error),
	status func() (string, error),
	key func(string) error,
	cwd string,
	k agentKind,
	wait time.Duration,
	poll time.Duration,
) error {
	settled := func(status string) bool { return status == "idle" || status == "done" }
	text, err := screen()
	if err != nil {
		return err
	}
	if k.trustKey == nil && !k.inputBox(text) {
		if text, err = pollScreen(screen, status, k.inputBox, text, wait, poll); err != nil {
			return err
		}
	}
	for range maxTrustKeys {
		if k.trustKey == nil {
			break
		}
		next, err := k.trustKey(text, cwd)
		if err != nil {
			return err
		}
		if next == "" {
			break
		}
		if err := key(next); err != nil {
			return err
		}
		if text, err = pollScreen(screen, status, k.inputBox, text, wait, poll); err != nil {
			return err
		}
	}
	state, err := status()
	if err != nil {
		return err
	}
	if k.inputBox(text) && settled(state) {
		return nil
	}
	return exit.New(exit.Blocked,
		fmt.Sprintf("is not ready at its input box (herdr status %q); its screen:\n%s", state, text))
}

// pollScreen reads the screen every `poll` until it shows the input box
// (as `inputBox` reads it) with herdr settled, or the folder-trust dialog
// redrawn (the cursor moved), or `wait` has passed; the last screen is
// returned. A screen in between (start-up after the dialog) is waited out.
func pollScreen(screen, status func() (string, error), inputBox func(string) bool, before string,
	wait, poll time.Duration) (string, error) {
	deadline := time.Now().Add(wait)
	for {
		time.Sleep(poll)
		text, err := screen()
		if err != nil {
			return "", err
		}
		if inputBox(text) {
			state, err := status()
			if err != nil {
				return "", err
			}
			if state == "idle" || state == "done" {
				return text, nil
			}
		}
		if _, isDialog := trustDialog(text); isDialog && text != before {
			return text, nil
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

// trustPrompt is a folder-trust dialog as Claude Code 2.1.296 draws it:
// "Accessing workspace:", the directory, "Quick safety check", the cancel
// option listed first, the cursor ❯ on one of them. Above it the pane may
// still show the shell's command line, its prompt a ❯ too.
type trustPrompt struct {
	// Dir is the directory the dialog names: the lines between the two
	// headings joined, so a path wrapped over two lines reads whole.
	Dir string
	// Highlighted is the line the cursor is on, without the cursor.
	Highlighted string
}

// trustDialog reads the folder-trust dialog off `screen`; ok is false for
// any other screen.
func trustDialog(screen string) (p trustPrompt, ok bool) {
	_, after, found := strings.Cut(screen, "Accessing workspace:")
	between, options, found2 := strings.Cut(after, "Quick safety check")
	if !found || !found2 {
		return trustPrompt{}, false
	}
	for _, line := range lines(between) {
		p.Dir += strings.TrimSpace(line)
	}
	for _, line := range lines(options) {
		line = strings.TrimLeftFunc(line, unicode.IsSpace)
		if rest, found := strings.CutPrefix(line, "❯"); found {
			p.Highlighted = strings.TrimSpace(rest)
			break
		}
	}
	return p, true
}

// TrustKey is the one key to press next when `screen` is the folder-trust
// dialog for `cwd`: `down` while the cursor is on the cancel option, `enter`
// once it is on "Yes, I trust this folder". "" for any other screen. A
// dialog naming another directory (the only one fleet may trust is the one
// it chose for the agent), or one whose cursor cannot be placed, is exit 3
// with the screen, nothing pressed.
func TrustKey(screen, cwd string) (string, error) {
	p, ok := trustDialog(screen)
	if !ok {
		return "", nil
	}
	if !sameDir(p.Dir, cwd) {
		return "", exit.New(exit.Blocked, fmt.Sprintf("is at the folder-trust dialog for %s, not its own directory %s; "+
			"nothing pressed; its screen:\n%s", p.Dir, cwd, screen))
	}
	switch {
	case strings.Contains(p.Highlighted, "Yes, I trust this folder"):
		return "enter", nil
	case strings.HasPrefix(p.Highlighted, "No, ") && strings.Contains(screen, "Yes, I trust this folder"):
		return "down", nil
	}
	return "", exit.New(exit.Blocked, fmt.Sprintf("is at the folder-trust dialog with the cursor on %q; "+
		"nothing pressed; its screen:\n%s", p.Highlighted, screen))
}

// sameDir is whether the directory a dialog names is `cwd`, as written or
// with its symlinks resolved (Claude Code may print the resolved path).
func sameDir(named, cwd string) bool {
	if named == "" || cwd == "" {
		return false
	}
	if named == cwd {
		return true
	}
	resolved, err := canonicalize(cwd)
	return err == nil && named == resolved
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

// IsPiInputBox is whether `screen` shows Pi's editor. Pi draws the editor
// between two borders, above its footer: the text typed so far, or one
// empty line. Every dialog (project trust, a model picker, an extension's
// confirmation or selection) takes the editor's place between the same
// borders, but opens with an empty line and goes on; herdr reports Pi idle
// at those too.
func IsPiInputBox(screen string) bool {
	_, ok := piEditorTop(lines(screen))
	return ok
}

// piEditorTop is the index of the top border of Pi's editor in `all`, the
// screen's lines; ok is false when the last frame on the screen is not the
// editor, or there is none.
func piEditorTop(all []string) (top int, ok bool) {
	bottom := -1
	for i := len(all) - 1; i >= 0; i-- {
		if !piBorder(all[i]) {
			continue
		}
		if bottom < 0 {
			bottom = i
			continue
		}
		body := all[i+1 : bottom]
		return i, len(body) == 1 || len(body) > 1 && strings.TrimSpace(body[0]) != ""
	}
	return 0, false
}

// piBorder is a border of the frame Pi draws its editor in: a line of ─,
// where the top one may carry the working status (`── ⠋ Working ───`) or a
// scroll mark between them.
func piBorder(line string) bool {
	line = strings.TrimSpace(line)
	return utf8.RuneCountInString(line) >= 10 && strings.HasPrefix(line, "──") && strings.HasSuffix(line, "─")
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
// times the scope's load multiplier and MemAvailable is at least its
// floor plus one agent's share; a `light` agent skips the load. Inputs
// are the texts of /proc/loadavg and /proc/meminfo.
func CheckResources(loadavg string, cpus int, meminfo string, sc *config.Scope, light bool) error {
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
	if limit := float64(cpus) * sc.LoadMultiplier; !light && load >= limit {
		return exit.Refusedf("the 1-minute load average is %s, not below %.4g (%d CPUs × load_multiplier %g); "+
			"try again later", strconv.FormatFloat(load, 'f', -1, 64), limit, cpus, sc.LoadMultiplier)
	}
	if need := uint64(sc.MinAvailableMiB+sc.AgentMiB) * 1024; availableKiB < need {
		return exit.Refusedf("available memory is %d MiB, below %d MiB (min_available_mib %d + agent_mib %d "+
			"for the new agent); try again later", availableKiB/1024, need/1024, sc.MinAvailableMiB, sc.AgentMiB)
	}
	return nil
}

// machineResources is CheckResources on this machine; a variable so a
// test can stand in for the machine. `runtime.NumCPU` honors the affinity
// mask, not a cgroup CPU quota: in a container with a quota the load may
// be compared with more CPUs than the quota allows.
var machineResources = func(sc *config.Scope, light bool) error {
	loadavg, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return exit.IO(err)
	}
	meminfo, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return exit.IO(err)
	}
	return CheckResources(string(loadavg), runtime.NumCPU(), string(meminfo), sc, light)
}

// resources is machineResources with the scope's settings, unless the
// repo's config turns the check off.
func resources(cfg *config.Config, sc *config.Scope, light bool) error {
	if !cfg.ResourceCheck {
		return nil
	}
	return machineResources(sc, light)
}

// CheckName refuses a name that is empty, has characters outside
// [a-z0-9-], or is `watch`, the sender name of `fleet watch`.
func CheckName(name string) error {
	ok := name != ""
	for _, c := range name {
		ok = ok && (c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-')
	}
	if !ok {
		return exit.Refusedf("name %q must use only [a-z0-9-]", name)
	}
	if name == watchSender {
		return exit.Refusedf("the name `watch` is reserved")
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

// CheckRepo refuses what is not one directory name: a checkout under the
// scope's `paths.checkouts` or `paths.initiatives`.
func CheckRepo(repo string) error {
	if repo == "" || repo == "." || repo == ".." || strings.Contains(repo, "/") {
		return exit.Refusedf("<repo> %q must be one directory name", repo)
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

// workspacePanes is how many panes herdr has in workspace `id`.
func workspacePanes(h *herdr.Herdr, id string) (int, error) {
	list, err := h.CallOK("pane", "list", "--workspace", id)
	if err != nil {
		return 0, err
	}
	panes, ok := herdr.Lookup(list, "panes").([]any)
	if !ok {
		return 0, exit.Environmentf("herdr pane list: no panes array in the reply")
	}
	return len(panes), nil
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

// WorkOrderTitle is the title of a work order for `task`: the first line
// that is neither empty nor a heading below the top level (`##` and
// deeper), without a leading `# `, cut to 80 characters with `…` at the
// end. A task that opens with a card's JSON, alone or followed by its
// link, is titled after the card's question.
func WorkOrderTitle(task string) string {
	var card struct {
		Question string `json:"question"`
	}
	if strings.HasPrefix(strings.TrimSpace(task), "{") &&
		json.NewDecoder(strings.NewReader(task)).Decode(&card) == nil && card.Question != "" {
		task = card.Question
	}
	for _, line := range lines(task) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "##") {
			continue
		}
		if line = strings.TrimSpace(strings.TrimPrefix(line, "#")); line == "" {
			continue
		}
		if runes := []rune(line); len(runes) > 80 {
			line = strings.TrimSpace(string(runes[:79])) + "…"
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
// team and project from `.fleet/config.json` of its main checkout; a cross-repo
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

// jobConfig is the config a job runs under: `.fleet/config.json` of the
// lead's directory `leadCwd`, which for a single-repo job is the repo's
// main checkout, or for a cross-repo job of the initiative checkout that
// holds `leadCwd`. A cross-repo job's `linear` is not used (jobLinear).
func jobConfig(repo, leadCwd string) (*config.Config, error) {
	if repo == "" {
		return config.Load(filepath.Dir(leadCwd))
	}
	return config.Load(leadCwd)
}

// scopeOf is the claim scope of a job's work orders: `<repo>: job <job>`,
// or `cross-repo: job <job>`.
func scopeOf(repo, job string) string {
	if repo == "" {
		repo = "cross-repo"
	}
	return repo + ": job " + job
}

// workOrder creates `agent`'s work order under `parent`, labelled with
// the agent's role (`lead` or `worker`, as thread tickets are `thread`),
// and claims it in the agent's name, when the job uses Linear; otherwise
// it returns the zero Issue. The identifier is written onto the agent's
// reserved row as soon as the issue exists, before the claim, so a
// failure later leaves it on the row. An issue that was created is in
// `created`, claimed or not.
func workOrder(conn querier, l *linear, parent, title, task string, id *identity.Identity, source, scope string, created *[]string) (atb.Issue, error) {
	if l == nil {
		return atb.Issue{}, nil
	}
	agent := id.Agent
	issue, err := atb.Create(l.team, l.project, parent, id.Role.String(), title, task)
	if err != nil {
		return atb.Issue{}, err
	}
	*created = append(*created, fmt.Sprintf("work order %s (%s), not claimed", issue.Identifier, issue.URL))
	if err := setIssue(conn, agent, issue.Identifier); err != nil {
		return atb.Issue{}, err
	}
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
func insertStarting(conn querier, id *identity.Identity, cwd, task, parentIssue string, light bool) error {
	if _, err := conn.Exec(
		"INSERT INTO agents (name, role, job, cwd, parent, report_to, task, state, started_at, issue, parent_issue, thread, light) "+
			"VALUES (?1, ?2, ?3, ?4, ?5, ?5, ?6, 'starting', ?7, ?8, ?9, ?10, ?11)",
		id.Agent, id.Role.String(), id.Job, cwd, id.Parent, task, db.Now(), id.Issue, parentIssue, id.Thread, light); err != nil {
		return exit.Database(err)
	}
	return nil
}

// setIssue records the agent's work order on its reserved row.
func setIssue(conn querier, agent, issue string) error {
	if _, err := conn.Exec("UPDATE agents SET issue = ?1 WHERE name = ?2 AND state != 'ended'",
		issue, agent); err != nil {
		return exit.Database(err)
	}
	return nil
}

//go:embed lead_prompt.md
var leadPrompt string

//go:embed worker_prompt.md
var workerPrompt string

// roleSlots are the values of a lead's or a worker's prompt slots: the
// agent's identity, where it runs, what Linear holds for it, where its
// files go, and the model slots a replacement may still use. `cap` is the job's agent cap;
// `issue` the agent's work order (zero without Linear); `parentIssue` the
// job's parent issue; `leadCwd` where the job's lead runs: the checkout
// of a single-repo job, whose repo is `repo`, or `<job>/` in the
// initiative's checkout of a cross-repo job; `sc` the scope's paths,
// channels and models.
func roleSlots(sc *config.Scope, id *identity.Identity, cwd, repo, leadCwd string, cap int, issue atb.Issue, parentIssue string) map[string]string {
	issueNote := " (your work order, " + issue.URL + ")"
	if issue.Identifier == "" {
		issueNote = " (empty: this job has no Linear work orders)"
	}
	linear := "Linear is off for this job: no work orders, no parent issue; task files and reports are still files."
	if issue.Identifier != "" {
		linear = fmt.Sprintf("The job's parent issue is %s; fleet claims and releases it and every work order of "+
			"the job (`fleet job end --help` says when to add `--close-parent`).", parentIssue)
	}
	checkouts := sc.Tilde(sc.Paths.Checkouts)
	var repoLine, agentsMD string
	if repo != "" {
		checkout := sc.Tilde(leadCwd)
		repoLine = fmt.Sprintf("The job is single-repo: its repo is %s, main checkout %s; `<repo>` below is %s.",
			repo, checkout, repo)
		agentsMD = fmt.Sprintf("Before anything else, read the rules in %s, and follow them.", repoRules(checkout))
	} else {
		root := filepath.Dir(leadCwd)
		initiative := sc.Channels.InitiativePrefix + filepath.Base(root)
		repoLine = fmt.Sprintf("The job is cross-repo: it runs inside the checkout of the initiative %s (%s); "+
			"`<repo>` below is the repo a task names, main checkout %s/<repo>.", initiative, root, checkouts)
		agentsMD = fmt.Sprintf("Before anything else, read %s (the initiative's charter), then the rules in %s "+
			"of every repo R the job touches, and follow them.", filepath.Join(root, "AGENTS.md"), repoRules(checkouts+"/<R>"))
	}
	slots := map[string]string{"job": id.Job, "scope": id.Scope, "agent": id.Agent, "parent": id.Parent,
		"lead": id.Parent, "one_task": fmt.Sprintf("one task in the job %s (scope %s), for your lead %s", id.Job, id.Scope,
			id.Parent), "cwd": cwd, "repo_line": repoLine, "issue": issue.Identifier, "issue_note": issueNote,
		"linear": linear, "agents_md": agentsMD, "cap": strconv.Itoa(cap),
		"scratch": sc.Tilde(jobScratch(sc, repo, leadCwd))}
	maps.Copy(slots, modelValues(sc))
	return slots
}

// repoRules names the files that hold the rules of the repo checked out
// at `checkout`, whether its AGENTS.md is compiled or written by hand.
func repoRules(checkout string) string {
	return fmt.Sprintf("%[1]s/AGENTS.md (and %[1]s/AGENTS.override.md and %[1]s/CLAUDE.local.md where present)", checkout)
}

// jobScratch is the directory for the files of a job's agents, their
// `FLEET_SCRATCH`: `<scratch>/<repo>` for a single-repo job, else
// `<scratch>/<initiative prefix><I>` for the initiative whose checkout
// holds the lead's directory `leadCwd`.
func jobScratch(sc *config.Scope, repo, leadCwd string) string {
	if repo == "" {
		repo = sc.Channels.InitiativePrefix + filepath.Base(filepath.Dir(leadCwd))
	}
	return filepath.Join(sc.Paths.Scratch, repo)
}

// orDefault is the --model or --effort a start passes: the command's
// `flag`, else the scope's `value` for it; nil when neither is given, so
// the agent runtime's own default holds.
func orDefault(flag *string, value string) *string {
	if flag != nil || value == "" {
		return flag
	}
	return &value
}

// lightNote is what a `--light` agent is told: it got past the load check
// on the promise that it only reads, which fleet does not enforce.
func lightNote(light bool) string {
	if !light {
		return ""
	}
	return "You were started with `--light`: fleet skipped its load check for you, because you only read and do " +
		"I/O. Do not compile, run test suites, or start builds or other CPU-heavy commands (`cargo build`, " +
		"`cargo test`, `go test`, `npm run build`, `hugo`, `docker` and the like). When the work needs one, " +
		"stop and tell whoever gave you this task. fleet does not enforce this; it relies on you."
}

// taskSection is the last part of a first message, `## Your task`: the
// work order's URL when the agent has one, then the task file as it is,
// to the end of the message. fleet places the heading, so the task file
// cannot move where the task begins; the prompts say that everything
// from it on is the task, whatever it looks like.
func taskSection(url, task string) string {
	text := "\n## Your task\n\n"
	if url != "" {
		text += "Work order: " + url + "\n\n"
	}
	return text + task
}

// startAndDeliver starts the agent of kind `kind` in `place` (its
// directory `cwd`), delivers `body` (its first message, after the header `sender` adds) and
// marks the row active.
// Every step appends what it made to `created`. The row is marked active
// only when herdr reported the message delivered, so a row still
// `starting` means the agent may not have its task.
func startAndDeliver(h *herdr.Herdr, conn *sql.DB, id *identity.Identity, place Place, cwd, kind string,
	model, effort *string, sender, body string, created *[]string) (exit.Code, error) {
	if _, err := conn.Exec("UPDATE agents SET pane_id = ?1 WHERE name = ?2 AND state != 'ended'",
		place.PaneID, id.Agent); err != nil {
		return 0, exit.Database(err)
	}
	if err := StartAgent(h, id.Agent, place.PaneID, cwd, kind, model, effort); err != nil {
		return 0, err
	}
	*created = append(*created, fmt.Sprintf("agent %s in pane %s", id.Agent, place.PaneID))
	text, err := WithHeader(sender, body)
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
		atb.WriteStderr(os.Stderr, err)
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

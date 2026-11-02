// What `watch` posts when an agent's screen shows a usage limit or quota
// error (docs/design.md): a message of fixed form in the agent's thread,
// with no model called, once while the error stays and once when it is
// gone.

package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/fednet"
	"github.com/Luolc/agent-fleet/internal/herdr"
)

// quotaPhrases are the phrases, lower-cased, that make an error a usage
// limit or quota error.
var quotaPhrases = []string{"quota exhausted", "usage limit", "rate limit", "rate-limit", "rate_limit",
	"limiting requests", "limit reached", "hit your limit", "reached your limit", "limit will reset"}

// status429 is HTTP's status for too many requests, which makes an `API
// Error` line a quota error too.
var status429 = regexp.MustCompile(`\b429\b`)

// resetWords and rfc3339 are what resetPattern and resetTime are made of.
const (
	resetWords = `(?i)\b(?:earliest (?:reset|recovery)|resets(?: at)?|reset at)[ \t]+`
	rfc3339    = `\d{4}-\d\d-\d\dT[0-9:.]+(?:Z|[+-]\d\d:\d\d)`
)

// resetPattern is the reset time an error gives, as it gives it: an
// RFC 3339 time whole, else the words up to the next separator;
// resetTime only the time.
var (
	resetPattern = regexp.MustCompile(resetWords + `(` + rfc3339 + `|[^·;,\n]+)`)
	resetTime    = regexp.MustCompile(resetWords + `(` + rfc3339 + `)`)
)

const (
	// maxErrorLines bounds the lines an `API Error` line is read over:
	// its own and those Claude Code wrapped it onto.
	maxErrorLines = 8
	// shellTail is how many of a pane's last non-blank lines are read
	// when its agent is not running in it: the shell's prompt, of one
	// or two lines, and the line before it, which the terminal may have
	// wrapped onto two.
	shellTail = 4
	// quotaKey prefixes the steps of an agent's quota error: `seen`, done
	// when watch first found it, and `posted`, once the thread was told.
	quotaKey = "quota:"
)

// quotaState is what one run reads of an agent's quota error.
type quotaState int

const (
	// quotaUnknown is a screen that cannot tell: what is recorded stays.
	quotaUnknown quotaState = iota
	// quotaClear is a screen that shows the agent past any such error.
	quotaClear
	// quotaShown is a screen that shows one.
	quotaShown
)

// quotaAgent is an agent row in a quota message: since is when watch first
// found its error, reset the reset time its screen gives, or "unknown".
type quotaAgent struct {
	id          int64
	name, reset string
	since       int64
}

// quotaSteps is the record of an agent row's quota error.
type quotaSteps struct {
	seen   int64
	posted bool
}

// quota looks at the screen of every live agent but the ones watch
// started and tells the agent's thread, in one message per thread and
// run, which agents newly show a usage limit or quota error, and later
// which no longer do. Nothing is pressed, and nothing is done without a
// fednet socket. An agent at a usage-limit menu is the screen rules'
// (unblock.go), which ask the people on a card.
func (w *workers) quota() error {
	if w.cfg.FednetSocket == "" {
		return nil
	}
	recorded, err := quotaRecords(w.watchRun)
	if err != nil {
		return err
	}
	rows, err := stopRows(w.conn)
	if err != nil {
		return err
	}
	live := map[int64]bool{}
	shown, gone := map[string][]quotaAgent{}, map[string][]quotaAgent{}
	for _, row := range rows {
		if row.startedByWatch() {
			continue
		}
		live[row.ID] = true
		if w.outOfTime() {
			return nil
		}
		if err := w.quotaRow(row, recorded, shown, gone); err != nil {
			return err
		}
	}
	for id := range recorded {
		if !live[id] {
			if err := w.clearQuota(id); err != nil {
				return err
			}
		}
	}
	return w.postQuota(shown, gone)
}

// quotaRow reads `row`'s screen against its `recorded` error: a new one
// is recorded as seen and goes into `shown`, a posted one found gone into
// `gone`, one never posted and found gone is deleted.
func (w *workers) quotaRow(row stopRow, recorded map[int64]quotaSteps, shown, gone map[string][]quotaAgent) error {
	state, reset, err := w.quotaRead(row)
	if err != nil {
		w.failed(fmt.Errorf("quota %s: %w", row.Name, err))
		return nil
	}
	steps, had := recorded[row.ID]
	switch {
	case state == quotaShown && !steps.posted:
		if !had {
			if err := w.quotaStep(row.ID, "seen"); err != nil {
				return err
			}
			steps.seen = w.now
		}
		return w.quotaTell(row, shown, quotaAgent{id: row.ID, name: row.Name, reset: reset, since: steps.seen}, false)
	case state == quotaClear && steps.posted:
		return w.quotaTell(row, gone, quotaAgent{id: row.ID, name: row.Name, since: steps.seen}, true)
	case state == quotaClear && had:
		return w.clearQuota(row.ID)
	}
	return nil
}

// quotaTell adds `a`, the agent of `row`, to `by` under its home thread
// (homeThread). Without one, an error found `past` has its record
// deleted, and a new one is only named on stdout.
func (w *workers) quotaTell(row stopRow, by map[string][]quotaAgent, a quotaAgent, past bool) error {
	thread, err := w.homeThread(row)
	switch {
	case err != nil:
		w.failed(fmt.Errorf("quota %s: %w", row.Name, err))
	case thread != "":
		by[thread] = append(by[thread], a)
	case past:
		return w.clearQuota(row.ID)
	default:
		fmt.Fprintf(os.Stdout, "quota: %s shows a usage limit or quota error; it has no thread to tell\n", row.Name)
	}
	return nil
}

// quotaRead is what this run reads on `row`'s screen. An agent herdr
// has is read off its screen while it is not working, so a retry is not
// taken for a recovery; a row's pane its agent is not running in, off
// the shell's last lines.
func (w *workers) quotaRead(row stopRow) (quotaState, string, error) {
	if agent, ok := w.inHerdr[row.Name]; ok {
		if agent.Status == "working" {
			return quotaUnknown, "", nil
		}
		screen, err := w.h.Screen(row.Name)
		if err != nil {
			return quotaUnknown, "", err
		}
		state, reset := quotaOnScreen(agent.Status, screen, row.Cwd)
		return state, reset, nil
	}
	if pane, ok := w.panes[row.Pane]; !ok || row.Pane == "" || pane.agent {
		return quotaUnknown, "", nil
	}
	screen, err := paneScreen(w.h, row.Pane)
	if err != nil {
		return quotaUnknown, "", err
	}
	state, reset := quotaOnShell(screen)
	return state, reset, nil
}

// quotaOnScreen reads the screen of an agent herdr reports with `status`,
// whose directory is `cwd`. Blocked at a usage-limit menu, it is unknown
// here, the card's; blocked at any other dialog, past the error, since
// the agent got a reply to get there. At its input box, whatever herdr
// says, only the agent's own error decides: Claude Code's `API Error`
// line naming a usage limit or quota (or HTTP 429), with the lines it
// wrapped onto, as the last entry above the input box; whatever the
// transcript quotes above it cannot. Any other screen is unknown.
func quotaOnScreen(status, screen, cwd string) (quotaState, string) {
	all := lines(screen)
	end, box := claudeBoxTop(all)
	if !box && status == "blocked" {
		if _, ok := dialogQuota(screen, cwd); ok {
			return quotaUnknown, ""
		}
		return quotaClear, ""
	}
	if !box {
		return quotaUnknown, ""
	}
	for end > 0 && besideTranscript(all[end-1]) {
		end--
	}
	text, ok := lastAPIError(all[:end])
	if !ok || !hasQuotaPhrase(text) && !strings.Contains(strings.ToLower(text), "quota") && !status429.MatchString(text) {
		return quotaClear, ""
	}
	return quotaShown, resetOf(resetPattern, text)
}

// lastAPIError is the last entry of `transcript` when it is an `API
// Error` line, joined with the lines Claude Code wrapped it onto.
func lastAPIError(transcript []string) (string, bool) {
	for i := len(transcript) - 1; i >= 0 && i >= len(transcript)-maxErrorLines; i-- {
		head := entryText(transcript[i])
		if strings.HasPrefix(head, "API Error") {
			parts := []string{head}
			for _, line := range transcript[i+1:] {
				parts = append(parts, strings.TrimSpace(line))
			}
			return strings.Join(parts, " "), true
		}
		if !wrapped(transcript[i]) {
			break
		}
	}
	return "", false
}

// quotaOnShell reads the pane of an agent that is not running in it, as
// a start that failed left it: shown when one of its last shellTail
// non-blank lines names a usage limit or quota, else unknown. A line the
// terminal wrapped is read row by row, so the reset time is taken only
// as an RFC 3339 time, from those lines joined, so a time cut by the wrap
// reads whole or not at all.
func quotaOnShell(screen string) (quotaState, string) {
	var kept []string
	for _, line := range lines(screen) {
		if strings.TrimSpace(line) != "" {
			kept = append(kept, line)
		}
	}
	tail := kept[max(len(kept)-shellTail, 0):]
	if !slices.ContainsFunc(tail, hasQuotaPhrase) {
		return quotaUnknown, ""
	}
	return quotaShown, resetOf(resetTime, strings.Join(tail, ""))
}

// dialogQuota is whether `screen` is a usage-limit menu: a screen fleet
// never answers whose dialog names a usage limit or quota; with the reset
// time it gives.
func dialogQuota(screen, cwd string) (string, bool) {
	if classifyScreen(screen, cwd) != handsOffScreen {
		return "", false
	}
	dialog := dialogPart(screen)
	if !hasQuotaPhrase(dialog) {
		return "", false
	}
	return resetOf(resetPattern, dialog), true
}

func hasQuotaPhrase(text string) bool {
	text = strings.ToLower(text)
	return slices.ContainsFunc(quotaPhrases, func(phrase string) bool { return strings.Contains(text, phrase) })
}

// resetOf is the reset time `text` gives as `pattern` finds it, at most
// 40 characters, or "unknown".
func resetOf(pattern *regexp.Regexp, text string) string {
	m := pattern.FindStringSubmatch(text)
	if m == nil {
		return "unknown"
	}
	reset := strings.TrimRight(strings.TrimSpace(m[1]), ".")
	for utf8.RuneCountInString(reset) > 40 {
		_, size := utf8.DecodeLastRuneInString(reset)
		reset = reset[:len(reset)-size]
	}
	return strings.TrimSpace(reset)
}

// entryText is a line of Claude Code's transcript without its marker:
// `⏺` (`●` off macOS) for a message, `⎿` for what hangs under one.
func entryText(line string) string {
	text := strings.TrimSpace(line)
	for _, marker := range []string{"⏺", "●", "⎿"} {
		if rest, ok := strings.CutPrefix(text, marker); ok {
			return strings.TrimSpace(rest)
		}
	}
	return text
}

// wrapped is a line Claude Code wrapped an entry onto: indented, with no
// marker and not a prompt.
func wrapped(line string) bool {
	text := strings.TrimSpace(line)
	return text != "" && unicode.IsSpace([]rune(line)[0]) && entryText(line) == text &&
		!strings.HasPrefix(text, ">") && !strings.HasPrefix(text, "❯")
}

// besideTranscript is a line Claude Code draws between the transcript
// and the input box (isStatusLine), but an `API Error` under `⎿` and a
// wrapped line, which are the transcript's.
func besideTranscript(line string) bool {
	if strings.HasPrefix(entryText(line), "API Error") {
		return false
	}
	return isStatusLine(line) && (!wrapped(line) || len(line)-len(strings.TrimLeftFunc(line, unicode.IsSpace)) >= 20)
}

// paneScreen is the visible screen of `pane`, for an agent that is not
// running in it and so cannot be read by name.
func paneScreen(h *herdr.Herdr, pane string) (string, error) {
	out, errOut, err := h.Run("pane", "read", pane, "--source", "visible")
	var failure *exit.Failure
	if errors.As(err, &failure) {
		return "", failure
	}
	if err != nil {
		return "", exit.Environmentf("herdr pane read %s failed (%s): %s", pane, herdr.Status(err),
			firstLine(strings.TrimSpace(string(errOut))))
	}
	return string(out), nil
}

// quotaRecords are the recorded quota errors, by agent row id.
func quotaRecords(r *watchRun) (map[int64]quotaSteps, error) {
	recorded := map[int64]quotaSteps{}
	err := eachRow(r.conn, "SELECT key, step, done_at FROM steps WHERE key LIKE ?1", func(rows *sql.Rows) error {
		var key, step string
		var at int64
		if err := rows.Scan(&key, &step, &at); err != nil {
			return err
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(key, quotaKey), 10, 64)
		if err != nil {
			return nil
		}
		steps := recorded[id]
		switch step {
		case "seen":
			steps.seen = at
		case "posted":
			steps.posted = true
		}
		recorded[id] = steps
		return nil
	}, quotaKey+"%")
	return recorded, err
}

// quotaStep records `step` of row `id`'s quota error at the run's clock.
func (w *workers) quotaStep(id int64, step string) error {
	_, err := w.conn.Exec("INSERT OR IGNORE INTO steps (key, step, done_at) VALUES (?1, ?2, ?3)",
		quotaKey+strconv.FormatInt(id, 10), step, w.now)
	return dbErr(err)
}

// clearQuota deletes the record of row `id`'s quota error, so its next
// one is told again.
func (w *workers) clearQuota(id int64) error {
	_, err := w.conn.Exec("DELETE FROM steps WHERE key = ?1", quotaKey+strconv.FormatInt(id, 10))
	return dbErr(err)
}

// postQuota posts, in thread order, one message per thread of `shown`
// naming its agents with a new quota error, then one per thread of `gone`
// naming those past it; only a post that went out is recorded (`posted`,
// or the record deleted), so a failed one is posted next run.
func (w *workers) postQuota(shown, gone map[string][]quotaAgent) error {
	host := machine()
	posted := func(id int64) error { return w.quotaStep(id, "posted") }
	for _, thread := range slices.Sorted(maps.Keys(shown)) {
		text := fmt.Sprintf("fleet watch on %s found an error naming a quota, usage limit or rate limit on these "+
			"agents' screens (fleet's own check, no model called):\n", host)
		for _, a := range shown[thread] {
			text += fmt.Sprintf("- %s: seen since %s; reset: %s\n", a.name, stamp(a.since), a.reset)
		}
		if err := w.postQuotaTo(thread, text, shown[thread], posted); err != nil {
			return err
		}
	}
	for _, thread := range slices.Sorted(maps.Keys(gone)) {
		text := fmt.Sprintf("fleet watch on %s no longer finds that error on these agents' screens:\n", host)
		for _, a := range gone[thread] {
			text += fmt.Sprintf("- %s: seen since %s; gone by %s\n", a.name, stamp(a.since), stamp(w.now))
		}
		if err := w.postQuotaTo(thread, text, gone[thread], w.clearQuota); err != nil {
			return err
		}
	}
	return nil
}

// postQuotaTo posts `text` to `thread` and, once it is out, runs `done`
// for each of `agents`; a post that fails is the run's failure.
func (w *workers) postQuotaTo(thread, text string, agents []quotaAgent, done func(id int64) error) error {
	if err := fednet.Post(w.cfg.FednetSocket, thread, text); err != nil {
		w.failed(fmt.Errorf("quota: posting to thread %s: %w", thread, err))
		return nil
	}
	var names []string
	for _, a := range agents {
		if err := done(a.id); err != nil {
			return err
		}
		names = append(names, a.name)
	}
	fmt.Fprintf(os.Stdout, "quota: posted to thread %s about %s\n", thread, strings.Join(names, ", "))
	return nil
}

// stamp is a time of the run's clock as the messages give it.
func stamp(at int64) string {
	return time.Unix(at, 0).UTC().Format(time.RFC3339)
}

// machine is this machine's host name, "unknown" when it has none.
func machine() string {
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return "unknown"
}

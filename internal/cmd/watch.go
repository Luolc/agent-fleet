// `fleet watch`: the cron check for stuck agents (docs/design.md).

package cmd

import (
	"database/sql"
	"fmt"
	"hash/fnv"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
)

// staleSecs is the number of seconds without any change before an agent is
// a suspect (30 minutes).
const staleSecs int64 = 30 * 60

// staleEnv overrides staleSecs, so the end-to-end test does not wait 30
// minutes.
const staleEnv = "FLEET_WATCH_STALE_SECS"

// The agent that is told about suspects, and the reserved sender name.
const (
	orchestra = "orchestra"
	sender    = "cron"
)

// WatchAbout and WatchLongAbout are the help texts of `watch`.
const (
	WatchAbout     = "Cron check for stuck agents; notifies the orchestra when the suspect set changes"
	WatchLongAbout = "Cron check for stuck agents; notifies the orchestra when the suspect set changes.\n\n" +
		"Meant for cron every 15 minutes, with --session <dataset> and --repo. It looks at the " +
		"live lead and worker rows of the ledger; the orchestra and human-interface are idle by " +
		"design and are not watched. For each agent it reads `agent_status` and " +
		"`state_change_seq` from herdr and hashes the visible screen with the spinner line, the " +
		"input box and the status footer stripped (the spinner timer and the footer's countdowns " +
		"change even when the agent is stuck). An agent is a suspect when all three have been " +
		"unchanged for 30 minutes, or when it is gone from herdr.\n\n" +
		"Only when the set of suspects changes is the orchestra told, through the same path as " +
		"`fleet send`, with the header `[FROM: cron]` and the evidence for each suspect. It " +
		"never calls a model and never sends keys to a suspect. The screen filter knows only the " +
		"screen of Claude Code, currently the only supported agent.\n\n" +
		"Exit: 0 when the check ran (and the orchestra was told, if the set changed); 1 when " +
		"no repo is given; 2, 3 or 4 when telling the orchestra gave no clear signal, found it " +
		"blocked, or did not find it, as for `send` (on 3 and 4 the next run tells it again); 5 " +
		"when herdr or the database fails, or the ledger does not exist."
)

// WatchArgs are the arguments of `watch`.
type WatchArgs struct {
	// Repo is the dataset repo whose ledger to read (`owner/name` or
	// `name`); the ledger is ~/scratch/<name>/fleet.db. Defaults to
	// FLEET_REPO; cron must pass it.
	Repo *string
}

// reading is what `watch` read for one agent this run.
type reading struct {
	status string
	seq    int64
	hash   string
	// tail is the last lines of the filtered screen, kept as evidence.
	tail string
}

// watched is one watched agent after this run: its ledger row, what was
// read (nil when it is gone from herdr) and the resulting verdict.
type watched struct {
	live         Live
	reading      *reading
	lastChangeAt *int64
	suspect      bool
}

// Watch runs `watch`.
func Watch(h *herdr.Herdr, args WatchArgs) (exit.Code, error) {
	conn, err := OpenLedger(args.Repo)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	stale := watchStaleSecs()
	now := db.Now()
	inHerdr, err := HerdrAgents(h)
	if err != nil {
		return 0, err
	}
	all, err := watchRead(h, conn, inHerdr, now, stale)
	if err != nil {
		return 0, err
	}

	var before, after []string
	for _, w := range all {
		if w.live.Suspect {
			before = append(before, w.live.Name)
		}
		if w.suspect {
			after = append(after, w.live.Name)
		}
	}
	before, after = nameSet(before), nameSet(after)

	if err := saveReadings(conn, all); err != nil {
		return 0, err
	}
	for _, w := range all {
		if w.suspect {
			fmt.Fprintf(os.Stdout, "suspect: %s\n", evidence(w, now))
		}
	}
	if slices.Equal(before, after) {
		fmt.Fprintf(os.Stdout, "suspect set unchanged (%d suspect)\n", len(after))
		return exit.Ok, nil
	}

	text, err := WithHeader(sender, notice(all, before, now))
	if err != nil {
		return 0, err
	}
	code, err := Deliver(h, orchestra, text)
	if err != nil {
		return 0, err
	}
	// Without a clear signal (exit 2) the notice may have arrived; it is not
	// resent blindly. A blocked or missing orchestra is told again next run.
	if code == exit.Ok || code == exit.Unknown {
		if err := saveSuspects(conn, all); err != nil {
			return 0, err
		}
	}
	return code, nil
}

// watchRead reads every live lead and worker from herdr and compares it with
// the ledger.
func watchRead(h *herdr.Herdr, conn *sql.DB, inHerdr map[string]InHerdr, now, stale int64) ([]watched, error) {
	rows, err := LiveRows(conn, nil)
	if err != nil {
		return nil, err
	}
	var all []watched
	for _, live := range rows {
		if live.Role != "lead" && live.Role != "worker" {
			continue
		}
		var r *reading
		if agent, ok := inHerdr[live.Name]; ok {
			screen, err := h.Screen(live.Name)
			if err != nil {
				return nil, err
			}
			filtered := FilterClaudeScreen(screen)
			r = &reading{
				status: agent.Status,
				seq:    agent.Seq,
				hash:   Hash(filtered),
				tail:   tail(filtered, 5),
			}
		}
		all = append(all, observe(live, r, now, stale))
	}
	return all, nil
}

// nameSet is `BTreeSet<String>`: sorted, without duplicates.
func nameSet(names []string) []string {
	slices.Sort(names)
	return slices.Compact(names)
}

func watchStaleSecs() int64 {
	if stale, err := strconv.ParseInt(os.Getenv(staleEnv), 10, 64); err == nil {
		return stale
	}
	return staleSecs
}

// observe compares this run's reading with the last one in the ledger. Any
// difference in status, seq or screen hash restarts the clock.
func observe(live Live, r *reading, now, stale int64) watched {
	if r == nil {
		var lastChangeAt *int64
		if live.LastChangeAt.Valid {
			at := live.LastChangeAt.Int64
			lastChangeAt = &at
		}
		return watched{live: live, lastChangeAt: lastChangeAt, suspect: true}
	}
	unchanged := live.LastStatus.Valid && live.LastStatus.String == r.status &&
		live.LastSeq.Valid && live.LastSeq.Int64 == r.seq &&
		live.LastScreenHash.Valid && live.LastScreenHash.String == r.hash
	lastChangeAt := now
	if live.LastChangeAt.Valid && unchanged {
		lastChangeAt = live.LastChangeAt.Int64
	}
	return watched{
		live:         live,
		reading:      r,
		lastChangeAt: &lastChangeAt,
		suspect:      now-lastChangeAt >= stale,
	}
}

func saveReadings(conn *sql.DB, all []watched) error {
	for _, w := range all {
		if r := w.reading; r != nil {
			if _, err := conn.Exec(
				"UPDATE agents SET last_status = ?1, last_seq = ?2, last_screen_hash = ?3, "+
					"last_change_at = ?4 WHERE id = ?5",
				r.status, r.seq, r.hash, w.lastChangeAt, w.live.ID); err != nil {
				return exit.Database(err)
			}
		}
	}
	return nil
}

func saveSuspects(conn *sql.DB, all []watched) error {
	for _, w := range all {
		if _, err := conn.Exec("UPDATE agents SET suspect = ?1 WHERE id = ?2", w.suspect, w.live.ID); err != nil {
			return exit.Database(err)
		}
	}
	return nil
}

// notice is the message body for the orchestra: every current suspect with
// its evidence, and the agents that stopped being suspects.
func notice(all []watched, before []string, now int64) string {
	var suspects []watched
	var cleared []string
	for _, w := range all {
		if w.suspect {
			suspects = append(suspects, w)
		} else if slices.Contains(before, w.live.Name) {
			cleared = append(cleared, w.live.Name)
		}
	}
	var text strings.Builder
	text.WriteString("fleet watch: the set of suspect agents changed.\n")
	if len(suspects) == 0 {
		text.WriteString("\nNo agent is a suspect now.\n")
	} else {
		fmt.Fprintf(&text, "\nSuspects now (%d):\n", len(suspects))
		for _, w := range suspects {
			fmt.Fprintf(&text, "- %s\n", evidence(w, now))
			if r := w.reading; r != nil && r.tail != "" {
				text.WriteString("  last lines of its screen:\n")
				for _, line := range lines(r.tail) {
					fmt.Fprintf(&text, "    %s\n", line)
				}
			}
		}
	}
	if len(cleared) > 0 {
		fmt.Fprintf(&text, "\nNo longer suspect: %s\n", strings.Join(cleared, ", "))
	}
	return text.String()
}

func evidence(w watched, now int64) string {
	who := fmt.Sprintf("%s (%s, job %s, parent %s)", w.live.Name, w.live.Role, w.live.Job, w.live.Parent)
	switch {
	case w.reading == nil:
		return who + ": gone from herdr"
	case w.lastChangeAt != nil:
		return fmt.Sprintf("%s: no change for %s: status %s, state_change_seq %d, screen hash %s",
			who, Duration(now-*w.lastChangeAt), w.reading.status, w.reading.seq, w.reading.hash)
	default:
		return who
	}
}

// FilterClaudeScreen is Claude Code's visible screen without the parts that
// move while the agent is stuck: the input box and the footer below it
// (status line, usage countdowns, permission mode), and just above the box
// the spinner line with its timer, the tip under it and right-aligned
// notices. What remains is the transcript.
func FilterClaudeScreen(screen string) string {
	all := lines(screen)
	boxTop := len(all)
	for i := len(all) - 2; i >= 0; i-- {
		if isRule(all[i]) && strings.HasPrefix(strings.TrimLeftFunc(all[i+1], unicode.IsSpace), "❯") {
			boxTop = i
			break
		}
	}
	kept := all[:boxTop]
	for len(kept) > 0 && isStatusLine(kept[len(kept)-1]) {
		kept = kept[:len(kept)-1]
	}
	return strings.Join(kept, "\n")
}

func isRule(line string) bool {
	line = strings.TrimSpace(line)
	return line != "" && strings.Trim(line, "─") == ""
}

// isStatusLine is a line of the status area above the input box: blank,
// the spinner (one of Claude's spinner glyphs and a space), a `⎿` line
// hanging under it, or a notice pushed to the right edge.
func isStatusLine(line string) bool {
	trimmed := strings.TrimLeftFunc(line, unicode.IsSpace)
	spinner := false
	for _, glyph := range []string{"·", "✢", "✳", "✶", "✻", "✽", "*"} {
		if strings.HasPrefix(trimmed, glyph+" ") {
			spinner = true
		}
	}
	return trimmed == "" || spinner || strings.HasPrefix(trimmed, "⎿") || len(line)-len(trimmed) >= 20
}

// Hash is FNV-1a, 64 bit, the function the earlier implementation used:
// stable across releases, so a new binary does not count as a screen
// change.
func Hash(text string) string {
	h := fnv.New64a()
	h.Write([]byte(text))
	return fmt.Sprintf("%016x", h.Sum64())
}

// tail is the last `n` non-blank lines.
func tail(text string, n int) string {
	var kept []string
	for _, line := range lines(text) {
		if strings.TrimSpace(line) != "" {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept[max(len(kept)-n, 0):], "\n")
}

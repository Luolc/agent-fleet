// `fleet watch`: the timer's check of a scope's jobs, threads and
// questions (docs/design.md). The job rules are here, the thread rules in
// watchthread.go.

package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
	"github.com/Luolc/agent-fleet/internal/identity"
)

// nowEnv sets the clock of a run, in seconds since the epoch, so the
// end-to-end test can step through hours and days without waiting.
const nowEnv = "FLEET_WATCH_NOW"

// watchSender is the header name of what watch sends to agents.
const watchSender = "watch"

// runBudget is how long a run may start new actions; what is left waits
// for the next run, so a run ends within a minute.
const runBudget = 45 * time.Second

// WatchAbout and WatchLongAbout are the help texts of `watch`.
const (
	WatchAbout     = "Timer check of a scope: stuck workers, quiet threads, idle sessions, unanswered questions"
	WatchLongAbout = "Timer check of a scope: stuck workers, quiet threads, idle sessions, unanswered questions.\n\n" +
		"Meant for a timer every 5 minutes, with --scope; one run at a time per scope (a lock " +
		"file next to the ledger; a second run is refused). Each run first reads everything it " +
		"needs: the ledger, `herdr agent list`, the visible screen of every live lead and worker, " +
		"the config files, and, with a fednet socket configured, the newest message of every " +
		"thread that has a live thread agent, an open job reporting to it or a pending question " +
		"(`fednet client read-thread`). A failed read is exit 5 with nothing done, except a " +
		"thread fednet answers it cannot give (its exit 1 or 2): that thread's rules are skipped, " +
		"saying so, and the exit is 5. Then it acts; " +
		"whatever it sends an agent goes through the same path as `fleet send`, headed `[FROM: " +
		"watch]`, naming the rule and its evidence. It never calls a model and never sends keys.\n\n" +
		"Jobs: a lead's or worker's status, `state_change_seq` and screen hash (the screen with the " +
		"spinner line, the input box and the footer stripped) are recorded; an agent is a suspect " +
		"when all three have been unchanged for `worker_stale` (10m), or when it is gone from " +
		"herdr; an agent blocked at a prompt is not. Each lead is told when the set of suspects " +
		"among its workers changes; a lead blocked or gone is told again next run. A lead's own " +
		"state goes into the quiet-thread notice below.\n\n" +
		"Threads (only with a fednet socket): a live thread agent gone from herdr has its session " +
		"ended as abnormal (its ticket released without --done, a closing line saying the session " +
		"broke off). A thread with no message for `thread_idle` (72h) has its live session " +
		"reclaimed: the agent is asked to write its summary and end; 10 minutes later fleet ends it " +
		"(the ticket gets a comment saying no summary was left), closes its tab and posts the " +
		"closing line. A thread with no pending question and no message for `thread_quiet` (30m) " +
		"is asked about once per quiet spell: with open jobs reporting to it, its thread agent " +
		"(started for it when none is live) gets each job's state and is asked for progress; " +
		"with none, a live thread agent is asked why it has not ended.\n\n" +
		"Questions: the people are reminded of a thread's pending questions in one post listing " +
		"them, at each of `reminders` (30m, 3h, 24h) after the oldest. A thread agent's question " +
		"pending for `thread_question` (72h) is closed and its session reclaimed. A lead's question " +
		"pending for `lead_question` (72h): the lead is told its job ends in 30 minutes; if the job " +
		"is still open then, watch reclaims it as `job end --force` does and posts the Linear steps " +
		"not done to the thread. The pending questions of a job that is not open are closed.\n\n" +
		"The limits are durations such as `10m` or `72h` under `watch` in the scope's settings; " +
		"`worker_stale` and `lead_question` of a single-repo job come from its repo's " +
		"`.fleet/config.json`. FLEET_WATCH_NOW (seconds since the epoch) sets the run's clock.\n\n" +
		"Exit: 0 when every rule that fired was carried out; 1 when the scope is not a scope name, " +
		"a config file or FLEET_WATCH_NOW is invalid, or another run holds the lock; 2, 3 or 4 when " +
		"a delivery gave no clear signal, found the agent blocked, or did not find it, as for " +
		"`send` (the next run tries again where that matters); 5 when a read fails (nothing " +
		"done), an action fails (the rest still done), the run is out of time, or the ledger does " +
		"not exist."
)

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

// watchRun is one run: what it acts with, the clock, and the exit code so
// far (the first that was not ok).
type watchRun struct {
	h     *herdr.Herdr
	conn  *sql.DB
	cfg   *config.Scope
	scope string
	now   int64
	start time.Time
	code  exit.Code
	late  bool
}

// failed records a failed action: printed, its code kept, the run goes on.
func (r *watchRun) failed(err error) {
	fmt.Fprintf(os.Stderr, "fleet watch: %v\n", err)
	code := exit.Environment
	var failure *exit.Failure
	if errors.As(err, &failure) {
		code = failure.Code
	}
	r.got(code)
}

// got keeps the first code that is not ok.
func (r *watchRun) got(code exit.Code) {
	if r.code == exit.Ok {
		r.code = code
	}
}

// outOfTime is whether the run may start no more actions; said once.
func (r *watchRun) outOfTime() bool {
	if !r.late && time.Since(r.start) > runBudget {
		r.late = true
		fmt.Fprintf(os.Stderr, "fleet watch: out of time after %v; the rest waits for the next run\n", runBudget)
		r.got(exit.Environment)
	}
	return r.late
}

// Watch runs `watch`.
func Watch(h *herdr.Herdr) (exit.Code, error) {
	start := time.Now()
	scope, err := identity.Scope()
	if err != nil {
		return 0, err
	}
	now, err := watchNow()
	if err != nil {
		return 0, err
	}
	cfg, err := config.LoadScope(scope)
	if err != nil {
		return 0, err
	}
	conn, err := OpenLedger()
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	unlock, err := lockWatch(scope)
	if err != nil {
		return 0, err
	}
	defer unlock()
	// Everything is read before anything is done.
	inHerdr, err := HerdrAgents(h)
	if err != nil {
		return 0, err
	}
	limits, err := jobLimits(conn, cfg)
	if err != nil {
		return 0, err
	}
	all, err := watchRead(h, conn, inHerdr, now, cfg.Watch, limits)
	if err != nil {
		return 0, err
	}
	threads, on, err := readThreads(conn, cfg, inHerdr, limits)
	if err != nil {
		return 0, err
	}
	r := &watchRun{h: h, conn: conn, cfg: cfg, scope: scope, now: now, start: start}
	if err := r.jobs(all); err != nil {
		return 0, err
	}
	if !on {
		fmt.Fprintf(os.Stderr, "note: fednet.socket is not configured for scope %s; the thread and question rules are off\n", scope)
		return r.code, nil
	}
	if err := r.threads(threads, all); err != nil {
		return 0, err
	}
	return r.code, nil
}

// jobs runs the job rules: the readings and verdicts saved, every suspect
// printed, and each lead told when its suspect workers change.
func (r *watchRun) jobs(all []watched) error {
	if err := saveReadings(r.conn, all); err != nil {
		return err
	}
	suspects := 0
	for _, w := range all {
		if w.suspect {
			suspects++
			fmt.Fprintf(os.Stdout, "suspect: %s\n", evidence(w, r.now))
		}
	}
	// A lead's own verdict goes only into the quiet-thread notice, so it
	// is saved as is.
	var leads []watched
	for _, w := range all {
		if w.live.Role == "lead" {
			leads = append(leads, w)
		}
	}
	if err := saveSuspects(r.conn, leads); err != nil {
		return err
	}
	told, err := r.tellLeads(all)
	if err != nil {
		return err
	}
	if !told {
		fmt.Fprintf(os.Stdout, "suspect set unchanged for every lead (%d suspect)\n", suspects)
	}
	return nil
}

// watchNow is the run's clock: FLEET_WATCH_NOW when set, else now.
func watchNow() (int64, error) {
	value := os.Getenv(nowEnv)
	if value == "" {
		return db.Now(), nil
	}
	now, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, exit.Refusedf("%s=%q is not a number of seconds since the epoch", nowEnv, value)
	}
	return now, nil
}

// lockWatch takes the scope's watch lock, `<scope>.watch.lock` next to
// the ledger, or refuses when another run holds it. The lock goes with
// the process, so a killed run leaves none behind.
func lockWatch(scope string) (func(), error) {
	ledger, err := db.Path(scope)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(filepath.Dir(ledger), scope+".watch.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, exit.IO(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, exit.Refusedf("another fleet watch is running for scope %s (%s)", scope, path)
		}
		return nil, exit.IO(err)
	}
	return func() { _ = f.Close() }, nil
}

// jobLimits are the watch limits of each open job: its repo's config for
// a single-repo job, the scope's settings for a cross-repo one.
func jobLimits(conn *sql.DB, cfg *config.Scope) (map[string]config.Watch, error) {
	jobs, err := openJobs(conn)
	if err != nil {
		return nil, err
	}
	limits := map[string]config.Watch{}
	for _, j := range jobs {
		limits[j.Job] = cfg.Watch
		if j.Repo != "" {
			c, err := jobConfig(j.Repo, j.LeadCwd)
			if err != nil {
				return nil, err
			}
			limits[j.Job] = c.Watch
		}
	}
	return limits, nil
}

// workersByLead groups the watched workers by their lead, in lead name
// order.
func workersByLead(all []watched) (leads []string, byLead map[string][]watched) {
	byLead = map[string][]watched{}
	for _, w := range all {
		if w.live.Role != "worker" {
			continue
		}
		if _, seen := byLead[w.live.Parent]; !seen {
			leads = append(leads, w.live.Parent)
		}
		byLead[w.live.Parent] = append(byLead[w.live.Parent], w)
	}
	slices.Sort(leads)
	return leads, byLead
}

// tellLeads tells each lead whose set of suspect workers changed, and saves
// the verdicts of the workers whose lead was told (or may have been: exit
// 2); told is whether any lead was told.
func (r *watchRun) tellLeads(all []watched) (bool, error) {
	leads, byLead := workersByLead(all)
	told := false
	for _, lead := range leads {
		if r.outOfTime() {
			break
		}
		changed, err := r.tellLead(lead, byLead[lead])
		if err != nil {
			return false, err
		}
		told = told || changed
	}
	return told, nil
}

// tellLead tells `lead` when the set of suspects among `workers` changed
// since the last run; changed is whether it did. Without a clear signal
// (exit 2) the notice may have arrived; it is not resent blindly. A
// blocked or missing lead is told again next run, so the verdicts are
// saved only on exit 0 or 2.
func (r *watchRun) tellLead(lead string, workers []watched) (bool, error) {
	var before, after []string
	for _, w := range workers {
		if w.live.Suspect {
			before = append(before, w.live.Name)
		}
		if w.suspect {
			after = append(after, w.live.Name)
		}
	}
	before, after = nameSet(before), nameSet(after)
	if slices.Equal(before, after) {
		return false, nil
	}
	text, err := WithHeader(watchSender, notice(workers, before, r.now))
	if err != nil {
		return true, err
	}
	got, err := Deliver(r.h, lead, text)
	if err != nil {
		r.failed(err)
		return true, nil
	}
	r.got(got)
	if got == exit.Ok || got == exit.Unknown {
		if err := saveSuspects(r.conn, workers); err != nil {
			return true, err
		}
	}
	return true, nil
}

// watchRead reads every live lead and worker from herdr and compares it with
// the ledger, with the stale limit of its job (the scope's for a job the
// ledger has no open row for).
func watchRead(h *herdr.Herdr, conn *sql.DB, inHerdr map[string]InHerdr, now int64, scope config.Watch,
	limits map[string]config.Watch) ([]watched, error) {
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
		limit, ok := limits[live.Job]
		if !ok {
			limit = scope
		}
		all = append(all, observe(live, r, now, secs(limit.WorkerStale)))
	}
	return all, nil
}

// nameSet is a sorted slice without duplicates.
func nameSet(names []string) []string {
	slices.Sort(names)
	return slices.Compact(names)
}

// observe compares this run's reading with the last one in the ledger. Any
// difference in status, seq or screen hash restarts the clock. An agent
// blocked at a prompt is never a suspect: getting it past the prompt is
// another matter than finding it stuck.
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
		suspect:      now-lastChangeAt >= stale && r.status != "blocked",
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

// notice is the message body for a lead: every current suspect among
// `workers` with its evidence, and the workers that stopped being suspects.
func notice(workers []watched, before []string, now int64) string {
	var suspects []watched
	var cleared []string
	for _, w := range workers {
		if w.suspect {
			suspects = append(suspects, w)
		} else if slices.Contains(before, w.live.Name) {
			cleared = append(cleared, w.live.Name)
		}
	}
	var text strings.Builder
	text.WriteString("fleet watch, rule `suspect workers`: the set of suspect agents among your workers changed. " +
		"A suspect has had the same herdr status, state_change_seq and screen for the job's limit, or is gone " +
		"from herdr. Look at each: a false alarm needs nothing; otherwise send it a message, start another " +
		"worker to take over, or ask the people with `fleet ask-human`.\n")
	if len(suspects) == 0 {
		text.WriteString("\nNo worker of yours is a suspect now.\n")
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

// Hash is FNV-1a, 64 bit: stable across releases, so a new binary does not
// count as a screen change.
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

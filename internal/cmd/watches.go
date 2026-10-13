// The configured watches of `fleet watch` (docs/design.md): when each
// runs, its prompt, and the command sources with what `tell` does about
// their subjects. The built-in sources' rules are in watch.go and
// watchthread.go.

package cmd

import (
	"bufio"
	"bytes"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
)

// The built-in prompts of the default watches.
var (
	//go:embed watch_stale_workers.md
	staleWorkersPrompt string
	//go:embed watch_quiet_threads.md
	quietThreadsPrompt string
)

// watchSlots are the slots of a watch's prompt; a `worker` watch's also
// has `question_file`.
var watchSlots = []string{"key", "text"}

// slotsOf are the slots of the watch `e`'s prompt.
func slotsOf(e config.Entry) []string {
	if e.Do == "worker" {
		return append(slices.Clone(watchSlots), "question_file")
	}
	return watchSlots
}

// sourceTimeout bounds one run of a command source, sourceLimit what it
// may print.
const (
	sourceTimeout = 30 * time.Second
	sourceLimit   = 4 << 20
)

// builtinWatch is the entry of the default that has the built-in source.
func (r *watchRun) builtinWatch(source string) config.Entry {
	for _, e := range r.cfg.Watches {
		if e.Source == source {
			return e
		}
	}
	panic("no default watch with the source " + source)
}

// due is whether the watch runs this run: it is not off, and `every` has
// passed since its last run (step `ran` of `watch:<name>`, the run's
// clock).
func (r *watchRun) due(e config.Entry) (bool, error) {
	if e.Off {
		return false, nil
	}
	if e.Every == 0 {
		return true, nil
	}
	var last int64
	err := r.conn.QueryRow("SELECT done_at FROM steps WHERE key = ?1 AND step = 'ran'", "watch:"+e.Name).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, exit.Database(err)
	}
	return r.now-last >= secs(e.Every), nil
}

// ran records this run of the watch, unless the run is out of time: then
// the watch may have been cut short, and the next run takes it up again.
func (r *watchRun) ran(e config.Entry) error {
	if e.Every == 0 || r.late {
		return nil
	}
	_, err := r.conn.Exec("INSERT INTO steps (key, step, done_at) VALUES (?1, 'ran', ?2) "+
		"ON CONFLICT (key, step) DO UPDATE SET done_at = excluded.done_at", "watch:"+e.Name, r.now)
	return dbErr(err)
}

// builtinWatchPrompt is the built-in prompt of the default with the
// built-in source `source`; "" for a command source.
func builtinWatchPrompt(source string) string {
	return map[string]string{config.StaleAgents: staleWorkersPrompt, config.QuietThreads: quietThreadsPrompt,
		config.BlockedScreens: unblockPrompt + "\n" + screenGuidance}[source]
}

// watchPrompt is the prompt of the watch: the file its entry names, else
// a default's built-in text; nil for a command source without one.
func watchPrompt(sc *config.Scope, e config.Entry) (*promptText, error) {
	key := "watches." + e.Name + ".prompt"
	if e.Prompt == "" {
		text := builtinWatchPrompt(e.Source)
		if text == "" {
			return nil, nil
		}
		return &promptText{Name: key, Parts: []promptPart{{Source: "built-in", Text: text}}}, nil
	}
	part, shown, err := readPart(sc, key, e.Prompt)
	if err != nil {
		return nil, err
	}
	if err := checkSlots(part, shown, "a watch's prompt", slotsOf(e)); err != nil {
		return nil, err
	}
	return &promptText{Name: key, Parts: []promptPart{*part}}, nil
}

// watchMessage is what a watch tells about one subject: the text, then
// the prompt with its slots filled.
func watchMessage(prompt *promptText, key, text string) string {
	if prompt == nil {
		return text
	}
	return text + "\n" + prompt.fill(map[string]string{"key": key, "text": text})
}

// subject is one line of a command source's output, or the one subject
// of a timer.
type subject struct {
	Key    string `json:"key"`
	Text   string `json:"text"`
	Job    string `json:"job"`
	Thread string `json:"thread"`
	Agent  string `json:"agent"`
}

// sourceJob is one job as a command source reads it.
type sourceJob struct {
	Job         string `json:"job"`
	ParentIssue string `json:"parent_issue"`
	Repo        string `json:"repo"`
	HomeThread  string `json:"home_thread"`
	State       string `json:"state"`
	Outcome     string `json:"outcome"`
	StartedAt   int64  `json:"started_at"`
	EndedAt     *int64 `json:"ended_at"`
}

// sourceInput is what a command source reads on its standard input: the
// scope, the run's clock and the scope's jobs, open and ended, in start
// order.
func (r *watchRun) sourceInput() ([]byte, error) {
	jobs := []sourceJob{}
	err := eachRow(r.conn, "SELECT job, parent_issue, repo, home_thread, state, outcome, started_at, ended_at FROM jobs ORDER BY id",
		func(rows *sql.Rows) error {
			var j sourceJob
			var ended sql.NullInt64
			if err := rows.Scan(&j.Job, &j.ParentIssue, &j.Repo, &j.HomeThread, &j.State, &j.Outcome, &j.StartedAt, &ended); err != nil {
				return err
			}
			if ended.Valid {
				j.EndedAt = &ended.Int64
			}
			jobs = append(jobs, j)
			return nil
		})
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(map[string]any{"version": 1, "scope": r.scope, "now": r.now, "jobs": jobs})
	if err != nil {
		return nil, exit.Environmentf("cannot encode a watch source's input: %v", err)
	}
	return append(data, '\n'), nil
}

// runSource runs the command source of `e` through herdr's runner and
// reads its subjects: one JSON object per line, each with a key of its
// own and a text, and for `tell` the agent, job or thread it concerns. A
// `worker` watch without a source is a timer: its one subject is the
// watch itself, its text new each run, so it is due whenever the watch
// runs and `again` allows.
// What the source printed on stderr is shown only in its first line, the
// values of the variables it was given taken out before the line is cut.
func runSource(e config.Entry, input []byte, now int64) ([]subject, error) {
	if e.Command == nil {
		return []subject{{Key: e.Name, Text: fmt.Sprintf("The timer of the watch `%s`, run at %s.", e.Name,
			time.Unix(now, 0).UTC().Format(time.RFC3339))}}, nil
	}
	out, errOut, err := herdr.ExecInput("its source", e.Env, sourceTimeout, sourceLimit, input, e.Command[0],
		e.Command[1:]...)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil, exit.Environmentf("its source exited %d: %s", exitErr.ExitCode(),
			firstLine(redactEnv(string(errOut), e.Env)))
	}
	if err != nil {
		return nil, err
	}
	var subjects []subject
	seen := map[string]bool{}
	lines := bufio.NewScanner(bytes.NewReader(out))
	lines.Buffer(nil, 1<<20)
	for n := 1; lines.Scan(); n++ {
		line := bytes.TrimSpace(lines.Bytes())
		if len(line) == 0 {
			continue
		}
		var s subject
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		switch err := dec.Decode(&s); {
		case err != nil:
			return nil, exit.Environmentf("its source's line %d is not a subject: %v", n, err)
		case s.Key == "" || strings.TrimSpace(s.Text) == "":
			return nil, exit.Environmentf("its source's line %d has no key or no text", n)
		case seen[s.Key]:
			return nil, exit.Environmentf("its source gives the key %q twice", s.Key)
		case s.Agent == "" && s.Job == "" && s.Thread == "" && e.Do == "tell":
			return nil, exit.Environmentf("its source's line %d names no agent, job or thread to tell", n)
		}
		seen[s.Key] = true
		subjects = append(subjects, s)
	}
	if err := lines.Err(); err != nil {
		return nil, exit.Environmentf("cannot read its source's output: %v", err)
	}
	return subjects, nil
}

// firstLine is the first non-blank line of `text`, at most 200 bytes.
func firstLine(text string) string {
	for _, line := range lines(text) {
		if line = strings.TrimSpace(line); line != "" {
			return line[:min(len(line), 200)]
		}
	}
	return "(nothing on stderr)"
}

// redactEnv is `text` without the values of the variables `names`.
func redactEnv(text string, names []string) string {
	for _, name := range names {
		if value := os.Getenv(name); len(value) >= 4 {
			text = strings.ReplaceAll(text, value, "[redacted "+name+"]")
		}
	}
	return text
}

// watchSubject is a subject's row in `watch_subjects`.
type watchSubject struct {
	digest    string
	lastActed sql.NullInt64
	acts      int
	askedAt   sql.NullInt64
}

// commandWatches runs the command watches in name order, while the run
// has time.
func (r *watchRun) commandWatches() error {
	for _, e := range r.cfg.Watches {
		if e.Source != "" {
			continue
		}
		if r.outOfTime() {
			break
		}
		if err := r.commandWatch(e); err != nil {
			return err
		}
	}
	return nil
}

// commandWatch runs one command watch when it is due: its source, then
// its act (`tell`, or `worker`) on each subject due, at most `per_run`. A
// source that fails, or a broken prompt, skips the watch for this run.
func (r *watchRun) commandWatch(e config.Entry) error {
	if due, err := r.due(e); err != nil || !due {
		return err
	}
	input, err := r.sourceInput()
	if err != nil {
		return err
	}
	subjects, err := runSource(e, input, r.now)
	if err != nil {
		r.failed(fmt.Errorf("watch %s skipped: %w", e.Name, err))
		return nil
	}
	prompt, err := watchPrompt(r.cfg, e)
	var role *promptText
	if err == nil && e.Do == "worker" {
		role, err = loadPrompt(r.cfg, "worker")
	}
	if err != nil {
		thread := ""
		if len(subjects) > 0 {
			var threadErr error
			if thread, threadErr = r.subjectThread(subjects[0]); threadErr != nil {
				return errors.Join(err, threadErr)
			}
		}
		r.failed(r.promptBroken(err, thread, fmt.Sprintf("%s about the subjects of watch %s",
			map[string]string{"tell": "tell", "worker": "start a worker"}[e.Do], e.Name)))
		return nil
	}
	rows, err := r.syncSubjects(e.Name, subjects)
	if err != nil {
		return err
	}
	acts := r.actOn(e, role, prompt, subjects, rows)
	fmt.Fprintf(os.Stdout, "watch %s: %d subject(s), acted on %d\n", e.Name, len(subjects), acts)
	return r.ran(e)
}

// actOn acts on the due subjects, in output order, at most `per_run`,
// while the run has time: a tell or a worker's start (`role` its worker
// text), or at `max` the question to the people. It returns how many it
// acted on.
func (r *watchRun) actOn(e config.Entry, role, prompt *promptText, subjects []subject, rows map[string]watchSubject) int {
	acts := 0
	for _, s := range subjects {
		if acts >= e.PerRun || r.outOfTime() {
			break
		}
		row := rows[s.Key]
		due := row.due(e, r.now, s.Text)
		if e.Do == "worker" {
			var err error
			if due, err = r.workers.dueFor(e, s, row, due); err != nil {
				r.failed(fmt.Errorf("watch %s, subject %s: %w", e.Name, s.Key, err))
				continue
			}
		}
		if !due {
			continue
		}
		acts++
		var err error
		switch {
		case e.Max > 0 && row.acts >= e.Max:
			err = r.askAboutSubject(e, s)
		case e.Do == "worker":
			err = r.workers.startFor(e, role, prompt, s)
		default:
			err = r.tellSubject(e, prompt, s)
		}
		if err != nil {
			r.failed(fmt.Errorf("watch %s, subject %s: %w", e.Name, s.Key, err))
		}
	}
	return acts
}

// due is whether the subject gets an act at `now`: when first seen, then
// with `again` once that long has passed since its last act, without it
// when its text changed; never once the people were asked about it.
func (row watchSubject) due(e config.Entry, now int64, text string) bool {
	if row.askedAt.Valid {
		return false
	}
	if row.acts == 0 {
		return true
	}
	if e.Again > 0 {
		return now-row.lastActed.Int64 >= secs(e.Again)
	}
	return row.digest != Hash(text)
}

// syncSubjects records the subjects of this run of the watch `name`: a
// new one first seen now, one its source no longer gives dropped. It
// returns each subject's row by key.
func (r *watchRun) syncSubjects(name string, subjects []subject) (map[string]watchSubject, error) {
	tx, err := r.conn.Begin()
	if err != nil {
		return nil, exit.Database(err)
	}
	defer func() { _ = tx.Rollback() }()
	keys := make([]any, 0, len(subjects)+1)
	keys = append(keys, name)
	for _, s := range subjects {
		keys = append(keys, s.Key)
		if _, err := tx.Exec("INSERT OR IGNORE INTO watch_subjects (watch, key, first_seen) VALUES (?1, ?2, ?3)",
			name, s.Key, r.now); err != nil {
			return nil, exit.Database(err)
		}
	}
	drop := "DELETE FROM watch_subjects WHERE watch = ?"
	if len(subjects) > 0 {
		drop += " AND key NOT IN (?" + strings.Repeat(", ?", len(subjects)-1) + ")"
	}
	if _, err := tx.Exec(drop, keys...); err != nil {
		return nil, exit.Database(err)
	}
	rows := map[string]watchSubject{}
	q, err := tx.Query("SELECT key, digest, last_acted, acts, asked_at FROM watch_subjects WHERE watch = ?1", name)
	if err != nil {
		return nil, exit.Database(err)
	}
	defer q.Close()
	for q.Next() {
		var key string
		var row watchSubject
		if err := q.Scan(&key, &row.digest, &row.lastActed, &row.acts, &row.askedAt); err != nil {
			return nil, exit.Database(err)
		}
		rows[key] = row
	}
	if err := q.Err(); err != nil {
		return nil, exit.Database(err)
	}
	return rows, dbErr(tx.Commit())
}

// tellSubject sends the subject's text and the prompt to the agent it
// concerns: the agent named, else the live lead of the job, else the
// thread's agent, started for it when none is live. Only a tell that
// landed (exit 0, or 2: it may have) counts as an act; any other is tried
// again next run.
func (r *watchRun) tellSubject(e config.Entry, prompt *promptText, s subject) error {
	text := fmt.Sprintf("fleet watch, watch `%s`, subject `%s`:\n\n%s\n", e.Name, s.Key, strings.TrimRight(s.Text, "\n"))
	body := watchMessage(prompt, s.Key, text)
	to := s.Agent
	if to == "" && s.Job != "" {
		lead, err := liveLead(r.conn, s.Job)
		if err != nil {
			return err
		}
		if lead == "" {
			return exit.New(exit.NotFound, "job "+s.Job+" has no live lead to tell; the next run tries again")
		}
		to = lead
	}
	var code exit.Code
	var err error
	if to != "" {
		var message string
		if message, err = WithHeader(watchSender, body); err != nil {
			return err
		}
		code, err = Deliver(r.h, to, message)
	} else {
		to = "the agent of thread " + s.Thread
		code, err = toThread(r.h, r.conn, r.scope, r.cfg, inboundMessage{Thread: s.Thread, Text: body, Watch: true})
	}
	if err != nil {
		return err
	}
	r.got(code)
	if code != exit.Ok && code != exit.Unknown {
		return nil
	}
	if err := r.acted(e, s); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "watch %s: told %s about %s\n", e.Name, to, s.Key)
	return nil
}

// acted records an act on the subject `s`: its text's digest, the time
// and the count.
func (r *watchRun) acted(e config.Entry, s subject) error {
	_, err := r.conn.Exec("UPDATE watch_subjects SET digest = ?1, last_acted = ?2, acts = acts + 1 WHERE watch = ?3 AND key = ?4",
		Hash(s.Text), r.now, e.Name, s.Key)
	return dbErr(err)
}

// liveLead is the name of the job's live lead, "" for none.
func liveLead(conn *sql.DB, job string) (string, error) {
	var name string
	err := conn.QueryRow("SELECT name FROM agents WHERE role = 'lead' AND job = ?1 AND state != 'ended'", job).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return name, dbErr(err)
}

// subjectThread is the thread the people are asked in about a subject:
// its thread, else the home thread of its job (the latest of the name),
// else its agent's (a thread agent's own, or its job's); "" for none.
func (r *watchRun) subjectThread(s subject) (string, error) {
	if s.Thread != "" {
		return s.Thread, nil
	}
	job := s.Job
	if job == "" && s.Agent != "" {
		var thread string
		err := r.conn.QueryRow("SELECT job, thread FROM agents WHERE name = ?1 ORDER BY id DESC LIMIT 1", s.Agent).
			Scan(&job, &thread)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		if err != nil || thread != "" {
			return thread, dbErr(err)
		}
	}
	var thread string
	err := r.conn.QueryRow("SELECT home_thread FROM jobs WHERE job = ?1 ORDER BY id DESC LIMIT 1", job).Scan(&thread)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return thread, dbErr(err)
}

// askAboutSubject asks the people once about a subject due again whose
// acts reached the watch's `max`: posted to its thread as a question pending from
// watch, which is reminded of but never times out, and the thread's agent
// told there is nothing to pass on. With no thread to ask in, the question
// is printed on stderr. Either way the subject gets no more acts while its
// source gives it.
func (r *watchRun) askAboutSubject(e config.Entry, s subject) error {
	question := fmt.Sprintf("fleet watch, watch `%s`: subject `%s` has had the watch's max of %d act(s), and its "+
		"source still gives it:\n\n%s\n\nfleet does nothing more about it while the source gives it; what to do is yours.",
		e.Name, s.Key, e.Max, strings.TrimRight(s.Text, "\n"))
	thread, err := r.subjectThread(s)
	if err != nil {
		return err
	}
	if thread == "" || r.cfg.FednetSocket == "" {
		fmt.Fprintf(os.Stderr, "fleet watch: no thread to ask the people in about subject %s of watch %s; the question: %s\n",
			s.Key, e.Name, question)
	} else {
		if err := r.post(thread, s.Job, watchSender, question); err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "watch %s: asked the people in thread %s about %s\n", e.Name, thread, s.Key)
		notice := fmt.Sprintf("fleet watch asked the people in thread %s, and posted it there:\n\n%s\n\nNothing to pass "+
			"on: the question is reminded of but never times out.\n", thread, question)
		msg := inboundMessage{Thread: thread, Text: notice, Watch: true}
		if code, err := toThread(r.h, r.conn, r.scope, r.cfg, msg); err != nil || code != exit.Ok {
			fmt.Fprintf(os.Stderr, "fleet watch: watch %s: the question is posted, but did not reach the agent of thread "+
				"%s (exit %d): %v\n", e.Name, thread, code, err)
		}
	}
	_, err = r.conn.Exec("UPDATE watch_subjects SET asked_at = ?1 WHERE watch = ?2 AND key = ?3", r.now, e.Name, s.Key)
	return dbErr(err)
}

// dueFor is whether the `worker` watch `e` starts a worker for the subject
// `s` now, `byText` being what its row says: never while its worker is
// live or that worker's question waits for the people; once they
// answered it after the last start, whatever `again` and the text say.
func (w *workers) dueFor(e config.Entry, s subject, row watchSubject, byText bool) (bool, error) {
	name := workerName(e.Name, s.Key)
	if w.busy[name] {
		fmt.Fprintf(os.Stdout, "watch %s: %s is on %s\n", e.Name, name, s.Key)
		return false, nil
	}
	pending, answeredAt, err := pendingFrom(w.conn, []string{name})
	if err != nil || pending {
		return false, err
	}
	if answeredAt == 0 || row.askedAt.Valid {
		return byText, nil
	}
	// Both times are the ledger's clock, not the run's.
	var started int64
	if err := w.conn.QueryRow("SELECT COALESCE(max(started_at), 0) FROM agents WHERE name = ?1", name).Scan(&started); err != nil {
		return false, exit.Database(err)
	}
	return byText || answeredAt >= started, nil
}

// startFor starts the worker of the `worker` watch `e` for the subject
// `s`, with `role` its worker text and `prompt` the watch's; a start
// counts as an act once the worker has its first message.
func (w *workers) startFor(e config.Entry, role, prompt *promptText, s subject) error {
	id := w.workerID(workerName(e.Name, s.Key), "")
	ws := workerSubject{Watch: e.Name, Key: s.Key, Job: s.Job, Thread: s.Thread, Agent: s.Agent}
	body, err := w.workerMessage(role, prompt, id, ws, s.Text, []string{id.Agent})
	if err != nil {
		return err
	}
	if err := w.startWorker(e, id, ws, body); err != nil {
		return err
	}
	if err := w.acted(e, s); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "watch %s: started %s for %s\n", e.Name, id.Agent, s.Key)
	return nil
}

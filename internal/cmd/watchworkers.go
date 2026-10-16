// The workers `fleet watch` starts (docs/design.md): a `do: worker`
// watch's, one per subject at a time, and the `unblock` watch's, one per
// agent stopped at a screen. Each runs in the scope's `threads`
// workspace and its own directory, with no job and the parent `watch`;
// once its turn is over it is closed and the question it wrote is asked.

package cmd

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/identity"
)

const (
	// workerPrefix starts the name of every worker watch starts.
	workerPrefix = "watch-"
	// The prefixes of the agents watch started before they became
	// workers, whose rows and questions an upgrade may leave.
	legacyHelperPrefix  = "unblock-"
	legacyRevisitPrefix = "revisit-"
	// helperSecs is how long a watch worker may work before it is closed
	// as stuck.
	helperSecs int64 = 30 * 60
)

// helperNamed is whether `name` is that of an agent watch starts, or
// started before the upgrade: no job name may start the same way, and
// their questions never time out.
func helperNamed(name string) bool {
	for _, prefix := range []string{workerPrefix, legacyHelperPrefix, legacyRevisitPrefix} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// workerName is the worker of the watch `watch` for the subject `key`:
// the same name for every worker of that subject, so at most one is live.
// A key that is already a name's part keeps its form; another (upper
// case included, so keys that differ in case get workers of their own),
// or a name past herdr's 32 characters, is cut and hashed.
func workerName(watch, key string) string {
	slug := slugOf(key)
	name := workerPrefix + watch + "-" + slug
	if slug == key && len(name) <= 32 {
		return name
	}
	sum := sha256.Sum256([]byte(watch + "\x00" + key))
	return strings.TrimRight(name[:min(len(name), 23)], "-") + "-" + hex.EncodeToString(sum[:4])
}

// workerSubject is what a watch worker was started for. Its row keeps it
// in the `task` column as JSON, so closing the worker knows where to ask.
type workerSubject struct {
	Watch  string `json:"watch"`
	Key    string `json:"key"`
	Job    string `json:"job,omitempty"`
	Thread string `json:"thread,omitempty"`
	Agent  string `json:"agent,omitempty"`
}

// workers is one watch run's look at the agents it starts.
type workers struct {
	*watchRun
	inHerdr map[string]InHerdr
	// dir is where the workers run and write their questions; legacyDir
	// where the helpers of before did.
	dir, legacyDir string
	// busy are the workers still live after closing those that were done.
	busy map[string]bool
}

// newWorkers is the run's look at the agents watch starts: in the scope's
// own directory next to the ledger, `<scope>-watch`.
func (r *watchRun) newWorkers(inHerdr map[string]InHerdr) (*workers, error) {
	ledger, err := db.Path(r.scope)
	if err != nil {
		return nil, err
	}
	base := strings.TrimSuffix(ledger, ".db")
	return &workers{watchRun: r, inHerdr: inHerdr, dir: base + "-watch", legacyDir: base + "-unblock",
		busy: map[string]bool{}}, nil
}

// startedByWatch is whether the live row is that of an agent watch
// started: a worker with the parent `watch`, or a screen helper or revisit
// agent of before the upgrade.
func (r stopRow) startedByWatch() bool {
	return r.legacy() || r.Role == "worker" && r.Parent == watchSender
}

// legacy is whether the row is a screen helper or revisit agent of
// before the upgrade.
func (r stopRow) legacy() bool {
	return r.Role == "unblock" || r.Role == "revisit"
}

// close looks at every live agent watch started: one that is done is
// closed and its question asked; the others are busy. A failure for one
// is recorded on the run and the others are still looked at, while the
// run has time; those it has no time for stay busy.
func (w *workers) close() error {
	rows, err := stopRows(w.conn)
	if err != nil {
		return err
	}
	live := map[string]stopRow{}
	for _, row := range rows {
		live[row.Name] = row
	}
	for _, row := range rows {
		if !row.startedByWatch() {
			continue
		}
		if w.outOfTime() {
			w.busy[row.Name] = true
			continue
		}
		busy, err := w.closeDone(row, func(question string) error { return w.askFor(row, live, question) })
		if err != nil {
			w.failed(fmt.Errorf("closing %s: %w", row.Name, err))
		}
		w.busy[row.Name] = busy || err != nil
	}
	return nil
}

// subjectOf is the subject the worker `row` was started for: its row's
// JSON, or for a helper of before, the agent it helped (its parent) and
// for a revisit agent of before, the home thread of the latest job on its
// issue that has one (a thread, not the ended job, so the question is not
// closed with that job).
func (w *workers) subjectOf(row stopRow) (workerSubject, error) {
	switch row.Role {
	case "unblock":
		return workerSubject{Watch: "unblock", Key: strings.TrimPrefix(row.Name, legacyHelperPrefix), Agent: row.Parent}, nil
	case "revisit":
		var thread string
		err := w.conn.QueryRow("SELECT home_thread FROM jobs WHERE parent_issue = ?1 AND home_thread != '' "+
			"ORDER BY id DESC LIMIT 1", row.ParentIssue).Scan(&thread)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return workerSubject{}, exit.Database(err)
		}
		return workerSubject{Watch: "revisit", Key: row.ParentIssue, Thread: thread}, nil
	}
	var s workerSubject
	if err := json.Unmarshal([]byte(row.Task), &s); err != nil {
		return s, exit.Environmentf("the row of %s does not record its subject: %v", row.Name, err)
	}
	return s, nil
}

// askFor asks the people the question the worker `row` wrote, on a card
// (questionCard): about the stopped agent, for the `unblock` watch's
// workers while that agent is live; else in the subject's thread, or on
// stderr without one.
func (w *workers) askFor(row stopRow, live map[string]stopRow, question string) error {
	s, err := w.subjectOf(row)
	if err != nil {
		return err
	}
	if w.screenWorker(s) {
		stopped, ok := live[s.Agent]
		if !ok {
			return nil
		}
		header := fmt.Sprintf("%s (%s) is stopped at a screen that fleet's guidance does not cover; its helper %s "+
			"asks this.", stopped.Name, stopped.who(), row.Name)
		if issue, _ := helperIssue(w.conn, []string{row.Name}); issue != "" {
			header += "\n\nTicket: " + issue
		}
		card, err := questionCard(question, header)
		if err != nil {
			return err
		}
		if err := w.ask(stopped, row.Name, card); err != nil {
			return err
		}
		w.tell(stopped, "its helper asked the people in the home thread what to press")
		return nil
	}
	header := fmt.Sprintf("%s, the worker of fleet watch `%s` for subject `%s`, asks this.", row.Name, s.Watch, s.Key)
	thread, err := w.subjectThread(subject{Key: s.Key, Job: s.Job, Thread: s.Thread, Agent: s.Agent})
	if err != nil {
		return err
	}
	if thread == "" || w.cfg.FednetSocket == "" {
		fmt.Fprintf(os.Stderr, "fleet watch: no thread to ask the people in for %s; its question: %s\n\n%s\n", row.Name,
			header, question)
		return nil
	}
	card, err := questionCard(question, header)
	if err != nil {
		return err
	}
	link, err := w.askCard(thread, s.Job, row.Name, card)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "watch %s: %s asked the people in thread %s\n", s.Watch, row.Name, thread)
	msg := inboundMessage{Thread: thread, Text: cardText(link, card), Question: row.Name, Worker: true}
	if code, err := toThread(w.h, w.conn, w.scope, w.cfg, msg); err != nil || code != exit.Ok {
		fmt.Fprintf(os.Stderr, "fleet watch: %s: the question is posted, but did not reach the agent of thread %s "+
			"(exit %d): %v\n", row.Name, thread, code, err)
	}
	return nil
}

// screenWorker is whether `s` is a subject of the watch with the source
// blocked-screens: an agent stopped at a screen.
func (w *workers) screenWorker(s workerSubject) bool {
	for _, e := range w.cfg.Watches {
		if e.Name == s.Watch {
			return e.Source == config.BlockedScreens
		}
	}
	return false
}

// closeDone looks at a live agent watch started; busy is whether it is
// still working. One done with its turn (herdr idle or done), stopped at
// a screen itself, working for helperSecs, gone from herdr, or left
// `starting` by a start that failed is closed and its row ended, after
// `ask` is given the question it wrote, if any; a card fednet refused is
// returned once the worker is closed.
func (w *workers) closeDone(row stopRow, ask func(question string) error) (busy bool, err error) {
	agent, present := w.inHerdr[row.Name]
	if present && row.State == "active" && agent.Status != "idle" && agent.Status != "done" &&
		agent.Status != "blocked" && w.now-row.StartedAt < helperSecs {
		return true, nil
	}
	file := w.questionFile(row.Name)
	if row.legacy() {
		file = filepath.Join(w.legacyDir, row.Name+"-question.md")
	}
	// A card fednet refused is refused again: the worker is closed all
	// the same, and the error is the run's.
	var refused error
	if question, err := os.ReadFile(file); err == nil && strings.TrimSpace(string(question)) != "" {
		if err := ask(strings.TrimSpace(string(question))); errors.Is(err, errCardRefused) {
			refused = err
		} else if err != nil {
			return false, err
		}
	}
	if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, exit.IO(err)
	}
	if row.Pane != "" {
		if err := closeOwnTab(w.h, w.conn, row.Name); err != nil {
			return false, err
		}
	}
	if err := endRow(w.conn, row.Name); err != nil {
		return false, err
	}
	fmt.Fprintf(os.Stdout, "watch: closed %s (herdr status %q)\n", row.Name, agent.Status)
	return false, refused
}

// questionFile is where the worker `name` writes its question.
func (w *workers) questionFile(name string) string {
	return filepath.Join(w.dir, name+"-question.md")
}

// pendingFrom is whether a question from one of `askers` waits for the
// people; answeredAt is when the latest of them was answered (0: none).
func pendingFrom(conn *sql.DB, askers []string) (pending bool, answeredAt int64, err error) {
	query := "SELECT state, COALESCE(answered_at, 0) FROM questions WHERE asked_by IN (?" +
		strings.Repeat(", ?", len(askers)-1) + ")"
	err = eachRow(conn, query, func(rows *sql.Rows) error {
		var state string
		var at int64
		if err := rows.Scan(&state, &at); err != nil {
			return err
		}
		pending = pending || state == "pending"
		answeredAt = max(answeredAt, at)
		return nil
	}, anys(askers)...)
	return pending, answeredAt, err
}

// anys is `values` as query arguments.
func anys(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// workerPrompt is the worker text a watch worker starts with: the
// built-in one (or its replacement), `common` and the additions, as for a
// lead's worker.
func (w *workers) workerPrompt() (*promptText, error) {
	return loadPrompt(w.cfg, "worker")
}

// workerSlots are the values of the worker text's slots for the watch
// worker `id`, which has no job and no lead and runs in `dir`; `issue` is
// its ticket, "" for none.
func (w *workers) workerSlots(id *identity.Identity, issue string) map[string]string {
	note := " (empty: no ticket)"
	if issue != "" {
		note = " (the ticket your task names)"
	}
	dir := w.cfg.Tilde(w.dir)
	slots := map[string]string{"job": "", "scope": id.Scope, "agent": id.Agent, "parent": watchSender,
		"lead": watchSender, "cwd": dir, "one_task": fmt.Sprintf("one task for fleet's `watch` in scope %s, with no job and no lead", id.Scope),
		"repo_line": "fleet keeps this directory for the workers its `watch` starts; you work in no repository.",
		"issue":     issue, "issue_note": note, "linear": "", "cap": "", "scratch": dir,
		"agents_md": "Your task names what it concerns; its instructions come from the scope's config."}
	maps.Copy(slots, modelValues(w.cfg))
	return slots
}

// workerMessage is a watch worker's first message: the worker text, the
// questions earlier workers of the subject asked with the latest message
// a person posted in the thread of the last (`## Earlier`), and the task:
// the subject, its question file and the watch's prompt.
func (w *workers) workerMessage(role, prompt *promptText, id *identity.Identity, s workerSubject, text string,
	askers []string) (string, error) {
	earlier, err := earlierSection(w.conn, "Earlier", askers)
	if err != nil {
		return "", err
	}
	question := w.questionFile(id.Agent)
	task := fmt.Sprintf("fleet watch, watch `%s`, subject `%s`; your question file is %s:\n\n%s\n", s.Watch, s.Key,
		question, strings.TrimRight(text, "\n"))
	if prompt != nil {
		task += "\n" + prompt.fill(map[string]string{"key": s.Key, "text": text, "question_file": question})
	}
	return role.fill(w.workerSlots(id, id.Issue)) + earlier + taskSection("", task), nil
}

// workerID is the identity of the watch worker `name`, with the ticket
// `issue` ("" for none): no job, the parent `watch`, the workers'
// directory as its `FLEET_SCRATCH`.
func (w *workers) workerID(name, issue string) *identity.Identity {
	return &identity.Identity{Agent: name, Role: identity.Worker, Parent: watchSender, Scope: w.scope, Issue: issue,
		Scratch: w.dir}
}

// startWorker starts the watch worker `id` of the watch `e` for the
// subject `s`, with the first message `body`: in a new tab of the scope's
// `threads` workspace, in the workers' directory, its first message
// headed `[FROM: watch]`. A failed start leaves what the next run closes.
func (w *workers) startWorker(e config.Entry, id *identity.Identity, s workerSubject, body string) error {
	name := id.Agent
	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		return exit.IO(err)
	}
	record, err := json.Marshal(s)
	if err != nil {
		return exit.Environmentf("cannot encode the subject of %s: %v", name, err)
	}
	if err := reserve(w.conn, func(q querier) error { return liveNameTaken(q, name) },
		func(q querier) error { return insertStarting(q, id, w.dir, string(record), "", false) }); err != nil {
		return err
	}
	created := []string{fmt.Sprintf("ledger row %s (state starting)", name)}
	failed := func(err error, code exit.Code) error {
		code, _ = startFailed(name, err, code, created, "")
		return exit.New(code, fmt.Sprintf("the start of %s did not complete; the next run closes what it left", name))
	}
	home, err := Home()
	if err != nil {
		return failed(err, 0)
	}
	workspace, err := threadsWorkspaceID(w.h, w.scope, home)
	if err != nil {
		return failed(err, 0)
	}
	place, err := CreateTab(w.h, workspace, name, w.dir, id)
	if err != nil {
		return failed(err, 0)
	}
	created = append(created, fmt.Sprintf("tab %s (%s)", name, place.TabID))
	code, err := startAndDeliver(w.h, w.conn, id, place, w.dir, orDefault(nil, e.Model), orDefault(nil, e.Effort),
		watchSender, body, &created)
	if err != nil || code != exit.Ok {
		return failed(err, code)
	}
	return nil
}

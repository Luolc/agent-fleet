// The watch `model-changes` of `fleet watch` (docs/design.md): the model
// events in each live agent's Claude Code transcript, acted on
// (watchtakeover.go) and reported to the agent's parent and its thread.
// fleet never switches a session's model and never types into a session
// but its own messages.

package cmd

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/fednet"
	"github.com/Luolc/agent-fleet/internal/herdr"
)

// The kinds of model event, named as the transcript names them.
const (
	eventFallback   = "model_refusal_fallback"
	eventNoFallback = "model_refusal_no_fallback"
	eventRefusal    = "refusal"
	eventChange     = "model_change"
)

// readCheck is how many bytes of a transcript are read between two looks
// at the clock; listedEvents bounds the events one report lists.
const (
	readCheck    = 1 << 20
	listedEvents = 20
)

// TranscriptBudget is how long into a run the transcripts are read: what
// is not read by then waits for the next run, and the run keeps the rest
// of its budget to act and report. A variable so the tests can run out of
// it.
var TranscriptBudget = 30 * time.Second

// claudeConfigEnv moves Claude Code's directory from `~/.claude`.
const claudeConfigEnv = "CLAUDE_CONFIG_DIR"

// sessionIDShape is a Claude Code session id, the only form the watch
// puts into a path. valueShape is a value a report may carry: a model's
// name, a time, an id; the agent can write its own transcript, so any
// other text, which could pass for more of the report, is left out.
var (
	sessionIDShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	valueShape     = regexp.MustCompile(`^[A-Za-z0-9<][A-Za-z0-9._:+<>\[\]-]{0,99}$`)
)

// reportable is `value` as a report shows it: as it is when it is empty or
// has the shape of a value, else a mark that it was left out.
func reportable(value string) string {
	if value == "" || valueShape.MatchString(value) {
		return value
	}
	return "(unreadable)"
}

// modelEvent is one model event of a transcript: its metadata, never the
// conversation's text.
type modelEvent struct {
	Key        string
	Kind       string
	At         string
	From       string
	To         string
	StartModel string
	UUID       string
}

// transcriptRecord is what the watch reads of one line of a transcript.
// It has no field for the conversation's text, so none is ever kept.
type transcriptRecord struct {
	Type          string `json:"type"`
	Subtype       string `json:"subtype"`
	UUID          string `json:"uuid"`
	Timestamp     string `json:"timestamp"`
	IsSidechain   bool   `json:"isSidechain"`
	OriginalModel string `json:"originalModel"`
	FallbackModel string `json:"fallbackModel"`
	Message       struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
	} `json:"message"`
}

// transcriptState is how far a transcript was read, with the model of
// its session's first and latest assistant message (the first taken from
// a fallback's original model when the fallback comes first), the model
// its latest fallback after a refusal moved it to, and whether the latest
// message is stopped: refused by a safety classifier, or followed by a
// stop with no fallback, with no assistant message after it.
type transcriptState struct {
	readTo     int64
	startModel string
	lastModel  string
	fellTo     string
	stopped    bool
	// recovered is whether, at a line of this read, the session was
	// neither stopped nor moved; not recorded.
	recovered bool
}

// moved is whether a fallback after a refusal moved the session to the
// model it now runs on, another than it started on; a change no fallback
// made, such as a person's `/model`, is not.
func (st transcriptState) moved() bool {
	return realModel(st.startModel) && realModel(st.lastModel) && baseModel(st.startModel) != baseModel(st.lastModel) &&
		baseModel(st.fellTo) == baseModel(st.lastModel)
}

// variantSuffix is what a model's name may carry for a variant of the
// same model, such as `[1m]` for its long context.
var variantSuffix = regexp.MustCompile(`\[[^\]]*\]$`)

// baseModel is `model` without a variant suffix, so `claude-x[1m]` and
// `claude-x` are one model.
func baseModel(model string) string {
	return variantSuffix.ReplaceAllString(model, "")
}

// realModel is whether `model` names a model: Claude Code writes the
// messages it makes up itself, such as an API error, as `<synthetic>`.
func realModel(model string) bool {
	return model != "" && !strings.HasPrefix(model, "<")
}

// scanTranscript reads the complete lines of the transcript at `path`
// after `st`, until its last complete line or until `stop` says the time
// is up (asked every readCheck bytes), and returns the model events there,
// the state after them and whether it read to the last complete line. A
// transcript shorter than what was read is another file and is read again
// from its start; one gone is read as having nothing new.
func scanTranscript(path string, st transcriptState, stop func() bool) ([]modelEvent, transcriptState, bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, st, true, nil
	}
	if err != nil {
		return nil, st, false, exit.IO(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, st, false, exit.IO(err)
	}
	if info.Size() < st.readTo {
		st = transcriptState{}
	}
	if _, err := f.Seek(st.readTo, io.SeekStart); err != nil {
		return nil, st, false, exit.IO(err)
	}
	s := &transcriptScan{st: st, seen: map[string]bool{}}
	reader := bufio.NewReaderSize(f, 1<<20)
	complete := true
	for checked := st.readTo; ; {
		if s.st.readTo-checked >= readCheck {
			if checked = s.st.readTo; stop() {
				complete = false
				break
			}
		}
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, st, false, exit.IO(err)
		}
		s.line(line)
	}
	for i := range s.events {
		s.events[i].StartModel = reportable(s.st.startModel)
	}
	return s.events, s.st, complete, nil
}

// transcriptScan is one read of a transcript: the state so far, the events
// found and the keys of the refusals among them.
type transcriptScan struct {
	st     transcriptState
	events []modelEvent
	seen   map[string]bool
}

// line reads one complete line of the transcript and notes when the
// session is neither stopped nor moved after it.
func (s *transcriptScan) line(line []byte) {
	s.read(line)
	if !s.st.stopped && !s.st.moved() {
		s.st.recovered = true
	}
}

// fellBack notes the fallback `rec`: the model it moved the session to,
// and the one the session started on when no reply came before it.
func (s *transcriptScan) fellBack(rec transcriptRecord) {
	if s.st.startModel == "" && realModel(rec.OriginalModel) {
		s.st.startModel, s.st.lastModel = rec.OriginalModel, rec.OriginalModel
	}
	s.st.fellTo = rec.FallbackModel
}

// read reads one complete line of the transcript.
func (s *transcriptScan) read(line []byte) {
	at := s.st.readTo
	s.st.readTo += int64(len(line))
	if !bytes.Contains(line, []byte(`"assistant"`)) && !bytes.Contains(line, []byte(`"model_refusal_`)) {
		return
	}
	var rec transcriptRecord
	if json.Unmarshal(line, &rec) != nil || rec.IsSidechain {
		return
	}
	event := func(kind, from, to string) modelEvent {
		key := rec.UUID
		if key == "" {
			key = fmt.Sprintf("%s@%d", kind, at)
		}
		return modelEvent{Key: key, Kind: kind, At: reportable(rec.Timestamp), From: reportable(from),
			To: reportable(to), UUID: reportable(rec.UUID)}
	}
	switch {
	case rec.Type == "system" && rec.Subtype == eventFallback:
		s.events = append(s.events, event(eventFallback, rec.OriginalModel, rec.FallbackModel))
		s.fellBack(rec)
	case rec.Type == "system" && rec.Subtype == eventNoFallback:
		s.events = append(s.events, event(eventNoFallback, rec.OriginalModel, ""))
		s.st.stopped = true
	case rec.Type == "assistant" && realModel(rec.Message.Model):
		model := rec.Message.Model
		switch {
		case s.st.startModel == "":
			s.st.startModel = model
		case baseModel(model) != baseModel(s.st.lastModel):
			s.events = append(s.events, event(eventChange, s.st.lastModel, model))
		}
		s.st.lastModel = model
		if s.st.stopped = rec.Message.StopReason == "refusal"; !s.st.stopped {
			return
		}
		// Each content block of a message is a record of its own, so a
		// refused message is one event, keyed by its id.
		e := event(eventRefusal, model, "")
		if rec.Message.ID != "" {
			e.Key = "message:" + rec.Message.ID
		}
		if !s.seen[e.Key] {
			s.seen[e.Key] = true
			s.events = append(s.events, e)
		}
	}
}

// claudeDir is Claude Code's directory: CLAUDE_CONFIG_DIR, else
// `~/.claude`.
func claudeDir() string {
	if dir := os.Getenv(claudeConfigEnv); dir != "" {
		return dir
	}
	return filepath.Join(os.Getenv("HOME"), ".claude")
}

// jsonInt is a whole number of a decoded herdr reply; 0 when it is none.
func jsonInt(value any) int64 {
	n, _ := value.(json.Number)
	i, _ := n.Int64()
	return i
}

// sessionOf is the id of the Claude Code session running in `pane`: of
// the processes in the pane's foreground, those Claude Code registered in
// `<dir>/sessions/<pid>.json`, the one started first (a Claude Code the
// agent runs in its own commands starts after it); "" when none is.
func (r *watchRun) sessionOf(dir, pane string) (string, error) {
	result, err := r.h.CallOK("pane", "process-info", "--pane", pane)
	if err != nil {
		return "", err
	}
	info, _ := herdr.Lookup(result, "process_info").(map[string]any)
	foreground, _ := info["foreground_processes"].([]any)
	session, first := "", int64(0)
	for _, entry := range foreground {
		process, _ := entry.(map[string]any)
		pid := jsonInt(process["pid"])
		data, err := os.ReadFile(filepath.Join(dir, "sessions", fmt.Sprintf("%d.json", pid)))
		if err != nil {
			continue
		}
		var s struct {
			PID       int64  `json:"pid"`
			SessionID string `json:"sessionId"`
			StartedAt int64  `json:"startedAt"`
		}
		if json.Unmarshal(data, &s) != nil || s.PID != pid || !sessionIDShape.MatchString(s.SessionID) {
			continue
		}
		if session == "" || s.StartedAt < first {
			session, first = s.SessionID, s.StartedAt
		}
	}
	return session, nil
}

// findTranscript is the transcript of `session`,
// `<dir>/projects/<the directory's name>/<session>.jsonl`; "" until
// Claude Code wrote it.
func findTranscript(dir, session string) (string, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "projects", "*", session+".jsonl"))
	if err != nil || len(paths) == 0 {
		return "", err
	}
	return paths[0], nil
}

// modelChanges runs the watch `model-changes`: it reads what is new in
// the transcript of each live Claude Code agent in herdr, records the
// model events there, acts on the sessions that are now stopped or moved
// to another model (watchtakeover.go), and reports what it found and
// what it did.
func (r *watchRun) modelChanges(inHerdr map[string]InHerdr) error {
	e := r.builtinWatch(config.ModelChanges)
	if due, err := r.due(e); err != nil || !due {
		return err
	}
	rows, err := LiveRows(r.conn, nil)
	if err != nil {
		return err
	}
	reads := r.readTranscripts(rows, inHerdr)
	for _, live := range rows {
		if r.outOfTime() {
			return nil
		}
		if read := reads[live.ID]; read != nil && read.complete {
			if err := r.actOnSession(live.ID, read, inHerdr[live.Name]); err != nil {
				return err
			}
		}
	}
	cut, err := r.reportModelEvents(e, reads)
	if err != nil || cut {
		return err
	}
	return r.ran(e)
}

// readTranscripts reads what is new in the transcript of each live
// Claude Code agent in herdr among `rows`, by row; a failed read is the
// run's failure and skips that agent.
func (r *watchRun) readTranscripts(rows []Live, inHerdr map[string]InHerdr) map[int64]*sessionRead {
	dir := claudeDir()
	reads := map[int64]*sessionRead{}
	for _, live := range rows {
		if r.outOfTime() {
			break
		}
		agent, ok := inHerdr[live.Name]
		if !ok || live.Pane == "" || agent.Kind != "" && agent.Kind != KindClaude {
			continue
		}
		read, err := r.readTranscript(dir, live)
		if err != nil {
			r.failed(fmt.Errorf("the transcript of %s: %w", live.Name, err))
		}
		if read != nil {
			reads[live.ID] = read
		}
	}
	return reads
}

// sessionRead is what this run read of an agent's session: the session,
// its transcript and that file's size before this run read it (-1 when
// it could not tell), the transcript's state, whether this run read it to
// its last complete line, and whether the watch already decided about its
// present stop or move (`transcripts.decided_at`).
type sessionRead struct {
	session  string
	path     string
	size     int64
	st       transcriptState
	complete bool
	decided  bool
}

// readTranscript reads what is new in the transcript of `live`'s session,
// as long as TranscriptBudget allows, and records it with the events
// found, in one transaction; each new event is a line on stdout. Nil
// while the agent has no session or transcript yet.
func (r *watchRun) readTranscript(dir string, live Live) (*sessionRead, error) {
	session, err := r.sessionOf(dir, live.Pane)
	if err != nil || session == "" {
		return nil, err
	}
	var path string
	var st transcriptState
	var decided sql.NullInt64
	err = r.conn.QueryRow("SELECT path, read_to, start_model, last_model, fell_to, stopped, decided_at FROM "+
		"transcripts WHERE agent = ?1 AND session = ?2", live.ID, session).Scan(&path, &st.readTo, &st.startModel,
		&st.lastModel, &st.fellTo, &st.stopped, &decided)
	if errors.Is(err, sql.ErrNoRows) {
		if path, err = findTranscript(dir, session); err != nil || path == "" {
			return nil, err
		}
	} else if err != nil {
		return nil, exit.Database(err)
	}
	size := int64(-1)
	if info, err := os.Stat(path); err == nil {
		size = info.Size()
	}
	events, next, complete, err := scanTranscript(path, st,
		func() bool { return time.Since(r.start) > TranscriptBudget })
	if err != nil {
		return nil, err
	}
	read := &sessionRead{session: session, path: path, size: size, st: next, complete: complete, decided: decided.Valid}
	if next == st {
		return read, nil
	}
	tx, err := r.conn.Begin()
	if err != nil {
		return nil, exit.Database(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("INSERT INTO transcripts (agent, session, path, read_to, start_model, last_model, fell_to, "+
		"stopped) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8) ON CONFLICT (agent, session) DO UPDATE SET read_to = "+
		"excluded.read_to, start_model = excluded.start_model, last_model = excluded.last_model, fell_to = "+
		"excluded.fell_to, stopped = excluded.stopped",
		live.ID, session, path, next.readTo, next.startModel, next.lastModel, next.fellTo, next.stopped); err != nil {
		return nil, exit.Database(err)
	}
	var found []modelEvent
	for _, ev := range events {
		res, err := tx.Exec("INSERT OR IGNORE INTO model_events (agent, key, kind, at, from_model, to_model, start_model, "+
			"uuid, found_at) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9)",
			live.ID, ev.Key, ev.Kind, ev.At, ev.From, ev.To, ev.StartModel, ev.UUID, r.now)
		if err != nil {
			return nil, exit.Database(err)
		}
		if n, _ := res.RowsAffected(); n == 1 {
			found = append(found, ev)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, exit.Database(err)
	}
	for _, ev := range found {
		fmt.Fprintf(os.Stdout, "model event: %s (%s): %s\n", live.Name, live.Role, ev.line())
	}
	if !complete {
		fmt.Fprintf(os.Stdout, "model watch: the transcript of %s (%s) was not read to its end this run\n", live.Name,
			live.Role)
	}
	return read, nil
}

// line is the event as the agents read it.
func (ev modelEvent) line() string {
	var what string
	switch ev.Kind {
	case eventFallback:
		what = fmt.Sprintf("%s: %s -> %s", ev.Kind, ev.From, ev.To)
	case eventNoFallback:
		what = fmt.Sprintf("%s: stopped on %s, no fallback", ev.Kind, ev.From)
	case eventRefusal:
		what = fmt.Sprintf("stopped by a safety classifier (stop_reason refusal) on %s", ev.From)
	default:
		what = fmt.Sprintf("%s: %s -> %s, the session started on %s", ev.Kind, ev.From, ev.To, ev.StartModel)
	}
	return fmt.Sprintf("%s %s (record %s)", ev.At, what, ev.UUID)
}

// zhLine is the event as the people in a thread read it.
func (ev modelEvent) zhLine() string {
	var what string
	switch ev.Kind {
	case eventFallback:
		what = fmt.Sprintf("降级 (`%s`)：`%s` → `%s`", ev.Kind, ev.From, ev.To)
	case eventNoFallback:
		what = fmt.Sprintf("被拦停，没有降级 (`%s`)，模型 `%s`", ev.Kind, ev.From)
	case eventRefusal:
		what = fmt.Sprintf("被 safety classifier 拦停 (`stop_reason: refusal`)，模型 `%s`", ev.From)
	default:
		what = fmt.Sprintf("模型从 `%s` 换成了 `%s` (会话开始时是 `%s`)", ev.From, ev.To, ev.StartModel)
	}
	return fmt.Sprintf("%s %s，记录 `%s`", ev.At, what, ev.UUID)
}

// What the watch did about a session stopped or moved to another model,
// `model_actions.action`.
const (
	// actionReported: only reported, as for a thread agent moved to
	// another model or a watch worker.
	actionReported = "reported"
	// actionReplaced: the session of a lead or worker was replaced.
	actionReplaced = "replaced"
	// actionNudged: a thread agent was told to go on.
	actionNudged = "nudged"
	// actionHeld: the agent was handled before, so the people decide.
	actionHeld = "held"
	// actionFailed: the watch could not take the agent over; `detail`
	// says why.
	actionFailed = "failed"
)

// pendingEvent is a recorded event, with whether it is still to be
// reported to its agent's parent (`tell`) and to its thread (`post`).
type pendingEvent struct {
	id   int64
	ev   modelEvent
	tell bool
	post bool
}

// modelAction is what the watch did about one stop or move of a session:
// why (`stopped` or `moved`, from the model it started on to the one it
// runs on), what, when, the detail, and whether it is still to be
// reported to the agent's parent and to its thread.
type modelAction struct {
	id     int64
	action string
	why    string
	from   string
	to     string
	detail actionDetail
	at     int64
	tell   bool
	post   bool
}

// eventAgent is an agent row with what a report and a takeover need of it,
// and its events and actions still to report, in the order found.
type eventAgent struct {
	id        int64
	name      string
	role      string
	job       string
	parent    string
	thread    string
	state     string
	pane      string
	cwd       string
	task      string
	issue     string
	issueURL  string
	kind      string
	model     sql.NullString
	effort    sql.NullString
	light     bool
	takeovers int
	startedAt int64
	events    []pendingEvent
	actions   []modelAction
}

// loadAgent is the agent row `id`.
func loadAgent(conn *sql.DB, id int64) (*eventAgent, error) {
	a := &eventAgent{id: id}
	err := conn.QueryRow("SELECT name, role, job, parent, thread, state, pane_id, cwd, task, issue, issue_url, kind, "+
		"model, effort, light, takeovers, started_at FROM agents WHERE id = ?1", id).Scan(&a.name, &a.role, &a.job,
		&a.parent, &a.thread, &a.state, &a.pane, &a.cwd, &a.task, &a.issue, &a.issueURL, &a.kind, &a.model, &a.effort,
		&a.light, &a.takeovers, &a.startedAt)
	return a, dbErr(err)
}

// reportModelEvents reports each recorded event and each action once to
// the agent's parent, through `send`'s path as `watch`, when the parent
// has a live row (a thread agent has none to tell), and once to its thread
// (a thread agent's own, a job's agent's home thread) when it has one and
// the scope a socket; one message and one post per agent and run, saying
// when this run did not read the agent's transcript to its end (`reads`).
// A tell that did not land, or a post that failed, is tried again next
// run. A broken prompt tells nobody this run, and is `cut`.
func (r *watchRun) reportModelEvents(e config.Entry, reads map[int64]*sessionRead) (cut bool, err error) {
	agents, err := r.pendingReports()
	if err != nil {
		return false, err
	}
	prompt, promptErr := watchPrompt(r.cfg, e)
	for _, a := range agents {
		if r.outOfTime() {
			return true, nil
		}
		thread, parent, err := r.destinations(a)
		if err != nil {
			return cut, err
		}
		partial := reads[a.id] != nil && !reads[a.id].complete
		tell, post := a.pending(true), a.pending(false)
		if !tell.empty() && promptErr != nil && parent != "" {
			r.failed(r.promptBroken(promptErr, thread, "tell "+parent+" about the model events of "+a.name))
			cut, tell = true, modelReport{}
		}
		if err := r.tellReport(a, parent, tell, partial, prompt); err != nil {
			return cut, err
		}
		if err := r.postReport(a, thread, post, partial); err != nil {
			return cut, err
		}
	}
	return cut, nil
}

// destinations are where the reports about `a` go: its thread (a thread
// agent's own, a job's agent's home thread) and its parent when it has a
// live row (a thread agent's parent is nobody); "" for none.
func (r *watchRun) destinations(a *eventAgent) (thread, parent string, err error) {
	if a.role == "thread" {
		return a.thread, "", nil
	}
	if thread, err = homeThreadOf(r.conn, a.job, a.startedAt); err != nil {
		return "", "", err
	}
	parent, err = liveName(r.conn, a.parent)
	return thread, parent, err
}

// modelReport is what one message or post says about an agent.
type modelReport struct {
	events  []pendingEvent
	actions []modelAction
}

func (rep modelReport) empty() bool { return len(rep.events) == 0 && len(rep.actions) == 0 }

// pending is what is still to tell the parent of `a`, or with `tell`
// false still to post to its thread.
func (a *eventAgent) pending(tell bool) modelReport {
	var rep modelReport
	for _, p := range a.events {
		if tell && p.tell || !tell && p.post {
			rep.events = append(rep.events, p)
		}
	}
	for _, x := range a.actions {
		if tell && x.tell || !tell && x.post {
			rep.actions = append(rep.actions, x)
		}
	}
	return rep
}

// pendingReports are the agents with events or actions not yet reported,
// by row. The rows are read first and the agents after: the ledger has
// one connection.
func (r *watchRun) pendingReports() ([]*eventAgent, error) {
	events := map[int64][]pendingEvent{}
	actions := map[int64][]modelAction{}
	var ids []int64
	err := eachRow(r.conn, "SELECT agent, id, kind, at, from_model, to_model, start_model, uuid, told_at IS NULL, "+
		"posted_at IS NULL FROM model_events WHERE told_at IS NULL OR posted_at IS NULL ORDER BY agent, id",
		func(rows *sql.Rows) error {
			var id int64
			var p pendingEvent
			if err := rows.Scan(&id, &p.id, &p.ev.Kind, &p.ev.At, &p.ev.From, &p.ev.To, &p.ev.StartModel, &p.ev.UUID,
				&p.tell, &p.post); err != nil {
				return err
			}
			ids = append(ids, id)
			events[id] = append(events[id], p)
			return nil
		})
	if err != nil {
		return nil, err
	}
	err = eachRow(r.conn, "SELECT agent, id, action, why, from_model, to_model, detail, detail_zh, at, told_at IS NULL, "+
		"posted_at IS NULL FROM model_actions WHERE told_at IS NULL OR posted_at IS NULL ORDER BY agent, id",
		func(rows *sql.Rows) error {
			var id int64
			var x modelAction
			if err := rows.Scan(&id, &x.id, &x.action, &x.why, &x.from, &x.to, &x.detail.en, &x.detail.zh, &x.at,
				&x.tell, &x.post); err != nil {
				return err
			}
			ids = append(ids, id)
			actions[id] = append(actions[id], x)
			return nil
		})
	if err != nil {
		return nil, err
	}
	slices.Sort(ids)
	var agents []*eventAgent
	for _, id := range slices.Compact(ids) {
		a, err := loadAgent(r.conn, id)
		if err != nil {
			return nil, err
		}
		a.events, a.actions = events[id], actions[id]
		agents = append(agents, a)
	}
	return agents, nil
}

// homeThreadOf is the home thread of the job `job` an agent started at
// `startedAt` belongs to, the latest of that name started by then; ""
// without one.
func homeThreadOf(conn *sql.DB, job string, startedAt int64) (string, error) {
	var thread string
	err := conn.QueryRow("SELECT home_thread FROM jobs WHERE job = ?1 AND started_at <= ?2 ORDER BY id DESC LIMIT 1",
		job, startedAt).Scan(&thread)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return thread, dbErr(err)
}

// liveName is `name` when an agent of that name has a live row, else "".
func liveName(conn *sql.DB, name string) (string, error) {
	var n int
	err := conn.QueryRow("SELECT count(*) FROM agents WHERE name = ?1 AND state != 'ended'", name).Scan(&n)
	if err != nil || n == 0 {
		return "", dbErr(err)
	}
	return name, nil
}

// tellReport tells `parent` what `rep` holds about `a`; with no parent
// there is nobody to tell, and it is done. Without a clear signal (exit
// 2) the message may have arrived, so it is not sent again; a blocked or
// missing parent is told next run.
func (r *watchRun) tellReport(a *eventAgent, parent string, rep modelReport, partial bool, prompt *promptText) error {
	if rep.empty() || parent == "" {
		return r.markReported(rep, "told_at")
	}
	text, err := WithHeader(watchSender, watchMessage(prompt, a.name, reportMessage(a, rep, partial)))
	if err != nil {
		return err
	}
	got, err := Deliver(r.h, parent, text)
	if err != nil {
		r.failed(err)
		return nil
	}
	r.got(got)
	if got == exit.Ok || got == exit.Unknown {
		return r.markReported(rep, "told_at")
	}
	return nil
}

// postReport posts what `rep` holds about `a` to `thread`; with no thread
// or no socket there is nowhere to post, and it is done. A post that
// failed is tried next run.
func (r *watchRun) postReport(a *eventAgent, thread string, rep modelReport, partial bool) error {
	if !rep.empty() && thread != "" && r.cfg.FednetSocket != "" {
		if err := fednet.Post(r.cfg.FednetSocket, thread, reportPost(a, rep, partial)); err != nil {
			r.failed(err)
			return nil
		}
	}
	return r.markReported(rep, "posted_at")
}

// markReported sets `column` of what `rep` holds to the run's clock.
func (r *watchRun) markReported(rep modelReport, column string) error {
	for _, p := range rep.events {
		if _, err := r.conn.Exec("UPDATE model_events SET "+column+" = ?1 WHERE id = ?2", r.now, p.id); err != nil {
			return exit.Database(err)
		}
	}
	for _, x := range rep.actions {
		if _, err := r.conn.Exec("UPDATE model_actions SET "+column+" = ?1 WHERE id = ?2", r.now, x.id); err != nil {
			return exit.Database(err)
		}
	}
	return nil
}

// reportMessage is the message to an agent's parent about `rep`.
func reportMessage(a *eventAgent, rep modelReport, partial bool) string {
	who := a.name + " (" + a.role
	if a.job != "" {
		who += ", job " + a.job
	}
	var text strings.Builder
	if len(rep.events) > 0 {
		fmt.Fprintf(&text, "fleet watch, watch `model-changes`: the Claude Code session of %s) has %d model event(s) "+
			"in its transcript:\n", who, len(rep.events))
		for i, p := range rep.events {
			if i == listedEvents {
				fmt.Fprintf(&text, "- and %d more, each a line of `fleet watch`'s output\n", len(rep.events)-i)
				break
			}
			fmt.Fprintf(&text, "- %s\n", p.ev.line())
		}
	} else {
		fmt.Fprintf(&text, "fleet watch, watch `model-changes`: about the Claude Code session of %s):\n", who)
	}
	if partial {
		text.WriteString("\nThis run did not read its transcript to the end, so fleet did nothing about it yet; the " +
			"next run reads on.\n")
	}
	for _, x := range rep.actions {
		fmt.Fprintf(&text, "\n%s\n", x.line(a.name))
	}
	if len(rep.actions) == 0 && !partial {
		text.WriteString("\nfleet did nothing about these events: the session is not stopped now and no fallback moved " +
			"it to the model it runs on (a model change no fallback made, such as a person's `/model`, is left to the " +
			"people), or it is still busy and what fleet does about it is reported on its own.\n")
	}
	return text.String()
}

// reportPost is the post to an agent's thread about `rep`.
func reportPost(a *eventAgent, rep modelReport, partial bool) string {
	var text strings.Builder
	if len(rep.events) > 0 {
		fmt.Fprintf(&text, "fleet watch：%s 的 Claude Code 会话出现了 %d 个模型事件。\n", a.name, len(rep.events))
		for i, p := range rep.events {
			if i == listedEvents {
				fmt.Fprintf(&text, "- 另有 %d 个，见 `fleet watch` 的输出\n", len(rep.events)-i)
				break
			}
			fmt.Fprintf(&text, "- %s\n", p.ev.zhLine())
		}
	} else {
		fmt.Fprintf(&text, "fleet watch：关于 %s 的 Claude Code 会话。\n", a.name)
	}
	if partial {
		text.WriteString("\n这一轮没有把它的 transcript 读完 (未读完)，所以 fleet 还没有做任何处理，下一轮接着读。\n")
	}
	for _, x := range rep.actions {
		fmt.Fprintf(&text, "\n%s\n", x.zhLine(a.name, a.role))
	}
	if len(rep.actions) == 0 && !partial {
		text.WriteString("\nfleet 没有处理这些事件：会话现在没有被拦停，现在用的模型也不是降级换来的 (没有降级记录的模型变化，" +
			"比如有人用 `/model` 换的，交给人定)；或者它还在忙，之后怎么处理会另外报告。\n")
	}
	return text.String()
}

// whyLine says why the watch acted: the session is stopped, or a fallback
// moved it to another model.
func (x modelAction) whyLine(zh bool) string {
	switch {
	case x.why == "moved" && zh:
		return fmt.Sprintf("会话被拦下后降级，现在用的是 `%s`，开始时是 `%s`", x.to, x.from)
	case x.why == "moved":
		return fmt.Sprintf("a fallback after a refusal moved the session to %s; it started on %s", x.to, x.from)
	case zh:
		return fmt.Sprintf("会话被拦停住了 (最后一条回复被 safety classifier 拦下，之后没有新的回复)，模型 `%s`", x.to)
	}
	return fmt.Sprintf("the session is stopped: its latest message, on %s, was stopped by a safety classifier, with "+
		"no reply after it", x.to)
}

// line is the action about the lead or worker `name` as its parent reads
// it; a thread agent has no parent to tell.
func (x modelAction) line(name string) string {
	at := time.Unix(x.at, 0).UTC().Format(time.RFC3339)
	why := x.whyLine(false)
	switch x.action {
	case actionReplaced:
		return fmt.Sprintf("At %s fleet replaced the session of %s (%s) because %s: it closed the old session and "+
			"started a new one of the same name in a new tab, on the model and effort recorded at its start, with the "+
			"same directory, task, work order and parent, told to go on from where the work stands without redoing "+
			"the step that was stopped, as it was or in other words. fleet does this once per agent.",
			at, name, x.detail.en, why)
	case actionNudged:
		return fmt.Sprintf("At %s fleet told %s once, as watch, to go on without redoing the step that was stopped, "+
			"as it was or in other words, because %s; it changed no session and switched no model. fleet does this "+
			"once per session.", at, name, why)
	case actionHeld:
		return fmt.Sprintf("At %s fleet did nothing although %s: it handled %s once already (a new session, or a "+
			"message to go on), so a person decides what to do now.", at, why, name)
	case actionFailed:
		return fmt.Sprintf("At %s fleet could not take %s over (%s): %s. A person decides what to do now; fleet "+
			"will not handle this agent again.", at, name, why, x.detail.en)
	}
	return fmt.Sprintf("At %s fleet only reported that %s: it changed no session, switched no model and typed "+
		"nothing into %s. A person decides what to do.", at, why, name)
}

// zhLine is the action about the agent `name`, of `role`, as the people
// in a thread read it.
func (x modelAction) zhLine(name, role string) string {
	at := time.Unix(x.at, 0).UTC().Format(time.RFC3339)
	why := x.whyLine(true)
	switch {
	case x.action == actionReplaced && role == "thread":
		return fmt.Sprintf("%s fleet 就地换了 `%s` 的会话 (%s)，原因：%s。像回收会话那样结束了原会话 (在工单上留了一份 fleet "+
			"自己按记录的元数据写的总结)，再像 `inbox` 那样给线程另起了新会话，还是同一张工单，并交代它从线程的现状接着做，"+
			"不要原样或换个说法重做被拦的那一步。每个线程只自动换一次。", at, name, x.detail.zh, why)
	case x.action == actionReplaced:
		return fmt.Sprintf("%s fleet 就地换了 `%s` 的会话 (%s)，原因：%s。关掉原会话，在新 tab 里按原来记下的模型和 effort "+
			"起了同名的新会话，目录、任务、work order 和派活的一方都不变，并交代它从现状接着做，不要原样或换个说法重做被拦的"+
			"那一步。每个 agent 只自动换一次。", at, name, x.detail.zh, why)
	case x.action == actionNudged:
		return fmt.Sprintf("%s fleet 以 watch 的名义给 `%s` 发了一次「接着干」，原因：%s。交代它不要原样或换个说法重做被拦"+
			"的那一步；没有换会话，也没有切模型。每次会话只发一次。", at, name, why)
	case x.action == actionHeld:
		return fmt.Sprintf("%s fleet 这次没有处理 (%s)：`%s` 已经被自动处理过一次 (换过会话或收到过「接着干」)，"+
			"需要人来定怎么办。", at, why, name)
	case x.action == actionFailed:
		return fmt.Sprintf("%s fleet 没能接手 `%s` (%s)：%s。需要人来定怎么办，fleet 不会再自动处理这个 agent。", at, name,
			why, x.detail.zh)
	}
	return fmt.Sprintf("%s fleet 只报告 (%s)：没有换会话，没有切换模型，也没有往 `%s` 里输入任何东西。需要人来定怎么办。", at,
		why, name)
}

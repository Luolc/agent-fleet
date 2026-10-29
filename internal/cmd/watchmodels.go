// The watch `model-changes` of `fleet watch` (docs/design.md): the model
// events in each live agent's Claude Code transcript, reported to its
// parent and its thread. fleet only reports them: it switches no model and
// types nothing into the agent.

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
	"strings"

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

// transcriptChunk bounds what one run reads of one transcript, so a long
// one read for the first time does not take the run's time; the rest
// waits for the next run. listedEvents bounds the events one report
// lists.
const (
	transcriptChunk = 32 << 20
	listedEvents    = 20
)

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
// its session's first and latest assistant message.
type transcriptState struct {
	readTo     int64
	startModel string
	lastModel  string
}

// realModel is whether `model` names a model: Claude Code writes the
// messages it makes up itself, such as an API error, as `<synthetic>`.
func realModel(model string) bool {
	return model != "" && !strings.HasPrefix(model, "<")
}

// scanTranscript reads the complete lines of the transcript at `path`
// after `st`, at most transcriptChunk bytes of them, and returns the model
// events there and the state after them. A transcript shorter than what
// was read is another file and is read again from its start; one gone is
// read as having nothing new.
func scanTranscript(path string, st transcriptState) ([]modelEvent, transcriptState, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, st, nil
	}
	if err != nil {
		return nil, st, exit.IO(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, st, exit.IO(err)
	}
	if info.Size() < st.readTo {
		st = transcriptState{}
	}
	if _, err := f.Seek(st.readTo, io.SeekStart); err != nil {
		return nil, st, exit.IO(err)
	}
	s := &transcriptScan{st: st, seen: map[string]bool{}}
	reader := bufio.NewReaderSize(f, 1<<20)
	for start := st.readTo; s.st.readTo-start < transcriptChunk; {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, st, exit.IO(err)
		}
		s.line(line)
	}
	for i := range s.events {
		s.events[i].StartModel = reportable(s.st.startModel)
	}
	return s.events, s.st, nil
}

// transcriptScan is one read of a transcript: the state so far, the events
// found and the keys of the refusals among them.
type transcriptScan struct {
	st     transcriptState
	events []modelEvent
	seen   map[string]bool
}

// line reads one complete line of the transcript.
func (s *transcriptScan) line(line []byte) {
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
	case rec.Type == "system" && rec.Subtype == eventNoFallback:
		s.events = append(s.events, event(eventNoFallback, rec.OriginalModel, ""))
	case rec.Type == "assistant" && realModel(rec.Message.Model):
		model := rec.Message.Model
		switch {
		case s.st.startModel == "":
			s.st.startModel = model
		case model != s.st.lastModel:
			s.events = append(s.events, event(eventChange, s.st.lastModel, model))
		}
		s.st.lastModel = model
		if rec.Message.StopReason != "refusal" {
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
// model events there, and reports those not reported yet.
func (r *watchRun) modelChanges(inHerdr map[string]InHerdr) error {
	e := r.builtinWatch(config.ModelChanges)
	if due, err := r.due(e); err != nil || !due {
		return err
	}
	rows, err := LiveRows(r.conn, nil)
	if err != nil {
		return err
	}
	dir := claudeDir()
	for _, live := range rows {
		if r.outOfTime() {
			return nil
		}
		agent, ok := inHerdr[live.Name]
		if !ok || live.Pane == "" || agent.Kind != "" && agent.Kind != KindClaude {
			continue
		}
		if err := r.readTranscript(dir, live); err != nil {
			r.failed(fmt.Errorf("the transcript of %s: %w", live.Name, err))
		}
	}
	cut, err := r.reportModelEvents(e)
	if err != nil || cut {
		return err
	}
	return r.ran(e)
}

// readTranscript reads what is new in the transcript of `live`'s session
// and records it with the events found, in one transaction; each new
// event is a line on stdout.
func (r *watchRun) readTranscript(dir string, live Live) error {
	session, err := r.sessionOf(dir, live.Pane)
	if err != nil || session == "" {
		return err
	}
	var path string
	var st transcriptState
	err = r.conn.QueryRow("SELECT path, read_to, start_model, last_model FROM transcripts WHERE agent = ?1 AND session = ?2",
		live.ID, session).Scan(&path, &st.readTo, &st.startModel, &st.lastModel)
	if errors.Is(err, sql.ErrNoRows) {
		if path, err = findTranscript(dir, session); err != nil || path == "" {
			return err
		}
	} else if err != nil {
		return exit.Database(err)
	}
	events, next, err := scanTranscript(path, st)
	if err != nil || next == st {
		return err
	}
	tx, err := r.conn.Begin()
	if err != nil {
		return exit.Database(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("INSERT INTO transcripts (agent, session, path, read_to, start_model, last_model) "+
		"VALUES (?1, ?2, ?3, ?4, ?5, ?6) ON CONFLICT (agent, session) DO UPDATE SET read_to = excluded.read_to, "+
		"start_model = excluded.start_model, last_model = excluded.last_model",
		live.ID, session, path, next.readTo, next.startModel, next.lastModel); err != nil {
		return exit.Database(err)
	}
	var found []modelEvent
	for _, ev := range events {
		res, err := tx.Exec("INSERT OR IGNORE INTO model_events (agent, key, kind, at, from_model, to_model, start_model, "+
			"uuid, found_at) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9)",
			live.ID, ev.Key, ev.Kind, ev.At, ev.From, ev.To, ev.StartModel, ev.UUID, r.now)
		if err != nil {
			return exit.Database(err)
		}
		if n, _ := res.RowsAffected(); n == 1 {
			found = append(found, ev)
		}
	}
	if err := tx.Commit(); err != nil {
		return exit.Database(err)
	}
	for _, ev := range found {
		fmt.Fprintf(os.Stdout, "model event: %s (%s): %s\n", live.Name, live.Role, ev.line())
	}
	return nil
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

// pendingEvent is a recorded event, with whether it is still to be
// reported to its agent's parent (`tell`) and to its thread (`post`).
type pendingEvent struct {
	id   int64
	ev   modelEvent
	tell bool
	post bool
}

// eventAgent is an agent row with events to report, in the order found.
type eventAgent struct {
	id        int64
	name      string
	role      string
	job       string
	parent    string
	thread    string
	startedAt int64
	events    []pendingEvent
}

// reportModelEvents reports each recorded event once to the agent's
// parent, through `send`'s path as `watch`, when the parent has a live
// row (a thread agent has none to tell), and once to its thread (a thread
// agent's own, a job's agent's home thread) when it has one and the scope
// a socket; one message and one post per agent and run. A tell that did
// not land, or a post that failed, is tried again next run. A broken
// prompt tells nobody this run, and is `cut`.
func (r *watchRun) reportModelEvents(e config.Entry) (cut bool, err error) {
	agents, err := r.pendingEvents()
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
		tell, post := a.pending()
		if len(tell) > 0 && promptErr != nil && parent != "" {
			r.failed(r.promptBroken(promptErr, thread, "tell "+parent+" about the model events of "+a.name))
			cut, tell = true, nil
		}
		if err := r.tellEvents(a, parent, tell, prompt); err != nil {
			return cut, err
		}
		if err := r.postEvents(a, thread, post); err != nil {
			return cut, err
		}
	}
	return cut, nil
}

// destinations are where the events of `a` go: its thread (a thread
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

// pending are the events of `a` still to tell its parent and still to
// post to its thread.
func (a *eventAgent) pending() (tell, post []pendingEvent) {
	for _, p := range a.events {
		if p.tell {
			tell = append(tell, p)
		}
		if p.post {
			post = append(post, p)
		}
	}
	return tell, post
}

// pendingEvents are the agents with events not yet reported, by row.
func (r *watchRun) pendingEvents() ([]*eventAgent, error) {
	var agents []*eventAgent
	err := eachRow(r.conn, "SELECT e.id, e.kind, e.at, e.from_model, e.to_model, e.start_model, e.uuid, "+
		"e.told_at IS NULL, e.posted_at IS NULL, a.id, a.name, a.role, a.job, a.parent, a.thread, a.started_at "+
		"FROM model_events e JOIN agents a ON a.id = e.agent WHERE e.told_at IS NULL OR e.posted_at IS NULL "+
		"ORDER BY a.id, e.id",
		func(rows *sql.Rows) error {
			var p pendingEvent
			var a eventAgent
			if err := rows.Scan(&p.id, &p.ev.Kind, &p.ev.At, &p.ev.From, &p.ev.To, &p.ev.StartModel, &p.ev.UUID,
				&p.tell, &p.post, &a.id, &a.name, &a.role, &a.job, &a.parent, &a.thread, &a.startedAt); err != nil {
				return err
			}
			if len(agents) == 0 || agents[len(agents)-1].id != a.id {
				agents = append(agents, &a)
			}
			last := agents[len(agents)-1]
			last.events = append(last.events, p)
			return nil
		})
	return agents, err
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

// tellEvents tells `parent` about `events` of `a`; with no parent there
// is nobody to tell, and they are done. Without a clear signal (exit 2)
// the message may have arrived, so it is not sent again; a blocked or
// missing parent is told next run.
func (r *watchRun) tellEvents(a *eventAgent, parent string, events []pendingEvent, prompt *promptText) error {
	if len(events) == 0 || parent == "" {
		return r.markEvents(events, "told_at")
	}
	text, err := WithHeader(watchSender, watchMessage(prompt, a.name, eventsMessage(a, events)))
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
		return r.markEvents(events, "told_at")
	}
	return nil
}

// postEvents posts `events` of `a` to `thread`; with no thread or no
// socket there is nowhere to post, and they are done. A post that failed
// is tried next run.
func (r *watchRun) postEvents(a *eventAgent, thread string, events []pendingEvent) error {
	if len(events) > 0 && thread != "" && r.cfg.FednetSocket != "" {
		if err := fednet.Post(r.cfg.FednetSocket, thread, eventsPost(a, events)); err != nil {
			r.failed(err)
			return nil
		}
	}
	return r.markEvents(events, "posted_at")
}

// markEvents sets `column` of `events` to the run's clock.
func (r *watchRun) markEvents(events []pendingEvent, column string) error {
	for _, p := range events {
		if _, err := r.conn.Exec("UPDATE model_events SET "+column+" = ?1 WHERE id = ?2", r.now, p.id); err != nil {
			return exit.Database(err)
		}
	}
	return nil
}

// eventsMessage is the message to an agent's parent about `events`.
func eventsMessage(a *eventAgent, events []pendingEvent) string {
	who := a.name + " (" + a.role
	if a.job != "" {
		who += ", job " + a.job
	}
	var text strings.Builder
	fmt.Fprintf(&text, "fleet watch, watch `model-changes`: the Claude Code session of %s) has %d model event(s) "+
		"in its transcript:\n", who, len(events))
	for i, p := range events {
		if i == listedEvents {
			fmt.Fprintf(&text, "- and %d more, each a line of `fleet watch`'s output\n", len(events)-i)
			break
		}
		fmt.Fprintf(&text, "- %s\n", p.ev.line())
	}
	return text.String()
}

// eventsPost is the post to an agent's thread about `events`.
func eventsPost(a *eventAgent, events []pendingEvent) string {
	var text strings.Builder
	fmt.Fprintf(&text, "fleet watch：%s 的 Claude Code 会话出现了 %d 个模型事件。fleet 只报告，没有切换模型，"+
		"也没有往这个 agent 里输入任何东西。\n", a.name, len(events))
	for i, p := range events {
		if i == listedEvents {
			fmt.Fprintf(&text, "- 另有 %d 个，见 `fleet watch` 的输出\n", len(events)-i)
			break
		}
		fmt.Fprintf(&text, "- %s\n", p.ev.zhLine())
	}
	return text.String()
}

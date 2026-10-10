// Thread agents: the name a thread's agent gets, the `threads` workspace,
// starting a thread agent for a message (`fleet inbox` does it), and the
// commands a thread agent runs on its own thread: `fleet thread end`,
// `set-project` and `relate`.

package cmd

import (
	"crypto/sha256"
	"database/sql"
	_ "embed" // the built-in prompt
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Luolc/agent-fleet/internal/atb"
	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/fednet"
	"github.com/Luolc/agent-fleet/internal/herdr"
	"github.com/Luolc/agent-fleet/internal/identity"
)

// ThreadAbout, ThreadEndAbout and the others are the help texts of
// `thread` and its subcommands.
const (
	ThreadAbout         = "What a thread agent does to its own thread: post to it, end the session, set the ticket's project, relate an issue"
	ThreadPostAbout     = "Post to your thread in Slack (thread agents only)"
	ThreadPostLongAbout = "Post to your thread in Slack (thread agents only).\n\n" +
		"The text is read from --body-file and posted with `fednet client post` to the " +
		"thread your row in the ledger records, through the socket in the scope's settings " +
		"(`fednet.socket`); you never name either. Each --attach is passed to fednet as " +
		"`-file`, unchanged, and uploaded with the text. fednet's stdout is printed; fleet " +
		"does not retry.\n\n" +
		"Exit: 0 when fednet took the post; 1 when the caller is not a live thread agent, the " +
		"body file cannot be read, is empty or is over 100 KiB (102400 bytes), or the socket " +
		"is not configured; fednet's own exit code, with its stderr as it is, when the post " +
		"fails; 5 when fednet cannot be run or times out, or the settings or the database fail."
	ThreadEndAbout     = "End this session of your thread: summary on the ticket, ticket released, tab closed (thread agents only)"
	ThreadEndLongAbout = "End this session of your thread: summary on the ticket, ticket released, tab closed (thread agents only).\n\n" +
		"Call it from your own pane when nothing is pending for you; a job you started keeps " +
		"running. --summary-file is required and must not be empty. With a thread ticket " +
		"(FLEET_ISSUE), first the summary is written to it as a comment headed `Session <n> " +
		"ended` (`atb linear comment`), then the ticket is released as done (`atb linear " +
		"release`); a later message in the thread starts a new session that gets every such " +
		"summary. Then your row in the ledger is ended, and last your tab is closed, which " +
		"ends your own pane.\n\n" +
		"Each step done is recorded in the ledger (table `steps`), so running it again after " +
		"a failure skips the steps done and continues with the rest; an atb exit 4 (no " +
		"holder) on the retry is a failure, never taken as the step having been done. The tab " +
		"is found from the pane your row recorded, so it is closed even when herdr no longer " +
		"has the agent (it exited to the pane's shell), and checked gone afterwards. With " +
		"--force a Linear step that fails is skipped and listed at the end, with the command " +
		"to finish it by hand, and the local cleanup (row, tab) is done anyway.\n\n" +
		"Exit: 0 when the session ended (you will not see it: the tab closes); 1 when the " +
		"caller is not a thread agent started by `fleet inbox` (FLEET_THREAD), or the summary " +
		"file cannot be read or is empty; 5 when an atb step fails without --force (the steps " +
		"before it stay recorded), or herdr or the database fails."
	ThreadSetProjectAbout     = "Put your thread ticket into a Linear project (thread agents only)"
	ThreadSetProjectLongAbout = "Put your thread ticket into a Linear project (thread agents only).\n\n" +
		"The ticket is FLEET_ISSUE; the project is matched exactly against the unarchived " +
		"projects of the ticket's team (`atb linear set-project`). A ticket already in that " +
		"project is left alone; one in another project is refused by atb.\n\n" +
		"Exit: 0; 1 when the caller is not a thread agent or has no ticket (thread tickets are " +
		"off for this scope); 5 when atb fails."
	ThreadRelateAbout     = "Relate your thread ticket to an issue (thread agents only)"
	ThreadRelateLongAbout = "Relate your thread ticket to an issue (thread agents only).\n\n" +
		"Adds a `related` relation between FLEET_ISSUE and <ISSUE> (`atb linear relate`); a " +
		"relation already between them is left alone. `fleet job start` relates the ticket to " +
		"the job's parent issue itself.\n\n" +
		"Exit: 0; 1 when the caller is not a thread agent, has no ticket, or <ISSUE> is not an " +
		"identifier such as ABC-12; 5 when atb fails."
)

// threadsWorkspace is the herdr workspace thread agents run in, one tab
// per thread.
const threadsWorkspace = "threads"

// inboxSender is the header name of the messages `fleet inbox` delivers.
const inboxSender = "inbox"

//go:embed thread_prompt.md
var threadPrompt string

// ThreadSlug is the slug of a thread key: lower-cased, every run of other
// characters one `-`, and cut with a hash when `thread-<slug>` would pass
// herdr's 32 characters. Stable: the same key always gives the same slug.
func ThreadSlug(key string) string {
	var b strings.Builder
	dash := true
	for _, c := range strings.ToLower(key) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			b.WriteRune(c)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	slug := strings.TrimRight(b.String(), "-")
	if slug == "" {
		slug = "x"
	}
	if len("thread-"+slug) > 32 {
		sum := sha256.Sum256([]byte(key))
		slug = strings.TrimRight(slug[:16], "-") + "-" + hex.EncodeToString(sum[:4])
	}
	return slug
}

// threadRow is a thread as the ledger records it.
type threadRow struct {
	Thread, Slug, Channel, Context, Ticket, TicketURL, Mapping, Cwd string
	Sessions                                                        int64
}

// threadByKey is the thread `key`, or nil when this scope has not seen it.
func threadByKey(conn querier, key string) (*threadRow, error) {
	var r threadRow
	err := conn.QueryRow("SELECT thread, slug, channel, context, ticket, ticket_url, mapping, cwd, sessions FROM threads WHERE thread = ?1",
		key).Scan(&r.Thread, &r.Slug, &r.Channel, &r.Context, &r.Ticket, &r.TicketURL, &r.Mapping, &r.Cwd, &r.Sessions)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, exit.Database(err)
	}
	return &r, nil
}

// liveThreadAgent is the name and state of the live thread agent of
// `key`, or "" when none.
func liveThreadAgent(conn querier, key string) (name, state string, err error) {
	err = conn.QueryRow("SELECT name, state FROM agents WHERE role = 'thread' AND thread = ?1 AND state != 'ended'",
		key).Scan(&name, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", exit.Database(err)
	}
	return name, state, nil
}

// endRow ends the live row named `name`.
func endRow(conn querier, name string) error {
	if _, err := conn.Exec("UPDATE agents SET state = 'ended', ended_at = ?1 WHERE name = ?2 AND state != 'ended'",
		db.Now(), name); err != nil {
		return exit.Database(err)
	}
	return nil
}

// generalMapping is the mapping of a direct message to the bot, and of
// the channel of the same name.
const generalMapping = "x-repo-general"

// ChannelMapping is where a message's thread belongs: `x-repo-general` for
// a direct message, else the channel's name when it is `repo-<R>` or
// `x-repo-<I>`, else "": fleet serves no other channel.
func ChannelMapping(channelName string, dm bool) string {
	if dm {
		return generalMapping
	}
	for _, prefix := range []string{"repo-", "x-repo-"} {
		if rest, ok := strings.CutPrefix(channelName, prefix); ok && CheckRepo(rest) == nil {
			return channelName
		}
	}
	return ""
}

// MappingDir is the directory a mapping's thread agents run in: the main
// checkout ~/dev/<R> for `repo-<R>`, the checkout of the initiative's repo
// ~/x-repo/<I> for `x-repo-<I>`. fleet never makes either.
func MappingDir(home, mapping string) string {
	if rest, ok := strings.CutPrefix(mapping, "x-repo-"); ok {
		return filepath.Join(home, "x-repo", rest)
	}
	return filepath.Join(home, "dev", strings.TrimPrefix(mapping, "repo-"))
}

// crossRepoRoot is the directory the cross-repo jobs of a thread put their
// leads' directories in: the thread's own for `x-repo-<I>`, ~/x-repo/general
// for a `repo-<R>` thread.
func crossRepoRoot(home, mapping string) string {
	if strings.HasPrefix(mapping, "x-repo-") {
		return MappingDir(home, mapping)
	}
	return MappingDir(home, generalMapping)
}

// tempFile writes `content` to a new file for an atb `--body-file` or
// `--description-file`, and returns its path with a remover.
func tempFile(content string) (string, func(), error) {
	f, err := os.CreateTemp("", "fleet-*.md")
	if err != nil {
		return "", nil, exit.IO(err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", nil, exit.IO(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", nil, exit.IO(err)
	}
	return f.Name(), func() { _ = os.Remove(f.Name()) }, nil
}

// inboundMessage is what a thread agent is given: a message from a
// person, as `fleet inbox` got it, or (`Question` set) a question from
// an agent for the people in the thread, from `fleet ask-human` or the
// job's conclusion.
type inboundMessage struct {
	Thread, Text, User, TS, Context string
	// Mapping is the thread's mapping as the message gives it (empty for
	// a question): used only for a thread the ledger does not know.
	Mapping string
	// Question is the asking agent's name; Conclusion marks a job's
	// conclusion, which needs no answer.
	Question   string
	Conclusion bool
}

// channel is the channel part of the thread key `CHANNEL/TS`.
func (m inboundMessage) channel() string {
	channel, _, _ := strings.Cut(m.Thread, "/")
	return channel
}

// sender is the header name the message is delivered under.
func (m inboundMessage) sender() string {
	if m.Question != "" {
		return m.Question
	}
	return inboxSender
}

// body is the message as the thread agent reads it.
func (m inboundMessage) body() string {
	if m.Conclusion {
		return fmt.Sprintf("Conclusion of a job from %s for the people in thread %s. Post it to the thread with "+
			"`fleet thread post`; nothing is waiting for an answer.\n\n%s", m.Question, m.Thread, m.Text)
	}
	if m.Question != "" {
		return fmt.Sprintf("Question from %s for the people in thread %s, already posted there by fleet; "+
			"when they answer, pass the answer on with `fleet send %s --file <file>`.\n\n%s\n", m.Question, m.Thread, m.Question, m.Text)
	}
	return fmt.Sprintf("Message in thread %s from %s at %s:\n\n%s\n", m.Thread, m.User, m.TS, m.Text)
}

// trigger is what the `Session <n> started` comment names.
func (m inboundMessage) trigger() string {
	if m.Conclusion {
		return "the conclusion of a job from " + m.Question
	}
	if m.Question != "" {
		return "a question from " + m.Question
	}
	return fmt.Sprintf("a message from %s at %s", m.User, m.TS)
}

// threadStart is one start of a thread agent for a message.
type threadStart struct {
	h       *herdr.Herdr
	conn    *sql.DB
	cfg     *config.Scope
	msg     inboundMessage
	id      *identity.Identity
	mapping string
	cwd     string
	home    string
	known   *threadRow // nil for a thread this scope has not seen
	session int64
	ticket  atb.Issue
	notes   []string // for the prompt: earlier summaries
	created []string
}

// newThreadStart checks what a start needs before anything is written:
// the thread's name, whether the thread is known, and where it belongs: a
// known thread keeps the mapping and directory recorded at its first
// delivery, a new one takes them from the message.
func newThreadStart(h *herdr.Herdr, conn *sql.DB, scope string, cfg *config.Scope, msg inboundMessage) (*threadStart, error) {
	home, err := Home()
	if err != nil {
		return nil, err
	}
	s := &threadStart{h: h, conn: conn, cfg: cfg, msg: msg, home: home,
		id: &identity.Identity{Agent: "thread-" + ThreadSlug(msg.Thread), Role: identity.Thread, Scope: scope, Thread: msg.Thread}}
	if err := CheckAgentName(s.id.Agent); err != nil {
		return nil, err
	}
	if s.known, err = threadByKey(conn, msg.Thread); err != nil {
		return nil, err
	}
	if err := s.belong(); err != nil {
		return nil, err
	}
	return s, nil
}

// belong sets the thread's mapping and directory: the recorded ones of a
// known thread, else the message's.
func (s *threadStart) belong() error {
	if s.known != nil && s.known.Mapping != "" {
		s.mapping, s.cwd = s.known.Mapping, s.known.Cwd
		return nil
	}
	if s.msg.Mapping == "" {
		return exit.Environmentf("the ledger records no channel for thread %s", s.msg.Thread)
	}
	s.mapping, s.cwd = s.msg.Mapping, MappingDir(s.home, s.msg.Mapping)
	return nil
}

// reserve writes the agent's row (`starting`) and counts the session on
// the thread's row, in one transaction with the check that the thread has
// no live agent.
func (s *threadStart) reserve() error {
	return reserve(s.conn, func(q querier) error {
		if err := liveNameTaken(q, s.id.Agent); err != nil {
			return err
		}
		name, _, err := liveThreadAgent(q, s.msg.Thread)
		if err != nil {
			return err
		}
		if name != "" {
			return exit.Refusedf("thread %s already has the live agent %s", s.msg.Thread, name)
		}
		return nil
	}, func(q querier) error {
		if _, err := q.Exec("INSERT INTO threads (thread, slug, channel, context, mapping, cwd, sessions, created_at) "+
			"VALUES (?1, ?2, ?3, ?4, ?5, ?6, 1, ?7) "+
			"ON CONFLICT (thread) DO UPDATE SET sessions = sessions + 1, "+
			"context = CASE WHEN excluded.context != '' THEN excluded.context ELSE context END",
			s.msg.Thread, ThreadSlug(s.msg.Thread), s.msg.channel(), s.msg.Context, s.mapping, s.cwd, db.Now()); err != nil {
			return exit.Database(err)
		}
		if err := q.QueryRow("SELECT sessions FROM threads WHERE thread = ?1", s.msg.Thread).Scan(&s.session); err != nil {
			return exit.Database(err)
		}
		return insertStarting(q, s.id, s.cwd, "", "")
	})
}

// unreserve takes the reservation back after the Linear steps failed:
// the row is ended and the session uncounted.
func (s *threadStart) unreserve() error {
	if err := endRow(s.conn, s.id.Agent); err != nil {
		return err
	}
	if _, err := s.conn.Exec("UPDATE threads SET sessions = sessions - 1 WHERE thread = ?1", s.msg.Thread); err != nil {
		return exit.Database(err)
	}
	return nil
}

// linearSteps are the thread ticket's steps when the scope has a Linear
// team: a new thread gets its ticket created (label `thread`, no project)
// and claimed; a known thread gets its ticket claimed again, a `Session
// <n> started` comment, and its earlier summaries read for the prompt.
// Any failure means Linear is unavailable to the caller.
func (s *threadStart) linearSteps() error {
	if s.cfg.LinearTeam == "" {
		return nil
	}
	if s.known == nil || s.known.Ticket == "" {
		title := WorkOrderTitle(s.msg.Text)
		if title == "" {
			title = "Thread " + s.msg.Thread
		}
		description := fmt.Sprintf("channel: %s\nthread: %s\nfirst message, from %s at %s:\n\n%s\n",
			s.msg.channel(), s.msg.Thread, s.msg.User, s.msg.TS, s.msg.Text)
		if s.msg.Context != "" {
			description += "\nchannel context:\n\n" + s.msg.Context + "\n"
		}
		file, remove, err := tempFile(description)
		if err != nil {
			return err
		}
		defer remove()
		if s.ticket, err = atb.CreateThreadTicket(s.cfg.LinearTeam, title, file); err != nil {
			return err
		}
		s.created = append(s.created, fmt.Sprintf("thread ticket %s (%s)", s.ticket.Identifier, s.ticket.URL))
		if _, err := s.conn.Exec("UPDATE threads SET ticket = ?1, ticket_url = ?2 WHERE thread = ?3",
			s.ticket.Identifier, s.ticket.URL, s.msg.Thread); err != nil {
			return exit.Database(err)
		}
	} else {
		s.ticket = atb.Issue{Identifier: s.known.Ticket, URL: s.known.TicketURL}
	}
	s.id.Issue = s.ticket.Identifier
	if err := setIssue(s.conn, s.id.Agent, s.ticket.Identifier); err != nil {
		return err
	}
	if err := atb.Claim(s.ticket.Identifier, s.id.Agent, s.msg.Thread, s.mapping+": thread "+s.msg.Thread); err != nil {
		return err
	}
	if s.session == 1 {
		return nil
	}
	file, remove, err := tempFile(fmt.Sprintf("Session %d started\n\nTriggered by %s.\n", s.session, s.msg.trigger()))
	if err != nil {
		return err
	}
	defer remove()
	if err := atb.Comment(s.ticket.Identifier, file); err != nil {
		return err
	}
	s.notes, err = summaries(s.ticket.Identifier)
	return err
}

var sessionEnded = regexp.MustCompile(`^Session [0-9]+ ended\n`)

// summaries are the `Session <n> ended` comments of `ticket`, oldest
// first, as `fleet thread end` wrote them.
func summaries(ticket string) ([]string, error) {
	op := "atb linear query " + ticket
	out, err := atb.Query(op, `{ issue(id: "`+ticket+`") { comments { nodes { body createdAt } } } }`)
	if err != nil {
		return nil, err
	}
	type comment struct{ Body, CreatedAt string }
	var reply struct {
		Data *struct {
			Issue *struct{ Comments struct{ Nodes []comment } }
		}
		Issue *struct{ Comments struct{ Nodes []comment } }
	}
	if json.Unmarshal(out, &reply) != nil {
		return nil, exit.Environmentf("%s printed no JSON; its output is not shown", op)
	}
	node := reply.Issue
	if node == nil && reply.Data != nil {
		node = reply.Data.Issue
	}
	if node == nil {
		return nil, exit.Environmentf("%s printed no issue; its output is not shown", op)
	}
	var found []comment
	for _, c := range node.Comments.Nodes {
		if sessionEnded.MatchString(c.Body) {
			found = append(found, c)
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].CreatedAt < found[j].CreatedAt })
	notes := make([]string, len(found))
	for i, c := range found {
		notes[i] = strings.TrimSpace(c.Body)
	}
	return notes, nil
}

// rules is how the thread agent decides between a single-repo and a
// cross-repo job, by the thread's mapping.
func (s *threadStart) rules() string {
	if repo, ok := strings.CutPrefix(s.mapping, "repo-"); ok {
		return fmt.Sprintf("This thread belongs to the repo %[1]s. Work asked for here is a single-repo job in %[1]s "+
			"(`fleet job start <job> --repo %[1]s ...`), unless the people say it reaches other repos; then it is a "+
			"cross-repo job (no `--repo`).", repo)
	}
	return fmt.Sprintf("This thread belongs to the cross-repo initiative %s; %s is its charter, read it first. "+
		"Decide from the request whether the work touches one repo (`--repo <R>`) or several (a cross-repo job, "+
		"no `--repo`); when unsure, ask in the thread.", strings.TrimPrefix(s.mapping, "x-repo-"), filepath.Join(s.cwd, "AGENTS.md"))
}

// prompt is the thread agent's first message: the built-in prompt, the
// channel's context, the earlier summaries, and the message.
func (s *threadStart) prompt() string {
	post := "posting to the thread is off: the fednet socket is not configured for this scope (`fednet.socket` in its config file)"
	if s.cfg.FednetSocket != "" {
		post = "`fleet thread post --body-file <file> [--attach <path>]...` posts the file's text to your thread " +
			"(fleet knows which thread and how), with the files to upload: progress, answers, a job's conclusion"
	}
	ticket := s.ticket.Identifier
	note := " (your thread ticket, " + s.ticket.URL + ")"
	if ticket == "" {
		note = " (empty: thread tickets are off for this scope)"
	}
	text := strings.NewReplacer("{{scope}}", s.id.Scope, "{{agent}}", s.id.Agent,
		"{{mapping}}", s.mapping, "{{cwd}}", s.cwd, "{{rules}}", s.rules(), "{{xrepo}}", crossRepoRoot(s.home, s.mapping),
		"{{ticket}}", ticket, "{{ticket_note}}", note, "{{post}}", post).Replace(threadPrompt)
	context := s.msg.Context
	if context == "" && s.known != nil {
		context = s.known.Context
	}
	if context != "" {
		text += "\n## Channel context\n\n" + context + "\n"
	}
	if len(s.notes) > 0 {
		text += "\n## Earlier sessions on this thread\n\n" + strings.Join(s.notes, "\n\n") + "\n"
	}
	return text + "\n## The message\n\n" + s.msg.body()
}

// place is the thread's new tab in the `threads` workspace, after the
// workspace and its `shell` tab are made or restored.
func (s *threadStart) place() (Place, error) {
	workspace, err := threadsWorkspaceID(s.h, s.id.Scope, s.home)
	if err != nil {
		return Place{}, err
	}
	slug := ThreadSlug(s.msg.Thread)
	place, err := CreateTab(s.h, workspace, slug, s.cwd, s.id)
	if err == nil {
		s.created = append(s.created, fmt.Sprintf("tab %s (%s)", slug, place.TabID))
	}
	return place, err
}

// start starts the thread agent and delivers the prompt with the message.
// Returns the exit code of the delivery; a failure after the reservation
// is printed with what was created, the row staying `starting` (the next
// message for the thread starts again once herdr has no such agent).
func (s *threadStart) start() (exit.Code, error) {
	s.created = append(s.created, fmt.Sprintf("ledger row %s (state starting)", s.id.Agent))
	place, err := s.place()
	if err != nil {
		return startFailed(s.id.Agent, err, 0, s.created, "")
	}
	code, err := startAndDeliver(s.h, s.conn, s.id, place, nil, nil, s.msg.sender(), s.prompt(), "", &s.created)
	if err != nil || code != exit.Ok {
		return startFailed(s.id.Agent, err, code, s.created, "")
	}
	fmt.Fprintf(os.Stdout, "started %s for thread %s (session %d, %s)\n", s.id.Agent, s.msg.Thread, s.session, s.cwd)
	return exit.Ok, nil
}

// linearUnavailable is what `fleet inbox` does when a Linear step failed:
// no agent (the reservation taken back), then the thread is told.
// An agent's question (`Question` set) gets the failure back instead of
// a post: nobody in the thread asked anything.
func (s *threadStart) linearUnavailable(cause error) error {
	fmt.Fprintf(os.Stderr, "fleet: Linear is unavailable, no thread agent started: %v\n", cause)
	if err := s.unreserve(); err != nil {
		return err
	}
	if s.msg.Question != "" {
		return nil
	}
	return s.tell("Linear is unavailable right now, so no agent was started for this thread; please try again later.",
		"Linear is unavailable")
}

// notHere is what a start does when the thread's directory is not on this
// machine: nothing is reserved or started, and the thread is told. An
// agent's question gets an error instead.
func (s *threadStart) notHere() error {
	what := "the repo " + strings.TrimPrefix(s.mapping, "repo-")
	if strings.HasPrefix(s.mapping, "x-repo-") {
		what = "the repo of " + s.mapping
	}
	if s.msg.Question != "" {
		return exit.Environmentf("%s is not checked out at %s on this machine", what, s.cwd)
	}
	fmt.Fprintf(os.Stderr, "fleet: no checkout at %s, no thread agent started\n", s.cwd)
	return s.tell(fmt.Sprintf("%s is not checked out on this machine (%s), so no agent was started for this thread.",
		capitalize(what), s.cwd), "the checkout is missing")
}

// tell posts one line to the thread; only when the post succeeded is the
// message dropped. A post that fails, or no socket to post with, is exit
// 5: the message stays reserved and fednet runs the hook again.
func (s *threadStart) tell(line, what string) error {
	if s.cfg.FednetSocket == "" {
		return exit.Environmentf("fednet.socket is not configured, so the thread cannot be told; the message is kept for a retry")
	}
	if err := fednet.Post(s.cfg.FednetSocket, s.msg.Thread, line); err != nil {
		return exit.Environmentf("%v; the thread was not told, the message is kept for a retry", err)
	}
	fmt.Fprintf(os.Stdout, "posted to the thread that %s\n", what)
	return nil
}

// capitalize upper-cases the first letter of an ASCII sentence.
func capitalize(text string) string {
	return strings.ToUpper(text[:1]) + text[1:]
}

// resume finishes a start an earlier run was killed in, found as a live
// row still `starting` with its agent in herdr: the agent is got to its
// input box, the earlier summaries are read again, and the full first
// message is delivered, after which the row is active. Nothing new is
// created.
func resumeThreadStart(h *herdr.Herdr, conn *sql.DB, scope string, cfg *config.Scope, msg inboundMessage, name string) (exit.Code, error) {
	fmt.Fprintf(os.Stderr, "note: %s is still starting from an earlier run; finishing that start\n", name)
	home, err := Home()
	if err != nil {
		return 0, err
	}
	known, err := threadByKey(conn, msg.Thread)
	if err != nil {
		return 0, err
	}
	if known == nil {
		return 0, exit.Environmentf("the ledger has %s but no row for thread %s", name, msg.Thread)
	}
	var pane string
	if err := conn.QueryRow("SELECT pane_id FROM agents WHERE name = ?1 AND state != 'ended'", name).Scan(&pane); err != nil {
		return 0, exit.Database(err)
	}
	s := &threadStart{h: h, conn: conn, cfg: cfg, msg: msg, known: known, session: known.Sessions, home: home,
		ticket: atb.Issue{Identifier: known.Ticket, URL: known.TicketURL},
		id:     &identity.Identity{Agent: name, Role: identity.Thread, Scope: scope, Thread: msg.Thread, Issue: known.Ticket}}
	if err := s.belong(); err != nil {
		return 0, err
	}
	if known.Ticket != "" && known.Sessions > 1 {
		if s.notes, err = summaries(known.Ticket); err != nil {
			return 0, err
		}
	}
	if err := SettleAgent(h, name, pane); err != nil {
		return 0, err
	}
	text, err := WithHeader(s.msg.sender(), s.prompt())
	if err != nil {
		return 0, err
	}
	code, err := Deliver(h, name, text)
	if err != nil || code != exit.Ok {
		return code, err
	}
	if _, err := conn.Exec("UPDATE agents SET state = 'active' WHERE name = ?1 AND state = 'starting'", name); err != nil {
		return 0, exit.Database(err)
	}
	return exit.Ok, nil
}

// maxPostBytes is the largest body `thread post` takes.
const maxPostBytes = 100 * 1024

// ThreadPostArgs are the arguments of `thread post`.
type ThreadPostArgs struct {
	// BodyFile holds the text.
	BodyFile string
	// Attach are the files to upload with it, as fednet's `-file`.
	Attach []string
}

// ThreadPost runs `thread post`: the thread comes from the caller's live
// row, the socket from the scope's settings, the text only from the file;
// fednet's output and exit code are handed back as they are.
func ThreadPost(args ThreadPostArgs) (exit.Code, error) {
	me, err := threadCaller()
	if err != nil {
		return 0, err
	}
	data, err := os.ReadFile(args.BodyFile)
	if err != nil {
		return 0, exit.Refusedf("cannot read %s: %v", args.BodyFile, err)
	}
	if len(data) > maxPostBytes {
		return 0, exit.Refusedf("the body is %d bytes, over the limit of %d", len(data), maxPostBytes)
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return 0, exit.Refusedf("the body file is empty")
	}
	cfg, err := config.LoadScope(me.Scope)
	if err != nil {
		return 0, err
	}
	if cfg.FednetSocket == "" {
		return 0, exit.Refusedf("fednet.socket is not configured for scope %s, so fleet cannot post to the thread", me.Scope)
	}
	conn, err := db.Open(me.Scope)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	var thread string
	err = conn.QueryRow("SELECT thread FROM agents WHERE name = ?1 AND role = 'thread' AND state != 'ended'", me.Agent).Scan(&thread)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, exit.Refusedf("the ledger has no live thread agent %s", me.Agent)
	}
	if err != nil {
		return 0, exit.Database(err)
	}
	if thread != me.Thread {
		return 0, exit.Refusedf("the ledger records thread %s for %s, not FLEET_THREAD %s", thread, me.Agent, me.Thread)
	}
	stdout, stderr, code, err := fednet.Relay(cfg.FednetSocket, thread, text, args.Attach)
	_, _ = os.Stdout.Write(stdout)
	_, _ = os.Stderr.Write(stderr)
	if err != nil {
		return 0, err
	}
	return exit.Code(code), nil
}

// ThreadEndArgs are the arguments of `thread end`.
type ThreadEndArgs struct {
	// SummaryFile is the session's summary.
	SummaryFile string
	// Force finishes the local cleanup even when a Linear step keeps
	// failing; the steps not done are printed.
	Force bool
}

// threadCaller is the caller as a thread agent started by `fleet inbox`;
// anyone else is refused.
func threadCaller() (*identity.Identity, error) {
	me, err := identity.FromEnv()
	if err != nil {
		return nil, err
	}
	if me.Role != identity.Thread {
		return nil, exit.Refusedf("a %s has no thread; only a thread agent runs `fleet thread`", me.Role)
	}
	if me.Thread == "" {
		return nil, exit.Refusedf("FLEET_THREAD is not set: only a thread agent started by `fleet inbox` runs `fleet thread`")
	}
	return me, nil
}

// ThreadEnd runs `thread end`. The ending's steps (the ticket comment,
// the release, the row) are recorded under `thread-end:<row id>`, so a
// run after a partial failure skips what was done; the tab close is
// always attempted last, since the tab is where the caller runs.
func ThreadEnd(h *herdr.Herdr, args ThreadEndArgs) (exit.Code, error) {
	me, err := threadCaller()
	if err != nil {
		return 0, err
	}
	summary, err := os.ReadFile(args.SummaryFile)
	if err != nil {
		return 0, exit.Refusedf("cannot read %s: %v", args.SummaryFile, err)
	}
	if strings.TrimSpace(string(summary)) == "" {
		return 0, exit.Refusedf("the summary file is empty")
	}
	conn, err := db.Open(me.Scope)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	var rowID, session int64
	if err := conn.QueryRow("SELECT id FROM agents WHERE name = ?1 ORDER BY id DESC LIMIT 1", me.Agent).Scan(&rowID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, exit.Refusedf("the ledger has no row for %s", me.Agent)
		}
		return 0, exit.Database(err)
	}
	if row, err := threadByKey(conn, me.Thread); err != nil {
		return 0, err
	} else if row != nil {
		session = row.Sessions
	}
	e := &threadEnding{conn: conn, key: fmt.Sprintf("thread-end:%d", rowID), force: args.Force}
	if me.Issue != "" {
		file, remove, err := tempFile(fmt.Sprintf("Session %d ended\n\n%s", session, strings.TrimSpace(string(summary))+"\n"))
		if err != nil {
			return 0, err
		}
		defer remove()
		if err := e.linear("comment", func() error { return atb.Comment(me.Issue, file) },
			fmt.Sprintf("atb linear comment %s --body-file <the summary, headed `Session %d ended`>", me.Issue, session)); err != nil {
			return 0, err
		}
		if err := e.linear("release", func() error { return atb.Release(me.Issue, me.Agent, false) },
			fmt.Sprintf("atb linear release %s --agent %s --reason done --done", me.Issue, me.Agent)); err != nil {
			return 0, err
		}
	}
	if err := runStep(conn, e.key, "end-row", func() error { return endRow(conn, me.Agent) }); err != nil {
		return 0, err
	}
	missed := e.missed
	fmt.Fprintf(os.Stdout, "ended session %d of thread %s\n", session, me.Thread)
	if len(missed) > 0 {
		fmt.Fprintf(os.Stdout, "Linear steps not done, finish them by hand:\n")
		for _, m := range missed {
			fmt.Fprintf(os.Stdout, "  - %s\n", m)
		}
	}
	return exit.Ok, closeOwnTab(h, conn, me.Agent)
}

// threadEnding is one `thread end`: its step key, and with --force the
// Linear steps skipped, each as the command to finish it by hand.
type threadEnding struct {
	conn   *sql.DB
	key    string
	force  bool
	missed []string
}

// linear runs a Linear step through runStep; a failure is returned, or
// with --force printed and listed in `missed`.
func (e *threadEnding) linear(step string, do func() error, byHand string) error {
	err := runStep(e.conn, e.key, step, do)
	if err == nil || !e.force {
		return err
	}
	fmt.Fprintf(os.Stderr, "fleet: %v\n", err)
	e.missed = append(e.missed, byHand)
	return nil
}

// closeOwnTab closes the tab the thread agent's row recorded (its pane's
// tab), whether or not herdr still has the agent: an agent that exited
// leaves its tab. A pane herdr no longer has means the tab is gone. The
// close is checked: the tab must be gone afterwards.
func closeOwnTab(h *herdr.Herdr, conn querier, name string) error {
	var pane string
	if err := conn.QueryRow("SELECT pane_id FROM agents WHERE name = ?1 ORDER BY id DESC LIMIT 1", name).Scan(&pane); err != nil {
		return exit.Database(err)
	}
	if pane == "" {
		return exit.Environmentf("the ledger recorded no pane for %s; close its tab by hand", name)
	}
	reply, err := h.Call("pane", "get", pane)
	if err != nil {
		return err
	}
	if reply.Error != nil && reply.Error.Code == "pane_not_found" {
		return nil
	}
	if reply.Error != nil {
		return exit.Environmentf("herdr: %s: %s", reply.Error.Code, reply.Error.Message)
	}
	inner, _ := herdr.Lookup(reply.Result, "pane").(map[string]any)
	tab, _ := inner["tab_id"].(string)
	if tab == "" {
		return exit.Environmentf("herdr pane get %s has no tab_id", pane)
	}
	if _, err := h.CallOK("tab", "close", tab); err != nil {
		return err
	}
	if reply, err = h.Call("tab", "get", tab); err != nil {
		return err
	}
	switch {
	case reply.Error == nil:
		return exit.Environmentf("tab %s of %s is still there after `herdr tab close`; close it by hand", tab, name)
	case reply.Error.Code != "tab_not_found":
		return exit.Environmentf("herdr: %s: %s; whether tab %s of %s is closed is not verified", reply.Error.Code,
			reply.Error.Message, tab, name)
	}
	return nil
}

// ticketCaller is threadCaller with a thread ticket.
func ticketCaller() (*identity.Identity, error) {
	me, err := threadCaller()
	if err != nil {
		return nil, err
	}
	if me.Issue == "" {
		return nil, exit.Refusedf("FLEET_ISSUE is empty: this thread has no ticket (thread tickets are off for scope %s)", me.Scope)
	}
	return me, nil
}

// ThreadSetProject runs `thread set-project`.
func ThreadSetProject(project string) (exit.Code, error) {
	me, err := ticketCaller()
	if err != nil {
		return 0, err
	}
	if strings.TrimSpace(project) == "" {
		return 0, exit.Refusedf("<PROJECT> is empty")
	}
	if err := atb.SetProject(me.Issue, project); err != nil {
		return 0, err
	}
	fmt.Fprintf(os.Stdout, "%s is in project %s\n", me.Issue, project)
	return exit.Ok, nil
}

// ThreadRelate runs `thread relate`.
func ThreadRelate(issue string) (exit.Code, error) {
	me, err := ticketCaller()
	if err != nil {
		return 0, err
	}
	if err := atb.CheckIdentifier(issue); err != nil {
		return 0, err
	}
	if err := atb.Relate(me.Issue, issue); err != nil {
		return 0, err
	}
	fmt.Fprintf(os.Stdout, "%s is related to %s\n", me.Issue, issue)
	return exit.Ok, nil
}

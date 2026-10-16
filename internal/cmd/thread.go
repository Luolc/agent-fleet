// Thread agents: the name a thread's agent gets, the `threads` workspace,
// starting a thread agent for a message (`fleet inbox` does it), and the
// commands a thread agent runs on its own thread: `fleet thread post`,
// `progress`, `end`, `set-project` and `relate`.

package cmd

import (
	"crypto/sha256"
	"database/sql"
	_ "embed" // the built-in prompt
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
	ThreadAbout         = "What a thread agent does to its own thread: post to it, show its progress, end the session, set the ticket's project, relate an issue"
	ThreadPostAbout     = "Post to your thread in Slack (thread agents only)"
	ThreadPostLongAbout = "Post to your thread in Slack (thread agents only).\n\n" +
		"The text is read from --body-file and posted with `fednet client post` to the " +
		"thread your row in the ledger records, through the socket in the scope's settings " +
		"(`fednet.socket`); you never name either. Each --attach is passed to fednet as " +
		"`-file`, unchanged, and uploaded with the text; the text goes as the file has it. fednet's stdout is printed; fleet " +
		"does not retry.\n\n" +
		"Exit: 0 when fednet took the post; 1 when the caller is not a live thread agent, the " +
		"body file cannot be read, is empty (or only whitespace) or is over 100 KiB (102400 bytes), or the socket " +
		"is not configured; fednet's own exit code, with its stderr as it is, when the post " +
		"fails; 5 when fednet cannot be run or times out, or the settings or the database fail."
	ThreadProgressAbout     = "Show or complete the progress card of your thread in Slack (thread agents only)"
	ThreadProgressLongAbout = "Show or complete the progress card of your thread in Slack (thread agents only).\n\n" +
		"A thread has at most one open card: a short title with a spinner and, expanded, its " +
		"items. Each call gives the whole card (`fednet client progress`): --title is the " +
		"status now, about 10 to 20 characters, and each --item is `<text>:<state>` with the " +
		"state `doing`, `done` or `error`; the first call opens the card, each later one " +
		"replaces it, so items can be merged, dropped or rewritten (at most 50). Keep the " +
		"items few. --done completes the card (the items still doing marked done), with the " +
		"closed card's wording when --title or --item come with it; a reply posted with `fleet " +
		"thread post` completes it as well, so --done is for when no reply follows. The thread " +
		"comes from your row in the ledger " +
		"and the socket from the scope's settings (`fednet.socket`); you never name either. " +
		"fednet's stdout is printed; fleet does not retry.\n\n" +
		"Exit: 0 when fednet took the card; 1 when the caller is not a live thread agent, " +
		"--title is missing or empty without --done, there are over 50 items, an item has no " +
		"state or an unknown one, or the socket is not configured; fednet's own " +
		"exit code, with its stderr as it is, when the call fails; 5 when fednet cannot be run " +
		"or times out, or the settings or the database fail."
	ThreadEndAbout     = "End this session of your thread: summary on the ticket, ticket released, a closing line in the thread, tab closed (thread agents only)"
	ThreadEndLongAbout = "End this session of your thread: summary on the ticket, ticket released, a closing line in the thread, tab closed (thread agents only).\n\n" +
		"Call it from your own pane once the conversation is over. While the thread waits (a " +
		"question in it pending, yours or a lead's, or a job whose home thread it is still " +
		"open) it is refused, naming each: the answer and the lead's messages reach the live " +
		"session, an idle one costs nothing, and a new one starts cold. --asked-to-end ends " +
		"it anyway; use it only when the people in the thread asked you to end, or `fleet watch` " +
		"is reclaiming your session. A job you " +
		"started keeps running. --summary-file is required and must not be empty. With a thread ticket " +
		"(FLEET_ISSUE), first the summary is written to it as a comment headed `Session <n> " +
		"ended` (`atb linear comment`), then the ticket is released as done (`atb linear " +
		"release`); a later message in the thread starts a new session that gets every such " +
		"summary. A session that started without its ticket because Linear failed (FLEET_ISSUE empty " +
		"in a scope with a Linear team) has its summary written to the thread's ticket instead, the " +
		"ticket created first when the thread has none (not claimed, so not released); when Linear " +
		"does not answer, the summary is kept on this machine for the next session that reaches " +
		"Linear, and the ending goes on. Then fleet posts one line of small grey text to the thread, `会话已结束 · " +
		"<ticket>` with the ticket linked (`会话已结束` alone without a ticket; nothing when the " +
		"scope has no fednet socket), which also completes an open progress card. Then your " +
		"row in the ledger is ended, and last your tab is closed, which ends your own pane.\n\n" +
		"Each step done is recorded in the ledger (table `steps`), so running it again after " +
		"a failure skips the steps done and continues with the rest, and the closing line is " +
		"posted at most once; an atb exit 4 (no holder) on the retry is a failure, never taken " +
		"as the step having been done. The tab " +
		"is found from the pane your row recorded, so it is closed even when herdr no longer " +
		"has the agent (it exited to the pane's shell), and checked gone afterwards. With " +
		"--force a Linear step that fails is skipped and listed at the end, with the command " +
		"to finish it by hand, a closing line that fails is skipped and said so, and the " +
		"local cleanup (row, tab) is done anyway.\n\n" +
		"Exit: 0 when the session ended (you will not see it: the tab closes); 1 when the " +
		"caller is not a thread agent started by `fleet inbox` (FLEET_THREAD), the summary " +
		"file cannot be read or is empty, or the thread still waits without --asked-to-end; 5 when an atb step or the closing line fails without " +
		"--force (the steps before it stay recorded), or herdr or the database fails."
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
	slug := slugOf(key)
	if len("thread-"+slug) > 32 {
		sum := sha256.Sum256([]byte(key))
		slug = strings.TrimRight(slug[:16], "-") + "-" + hex.EncodeToString(sum[:4])
	}
	return slug
}

// slugOf is `key` lower-cased, with each run of characters other than
// [a-z0-9] as one `-`, none at either end; `x` when nothing is left.
func slugOf(key string) string {
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
// the channel of the same name: the general initiative's.
func generalMapping(ch config.Channels) string {
	return ch.InitiativePrefix + ch.GeneralInitiative
}

// ChannelMapping is where a message's thread belongs: the general
// initiative's mapping for a direct message, else the channel's name when
// it is `<repo prefix><R>` or `<initiative prefix><I>`, else "": fleet
// serves no other channel.
func ChannelMapping(ch config.Channels, channelName string, dm bool) string {
	if dm {
		return generalMapping(ch)
	}
	for _, prefix := range []string{ch.RepoPrefix, ch.InitiativePrefix} {
		if rest, ok := strings.CutPrefix(channelName, prefix); ok && CheckRepo(rest) == nil {
			return channelName
		}
	}
	return ""
}

// MappingDir is the directory a mapping's thread agents run in: the main
// checkout `<checkouts>/<R>` for a repo's channel, the checkout of the
// initiative's repo `<initiatives>/<I>` for an initiative's. fleet never
// makes either.
func MappingDir(sc *config.Scope, mapping string) string {
	if rest, ok := strings.CutPrefix(mapping, sc.Channels.InitiativePrefix); ok {
		return filepath.Join(sc.Paths.Initiatives, rest)
	}
	return filepath.Join(sc.Paths.Checkouts, strings.TrimPrefix(mapping, sc.Channels.RepoPrefix))
}

// mappingScratch is the directory for the files of a mapping's thread
// agents, their `FLEET_SCRATCH`, named as a job's: `<scratch>/<R>` for a
// repo's channel, `<scratch>/<initiative prefix><I>` for an initiative's.
func mappingScratch(sc *config.Scope, mapping string) string {
	if strings.HasPrefix(mapping, sc.Channels.InitiativePrefix) {
		return filepath.Join(sc.Paths.Scratch, mapping)
	}
	return filepath.Join(sc.Paths.Scratch, strings.TrimPrefix(mapping, sc.Channels.RepoPrefix))
}

// jobMapping is the channel of a single-repo job's repo, by the checkout
// its lead runs in: the initiative's channel, whose name is the repo's,
// for an initiative's repo run in the initiative's checkout (as `fleet
// worktree` finds it); else the repo's channel `<repo prefix><R>`.
func jobMapping(sc *config.Scope, repo, checkout string) string {
	if strings.HasPrefix(repo, sc.Channels.InitiativePrefix) && MappingDir(sc, repo) == checkout {
		return repo
	}
	return sc.Channels.RepoPrefix + repo
}

// crossRepoRoot is the directory the cross-repo jobs of a thread put their
// leads' directories in: the thread's own for an initiative's channel, the
// general initiative's for a repo's.
func crossRepoRoot(sc *config.Scope, mapping string) string {
	if strings.HasPrefix(mapping, sc.Channels.InitiativePrefix) {
		return MappingDir(sc, mapping)
	}
	return MappingDir(sc, generalMapping(sc.Channels))
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
// person, as `fleet inbox` got it, a press on a card (`Answer` set), or
// (`Question` set) a question from an agent for the people in the
// thread, from `fleet ask-thread` or the job's conclusion.
type inboundMessage struct {
	Thread, Text, User, TS, Context string
	// Mapping is the thread's mapping as the message gives it (empty for
	// a question): used only for a thread the ledger does not know.
	Mapping string
	// Question is the asking agent's name; Conclusion marks a job's
	// conclusion, which needs no answer.
	Question   string
	Conclusion bool
	// QuestionID is the ledger's id of a lead's question or of the
	// question a press answers.
	QuestionID int64
	// Answer marks a press on a card of the thread agent's, Text its JSON.
	Answer bool
	// Watch marks a notice from `fleet watch`, delivered as it is.
	Watch bool
	// Screen marks a question `watch` asks about an agent stopped at a
	// screen, Worker one a watch worker asks: there is no agent to pass the
	// answer to.
	Screen, Worker bool
}

// channel is the channel part of the thread key `CHANNEL/TS`.
func (m inboundMessage) channel() string {
	channel, _, _ := strings.Cut(m.Thread, "/")
	return channel
}

// fromFleet is whether the message came from an agent or from watch, not
// from a person in the thread.
func (m inboundMessage) fromFleet() bool {
	return m.Question != "" || m.Watch || m.Answer
}

// sender is the header name the message is delivered under.
func (m inboundMessage) sender() string {
	if m.Watch {
		return watchSender
	}
	if m.Question != "" {
		return m.Question
	}
	return inboxSender
}

// seePrompt points a thread agent at what its prompt says to do with a
// message from fleet. The message itself gives the facts and the least
// to do, so an agent started on an older prompt, which has no such
// paragraph, still knows its next step.
const seePrompt = `Your prompt says what to do under "Messages from fleet"`

// body is the message as the thread agent reads it.
func (m inboundMessage) body() string {
	if m.Watch {
		return m.Text
	}
	if m.Answer {
		return fmt.Sprintf("[ANSWER #%d]\nA press on the card of your question #%d in thread %s. %s; at the least, act on "+
			"its outcome: answered (the choice and the note), snoozed (it comes back at snooze_until), closed (no need to "+
			"answer: drop it or ask another way), resurfaced (back in front of the people: wait).\n\n%s\n",
			m.QuestionID, m.QuestionID, m.Thread, seePrompt, m.Text)
	}
	if m.Conclusion {
		return fmt.Sprintf("Conclusion of a job from %s for the people in thread %s. %s; at the least, post it to "+
			"the thread with `fleet thread post`.\n\n%s", m.Question, m.Thread, seePrompt, m.Text)
	}
	if m.Screen {
		return fmt.Sprintf("Question from %s for the people in thread %s about an agent stopped at a screen, already "+
			"posted there by fleet. Nothing to pass on: once they answer, fleet's watch gives their answer to a new "+
			"helper on its next run, or they deal with the screen themselves.\n\n%s\n", m.Question, m.Thread, m.Text)
	}
	if m.Worker {
		return fmt.Sprintf("Question from %s for the people in thread %s, already posted there by fleet. Nothing to "+
			"pass on: once they answer, fleet's watch starts a new worker with their answer on its next run.\n\n%s\n",
			m.Question, m.Thread, m.Text)
	}
	if m.Question != "" {
		return fmt.Sprintf("[QUESTION #%d from %s]\nYours to settle, in thread %s; fleet has posted nothing. %s; at the "+
			"least, answer it with `fleet send %s --file <file>`, or ask the people with `fleet ask-human --card <file> "+
			"--for %d` (this card or one of your own), whose answers reach %s too.\n\n%s\n",
			m.QuestionID, m.Question, m.Thread, seePrompt, m.Question, m.QuestionID, m.Question, m.Text)
	}
	return fmt.Sprintf("Message in thread %s from %s at %s:\n\n%s\n", m.Thread, m.User, m.TS, m.Text)
}

// trigger is what the `Session <n> started` comment names.
func (m inboundMessage) trigger() string {
	if m.Watch {
		return "a notice from fleet watch"
	}
	if m.Conclusion {
		return "the conclusion of a job from " + m.Question
	}
	if m.Answer {
		return fmt.Sprintf("a press on the card of question #%d", m.QuestionID)
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
	notes   []string    // for the prompt: earlier summaries
	text    *promptText // the thread prompt, put together before the reservation
	created []string
	// claimed and commented are whether this session's claim and its
	// `Session <n> started` comment are done.
	claimed, commented bool
	// linearNote and checkoutNote are what the agent is told when a Linear
	// step failed or its checkout was not updated from origin.
	linearNote, checkoutNote string
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
	switch {
	case s.known != nil && s.known.Mapping != "":
		s.mapping, s.cwd = s.known.Mapping, s.known.Cwd
	case s.msg.Mapping == "":
		return exit.Environmentf("the ledger records no channel for thread %s", s.msg.Thread)
	default:
		s.mapping, s.cwd = s.msg.Mapping, MappingDir(s.cfg, s.msg.Mapping)
	}
	s.id.Scratch = mappingScratch(s.cfg, s.mapping)
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
		return insertStarting(q, s.id, s.cwd, "", "", false)
	})
}

// ticketDescription is a new thread ticket's description: where the
// thread is, its first message with who wrote it and when (or, for a
// thread fleet opened for a job, which agent asked), and the channel's
// context when it came with the message.
func (s *threadStart) ticketDescription() string {
	first := fmt.Sprintf("from %s at %s", s.msg.User, s.msg.TS)
	if s.msg.fromFleet() {
		first = s.msg.trigger()
	}
	description := fmt.Sprintf("channel: %s\nthread: %s\nfirst message, %s:\n\n%s\n",
		s.msg.channel(), s.msg.Thread, first, s.msg.Text)
	if s.msg.Context != "" {
		description += "\nchannel context:\n\n" + s.msg.Context + "\n"
	}
	return description
}

// linearSteps are the thread ticket's steps when the scope has a Linear
// team: a new thread, or one whose ticket an earlier session could not
// create, gets its ticket created (label `thread`, no project) and claimed;
// a known thread gets its ticket claimed again, a `Session <n> started`
// comment, and its earlier summaries read for the prompt. Each step is
// tried as linearLog.try does; the first that still fails ends them, and
// the caller starts the agent without the rest (withoutLinear).
func (s *threadStart) linearSteps() error {
	if s.cfg.LinearTeam == "" {
		return nil
	}
	log := newLinearLog(s.id.Scope, s.msg.Thread, s.id.Agent)
	if s.known == nil || s.known.Ticket == "" {
		title := WorkOrderTitle(WithoutLeadingMentions(s.msg.Text))
		if title == "" {
			title = "Thread " + s.msg.Thread
		}
		file, remove, err := tempFile(s.ticketDescription())
		if err != nil {
			return err
		}
		defer remove()
		if err := log.try(func() (err error) {
			s.ticket, err = atb.CreateThreadTicket(s.cfg.LinearTeam, title, file)
			return err
		}); err != nil {
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
	if err := log.try(func() error {
		return atb.Claim(s.ticket.Identifier, s.id.Agent, s.msg.Thread, s.mapping+": thread "+s.msg.Thread)
	}); err != nil {
		return err
	}
	s.claimed = true
	// What earlier sessions kept while Linear was down, before this
	// session's comment, so the comments stay in order.
	if err := log.writeKept(s.ticket.Identifier); err != nil {
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
	if err := log.try(func() error { return atb.Comment(s.ticket.Identifier, file) }); err != nil {
		return err
	}
	s.commented = true
	return log.try(func() (err error) {
		s.notes, err = summaries(s.ticket.Identifier)
		return err
	})
}

var sessionEnded = regexp.MustCompile(`^Session [0-9]+ ended\n`)

var leadingMentions = regexp.MustCompile(`^(?:\s*<@[^<>\s]+>)+\s*`)

// WithoutLeadingMentions is a Slack message without the mentions (`<@U…>`)
// it begins with, as a message addressed to the bot does; the rest is
// kept as it is. For a thread ticket's title; the description keeps the
// text whole.
func WithoutLeadingMentions(text string) string {
	return leadingMentions.ReplaceAllString(text, "")
}

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

// jobKind is the single-repo or cross-repo rule of the thread's mapping,
// the thread prompt's `rules` slot.
func (s *threadStart) jobKind() string {
	if repo, ok := strings.CutPrefix(s.mapping, s.cfg.Channels.RepoPrefix); ok {
		return fmt.Sprintf("This thread belongs to the repo %[1]s. Work asked for here is a single-repo job in %[1]s "+
			"(`fleet job start <job> --repo %[1]s ...`), unless the people say it reaches other repos; then it is a "+
			"cross-repo job (no `--repo`).", repo)
	}
	return fmt.Sprintf("This thread belongs to the cross-repo initiative %s; %s is its charter, read it first. "+
		"Decide from the request whether the work touches one repo (`--repo <R>`) or several (a cross-repo job, "+
		"no `--repo`); when unsure, ask in the thread.", strings.TrimPrefix(s.mapping, s.cfg.Channels.InitiativePrefix), filepath.Join(s.cwd, "AGENTS.md"))
}

// slots are the values of the thread prompt's slots.
func (s *threadStart) slots() map[string]string {
	post := "posting to the thread is off: the fednet socket is not configured for this scope (`fednet.socket` in its config file)"
	progress := "the progress card is off for the same reason: `fleet thread progress` is refused"
	if s.cfg.FednetSocket != "" {
		post = "`fleet thread post --body-file <file> [--attach <path>]...` posts the file's text to your thread " +
			"(fleet knows which thread and how), with the files to upload: your first line, answers, a job's conclusion"
		progress = "`fleet thread progress --title <status> [--item <text>:<doing|done|error>]...` shows a progress " +
			"card in your thread, replaced whole each call; `fleet thread progress --done` completes it"
	}
	ticket := s.ticket.Identifier
	note := " (your thread ticket, " + s.ticket.URL + ")"
	switch {
	case ticket == "" && s.cfg.LinearTeam != "":
		note = " (empty: this session runs without its thread ticket, see `Note from fleet` below)"
	case ticket == "":
		note = " (empty: thread tickets are off for this scope)"
	}
	slots := map[string]string{"scope": s.id.Scope, "agent": s.id.Agent, "mapping": s.mapping, "cwd": s.cwd,
		"rules": s.jobKind(), "xrepo": crossRepoRoot(s.cfg, s.mapping), "checkouts": s.cfg.Tilde(s.cfg.Paths.Checkouts),
		"scratch": s.cfg.Tilde(mappingScratch(s.cfg, s.mapping)), "ticket": ticket, "ticket_note": note, "post": post, "progress": progress}
	maps.Copy(slots, modelValues(s.cfg))
	return slots
}

// firstMessage is the thread agent's first message: the thread prompt,
// the channel's context, the earlier summaries, and the message.
func (s *threadStart) firstMessage() string {
	text := s.text.fill(s.slots())
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
	return text + noteSection(s.linearNote, s.checkoutNote) + "\n## The message\n\n" + s.msg.body()
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
	code, err := startAndDeliver(s.h, s.conn, s.id, place, s.cwd, nil, nil, s.msg.sender(), s.firstMessage(), &s.created)
	if err != nil || code != exit.Ok {
		return startFailed(s.id.Agent, err, code, s.created, "")
	}
	fmt.Fprintf(os.Stdout, "started %s for thread %s (session %d, %s)\n", s.id.Agent, s.msg.Thread, s.session, s.cwd)
	return exit.Ok, nil
}

// withoutLinear is what a start does when a Linear step still failed
// after its tries: the agent starts anyway. Once the ticket is claimed it
// keeps it and is told what is missing; before that (the ticket not
// created, or not claimed, Linear unavailable or the claim held by another
// agent) the session runs without its ticket, the agent and the thread are
// told, and the next session or `thread end` catches up.
func (s *threadStart) withoutLinear(cause error) error {
	fmt.Fprintf(os.Stderr, "fleet: a Linear step failed, the thread agent starts anyway: %v\n", cause)
	log := linearLog{scope: s.id.Scope, thread: s.msg.Thread, agent: s.id.Agent}
	log.fleetError(cause)
	class := linearClass(cause)
	if s.claimed {
		missing := ""
		if !s.commented {
			missing = fmt.Sprintf(" the `Session %d started` comment is missing, and", s.session)
		}
		s.linearNote = fmt.Sprintf("Linear failed after your ticket was claimed (%s), and the Linear steps after it were "+
			"skipped:%s the earlier sessions' summaries are not below; read them on %s with `atb linear query` when "+
			"you need them.", class, missing, s.ticket.Identifier)
		return nil
	}
	ticket := s.ticket.Identifier
	s.id.Issue, s.ticket = "", atb.Issue{}
	if err := setIssue(s.conn, s.id.Agent, ""); err != nil {
		return err
	}
	why := fmt.Sprintf("Linear was unavailable when this session started (%s)", class)
	line := fmt.Sprintf("Linear is unavailable (%s); this session runs without its ticket.", class)
	if atb.Held(cause) {
		why = fmt.Sprintf("Another agent holds the claim on the thread ticket %s (`atb linear claim` exit 3)", ticket)
		line = fmt.Sprintf("The thread ticket %s is claimed by someone else; this session runs without it.", ticket)
	}
	s.linearNote = why + ", so this session runs without its thread ticket: FLEET_ISSUE is empty, and `fleet thread " +
		"set-project`, `fleet thread relate` and the relation `fleet job start` makes are off. Do not claim the " +
		"ticket yourself: `fleet thread end` writes your summary to the thread's ticket (creating it when there is " +
		"none) if Linear answers by then, and the next session claims it."
	s.notify(line)
	return nil
}

// notify posts one line to the thread, as far as it can: a missing socket
// or a failed post is said on stderr and stops nothing.
func (s *threadStart) notify(line string) {
	if s.cfg.FednetSocket == "" {
		fmt.Fprintf(os.Stderr, "note: fednet.socket is not configured, so the thread was not told: %s\n", line)
		return
	}
	if err := fednet.Post(s.cfg.FednetSocket, s.msg.Thread, line); err != nil {
		fmt.Fprintf(os.Stderr, "note: the thread was not told (%v): %s\n", err, line)
		return
	}
	fmt.Fprintf(os.Stdout, "posted to the thread: %s\n", line)
}

// notHere is what a start does when the thread's directory is not on this
// machine: nothing is reserved or started, and the thread is told. What
// an agent or watch sent gets an error instead.
func (s *threadStart) notHere() error {
	what := "the repo " + strings.TrimPrefix(s.mapping, s.cfg.Channels.RepoPrefix)
	if strings.HasPrefix(s.mapping, s.cfg.Channels.InitiativePrefix) {
		what = "the repo of " + s.mapping
	}
	if s.msg.fromFleet() {
		return exit.Environmentf("%s is not checked out at %s on this machine", what, s.cwd)
	}
	fmt.Fprintf(os.Stderr, "fleet: no checkout at %s, no thread agent started\n", s.cwd)
	return s.tell(fmt.Sprintf("%s is not checked out on this machine (%s), so no agent was started for this thread.",
		capitalize(what), s.cwd), "the checkout is missing")
}

// promptBroken is what a start does when the thread prompt cannot be put
// together: nothing more is reserved or started, the thread is told and
// the message dropped (`dropped`). What an agent or watch sent gets the
// error instead, as does any other error.
func (s *threadStart) promptBroken(err error) (code exit.Code, dropped bool, _ error) {
	var broken *promptError
	if !errors.As(err, &broken) || s.msg.fromFleet() {
		return 0, false, err
	}
	fmt.Fprintf(os.Stderr, "fleet: %v; no thread agent started\n", err)
	return exit.Ok, true, s.tell(fmt.Sprintf("fleet's prompt config is broken (%v), so no agent was started for this "+
		"thread; once it is fixed, please post again.", err), "the prompt config is broken")
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
// created. A thread prompt that cannot be put together is told to the
// thread, the message dropped, as at a start.
func resumeThreadStart(h *herdr.Herdr, conn *sql.DB, scope string, cfg *config.Scope, msg inboundMessage, name string) (code exit.Code, dropped bool, err error) {
	fmt.Fprintf(os.Stderr, "note: %s is still starting from an earlier run; finishing that start\n", name)
	home, err := Home()
	if err != nil {
		return 0, false, err
	}
	known, err := threadByKey(conn, msg.Thread)
	if err != nil {
		return 0, false, err
	}
	if known == nil {
		return 0, false, exit.Environmentf("the ledger has %s but no row for thread %s", name, msg.Thread)
	}
	// The row's issue, not the thread's ticket: a session that started
	// without its ticket has none.
	var pane, issue string
	if err := conn.QueryRow("SELECT pane_id, issue FROM agents WHERE name = ?1 AND state != 'ended'", name).Scan(&pane, &issue); err != nil {
		return 0, false, exit.Database(err)
	}
	s := &threadStart{h: h, conn: conn, cfg: cfg, msg: msg, known: known, session: known.Sessions, home: home,
		id: &identity.Identity{Agent: name, Role: identity.Thread, Scope: scope, Thread: msg.Thread, Issue: issue}}
	if issue != "" {
		s.ticket = atb.Issue{Identifier: known.Ticket, URL: known.TicketURL}
	}
	if err := s.belong(); err != nil {
		return 0, false, err
	}
	if s.text, err = loadPrompt(cfg, "thread"); err != nil {
		return s.promptBroken(err)
	}
	if issue != "" && known.Sessions > 1 {
		if s.notes, err = summaries(issue); err != nil {
			return 0, false, err
		}
	}
	if err := ResumeAgent(h, name, pane, s.cwd); err != nil {
		return 0, false, err
	}
	text, err := WithHeader(s.msg.sender(), s.firstMessage())
	if err != nil {
		return 0, false, err
	}
	if code, err = Deliver(h, name, text); err != nil || code != exit.Ok {
		return code, false, err
	}
	if _, err := conn.Exec("UPDATE agents SET state = 'active' WHERE name = ?1 AND state = 'starting'", name); err != nil {
		return 0, false, exit.Database(err)
	}
	return exit.Ok, false, nil
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
	text := string(data)
	if strings.TrimSpace(text) == "" {
		return 0, exit.Refusedf("the body file is empty")
	}
	socket, err := socketOf(me.Scope)
	if err != nil {
		return 0, err
	}
	thread, err := liveThreadOf(me)
	if err != nil {
		return 0, err
	}
	return handBack(fednet.Relay(socket, thread, text, args.Attach))
}

// liveThreadOf is the thread the caller's live row records, which must
// be the caller's FLEET_THREAD.
func liveThreadOf(me *identity.Identity) (string, error) {
	conn, err := db.Open(me.Scope)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	var thread string
	err = conn.QueryRow("SELECT thread FROM agents WHERE name = ?1 AND role = 'thread' AND state != 'ended'", me.Agent).Scan(&thread)
	if errors.Is(err, sql.ErrNoRows) {
		return "", exit.Refusedf("the ledger has no live thread agent %s", me.Agent)
	}
	if err != nil {
		return "", exit.Database(err)
	}
	if thread != me.Thread {
		return "", exit.Refusedf("the ledger records thread %s for %s, not FLEET_THREAD %s", thread, me.Agent, me.Thread)
	}
	return thread, nil
}

// handBack prints what fednet printed and returns its exit code as the
// command's.
func handBack(stdout, stderr []byte, code int, err error) (exit.Code, error) {
	_, _ = os.Stdout.Write(stdout)
	_, _ = os.Stderr.Write(stderr)
	if err != nil {
		return 0, err
	}
	return exit.Code(code), nil
}

// socketOf is the scope's fednet socket; none is a refusal.
func socketOf(scope string) (string, error) {
	cfg, err := config.LoadScope(scope)
	if err != nil {
		return "", err
	}
	if cfg.FednetSocket == "" {
		return "", exit.Refusedf("fednet.socket is not configured for scope %s, so fleet cannot post to the thread", scope)
	}
	return cfg.FednetSocket, nil
}

// ThreadProgressArgs are the arguments of `thread progress`.
type ThreadProgressArgs struct {
	// Title is the card's status line; Items its items, each
	// `<text>:<doing|done|error>`.
	Title *string
	Items []string
	// Done completes the card instead.
	Done bool
}

// itemStates are the states an item of the card can be in.
var itemStates = map[string]bool{"doing": true, "done": true, "error": true}

// maxCardItems is what a Slack card holds.
const maxCardItems = 50

// checkCard refuses a card fleet can see is wrong before fednet gets it:
// no title without --done, more items than a card holds, an item without
// a state or with an unknown one.
func checkCard(args ThreadProgressArgs) error {
	if !args.Done && (args.Title == nil || strings.TrimSpace(*args.Title) == "") {
		return exit.Refusedf("--title is required (the card's status now), or --done to complete the card")
	}
	if len(args.Items) > maxCardItems {
		return exit.Refusedf("%d items; a card holds at most %d", len(args.Items), maxCardItems)
	}
	for _, item := range args.Items {
		i := strings.LastIndex(item, ":")
		if i < 0 || strings.TrimSpace(item[:i]) == "" || !itemStates[item[i+1:]] {
			return exit.Refusedf("--item %q is not <text>:<state> with the state doing, done or error", item)
		}
	}
	return nil
}

// ThreadProgress runs `thread progress`: the whole card, or --done, to
// the thread the caller's live row records; fednet's output and exit
// code are handed back as they are.
func ThreadProgress(args ThreadProgressArgs) (exit.Code, error) {
	me, err := threadCaller()
	if err != nil {
		return 0, err
	}
	if err := checkCard(args); err != nil {
		return 0, err
	}
	socket, err := socketOf(me.Scope)
	if err != nil {
		return 0, err
	}
	thread, err := liveThreadOf(me)
	if err != nil {
		return 0, err
	}
	title := ""
	if args.Title != nil {
		title = strings.TrimSpace(*args.Title)
	}
	return handBack(fednet.Progress(socket, thread, title, args.Items, args.Done))
}

// ThreadEndArgs are the arguments of `thread end`.
type ThreadEndArgs struct {
	// SummaryFile is the session's summary.
	SummaryFile string
	// Force finishes the local cleanup even when a Linear step keeps
	// failing; the steps not done are printed.
	Force bool
	// AskedToEnd ends the session even while the thread waits on a
	// person or a job: only when the people in the thread asked for it.
	AskedToEnd bool
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
// the release, the closing line, the row) are recorded under
// `thread-end:<row id>`, so a run after a partial failure skips what was
// done; the tab close is always attempted last, since the tab is where
// the caller runs. The thread and its ticket come from the caller's
// newest row and the thread's row, not from the environment.
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
	cfg, err := config.LoadScope(me.Scope)
	if err != nil {
		return 0, err
	}
	conn, err := db.Open(me.Scope)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	e, err := newThreadEnding(conn, me.Agent, args.Force)
	if err != nil {
		return 0, err
	}
	if !args.AskedToEnd {
		if err := stillWaiting(conn, e.thread); err != nil {
			return 0, err
		}
	}
	if err := e.linearSteps(me, cfg.LinearTeam, string(summary)); err != nil {
		return 0, err
	}
	// The closing line goes before the row ends, after Linear: it is what
	// the thread sees of the ending, and the hub completes an open
	// progress card before it.
	if err := e.step("footer", func() error {
		if cfg.FednetSocket == "" {
			fmt.Fprintf(os.Stderr, "note: fednet.socket is not configured for scope %s; no closing line posted\n", me.Scope)
			return nil
		}
		return fednet.Footer(cfg.FednetSocket, e.thread, e.footer)
	}, func(error) { e.unposted = true }); err != nil {
		return 0, err
	}
	if err := runStep(conn, e.key, "end-row", func() error { return endRow(conn, me.Agent) }); err != nil {
		return 0, err
	}
	fmt.Fprintf(os.Stdout, "ended session %d of thread %s\n", e.session, e.thread)
	if len(e.missed) > 0 {
		fmt.Fprintf(os.Stdout, "Linear steps not done, finish them by hand:\n")
		for _, m := range e.missed {
			fmt.Fprintf(os.Stdout, "  - %s\n", m)
		}
	}
	if e.unposted {
		fmt.Fprintf(os.Stdout, "the closing line was not posted to the thread\n")
	}
	return exit.Ok, closeOwnTab(h, conn, me.Agent)
}

// newThreadEnding reads what the ending acts on: the agent's newest row
// (its step key and thread) and the thread's row (the session and the
// ticket the closing line links).
func newThreadEnding(conn *sql.DB, agent string, force bool) (*threadEnding, error) {
	e := &threadEnding{conn: conn, force: force, footer: endedLine}
	var rowID int64
	if err := conn.QueryRow("SELECT id, thread, issue FROM agents WHERE name = ?1 ORDER BY id DESC LIMIT 1", agent).Scan(&rowID, &e.thread, &e.issue); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, exit.Refusedf("the ledger has no row for %s", agent)
		}
		return nil, exit.Database(err)
	}
	if e.thread == "" {
		return nil, exit.Refusedf("the ledger records no thread for %s", agent)
	}
	e.key = fmt.Sprintf("thread-end:%d", rowID)
	row, err := threadByKey(conn, e.thread)
	if err != nil {
		return nil, err
	}
	if row != nil {
		e.session = row.Sessions
		e.ticket, e.ticketURL = row.Ticket, row.TicketURL
		e.footer = closingLine(endedLine, row.Ticket, row.TicketURL)
	}
	return e, nil
}

// stillWaiting refuses an ending while the thread waits: a question in
// it is pending (the thread agent's own or a lead's), or a job whose home
// thread it is is open. The answer and the lead's messages reach the
// session that is live, so ending it only makes the next one start cold.
func stillWaiting(conn *sql.DB, thread string) error {
	rows, err := conn.Query("SELECT 0, id, 'question from ' || asked_by || ', pending: ', text FROM questions "+
		"WHERE thread = ?1 AND state = 'pending' "+
		"UNION ALL SELECT 1, id, 'job ' || job || ', open, reporting to this thread', '' FROM jobs "+
		"WHERE home_thread = ?1 AND state = 'open' ORDER BY 1, 2", thread)
	if err != nil {
		return exit.Database(err)
	}
	defer rows.Close()
	var waits []string
	for rows.Next() {
		var kind, id int64
		var what, text string
		if err := rows.Scan(&kind, &id, &what, &text); err != nil {
			return exit.Database(err)
		}
		waits = append(waits, what+WorkOrderTitle(text))
	}
	if err := rows.Err(); err != nil {
		return exit.Database(err)
	}
	if len(waits) == 0 {
		return nil
	}
	return exit.Refusedf("thread %s still waits, so this session stays:\n  - %s\n"+
		"A person's answer and the lead's messages reach you in this session; staying idle costs nothing. "+
		"End anyway with --asked-to-end only when the people in the thread asked you to end, or fleet watch is reclaiming this session.",
		thread, strings.Join(waits, "\n  - "))
}

// endedLine is what the closing line of a session's ending says.
const endedLine = "会话已结束"

// closingLine is a closing line: `<text> · <ticket>`, the ticket linked,
// or the text alone without one.
func closingLine(text, ticket, url string) string {
	switch {
	case ticket == "":
		return text
	case url == "":
		return text + " · " + ticket
	}
	return fmt.Sprintf("%s · [%s](%s)", text, ticket, url)
}

// threadEnding is one `thread end`: its step key, the thread, the session
// and the closing line, and with --force the Linear steps skipped, each as
// the command to finish it by hand, and whether the closing line was
// skipped.
type threadEnding struct {
	conn      *sql.DB
	key       string
	thread    string
	session   int64
	ticket    string
	ticketURL string
	issue     string // the agent's row's: empty for a session without its ticket
	footer    string
	force     bool
	missed    []string
	unposted  bool
}

// linearSteps are the ticket's steps, with a ticket: the summary comment,
// then the release in the agent's name. A session without its ticket in a
// scope with a Linear team (`team`) catches up instead.
func (e *threadEnding) linearSteps(me *identity.Identity, team, summary string) error {
	agent, issue := me.Agent, me.Issue
	if issue == "" && team != "" {
		return e.catchUp(me, team, summary)
	}
	if issue == "" {
		return nil
	}
	file, remove, err := tempFile(fmt.Sprintf("Session %d ended\n\n%s", e.session, strings.TrimSpace(summary)+"\n"))
	if err != nil {
		return err
	}
	defer remove()
	if err := e.linear("comment", func() error { return atb.Comment(issue, file) },
		fmt.Sprintf("atb linear comment %s --body-file <the summary, headed `Session %d ended`>", issue, e.session)); err != nil {
		return err
	}
	return e.linear("release", func() error { return atb.Release(issue, agent, false) },
		fmt.Sprintf("atb linear release %s --agent %s --reason done --done", issue, agent))
}

// catchUp is the Linear part of ending a session that ran without its
// ticket (FLEET_ISSUE empty in a scope with a Linear team: Linear failed
// at its start). The summary is kept first (keepSummary), then written to
// the thread's ticket, created when the thread has none, with any summary
// an earlier session kept; each step tried as at a start and recorded.
// The ticket is not claimed, so nothing is released; the next session
// claims it. When Linear fails, the summaries stay kept for the next
// session that reaches Linear, and the ending goes on; only keeping the
// summary can fail it.
func (e *threadEnding) catchUp(me *identity.Identity, team, summary string) error {
	if err := runStep(e.conn, e.key, "keep", func() error {
		return keepSummary(me.Scope, e.thread, e.session, fmt.Sprintf("Session %d ended\n\n%s", e.session, strings.TrimSpace(summary)+"\n"))
	}); err != nil {
		return err
	}
	log := newLinearLog(me.Scope, e.thread, me.Agent)
	err := runStep(e.conn, e.key, "create", func() error { return e.createTicket(log, team) })
	if err == nil && e.ticket == "" {
		// Created by an earlier run of this ending.
		var row *threadRow
		if row, err = threadByKey(e.conn, e.thread); err == nil && row != nil {
			e.ticket, e.ticketURL = row.Ticket, row.TicketURL
		}
	}
	if err == nil {
		e.footer = closingLine(endedLine, e.ticket, e.ticketURL)
		err = log.writeKept(e.ticket)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet: %v\n", err)
		log.fleetError(err)
		fmt.Fprintf(os.Stdout, "this session ran without its ticket, and Linear did not take its summary (%s); it is kept, "+
			"and the next session that reaches Linear writes it to the thread's ticket\n", linearClass(err))
		return nil
	}
	fmt.Fprintf(os.Stdout, "this session ran without its ticket; its summary is now on %s\n", e.ticket)
	return nil
}

// createTicket creates the thread's ticket for catchUp when the thread has
// none, titled after the latest message a person posted there, and
// records it on the thread's row.
func (e *threadEnding) createTicket(log linearLog, team string) error {
	if e.ticket != "" {
		return nil
	}
	var channel, last string
	if err := e.conn.QueryRow("SELECT channel, last_text FROM threads WHERE thread = ?1", e.thread).Scan(&channel, &last); err != nil {
		return exit.Database(err)
	}
	title := WorkOrderTitle(WithoutLeadingMentions(last))
	if title == "" {
		title = "Thread " + e.thread
	}
	file, remove, err := tempFile(fmt.Sprintf("channel: %s\nthread: %s\n\nCreated by `fleet thread end` at the end of "+
		"session %d, which ran without its ticket because Linear failed when it started.\n", channel, e.thread, e.session))
	if err != nil {
		return err
	}
	defer remove()
	var issue atb.Issue
	if err := log.try(func() (err error) {
		issue, err = atb.CreateThreadTicket(team, title, file)
		return err
	}); err != nil {
		return err
	}
	if _, err := e.conn.Exec("UPDATE threads SET ticket = ?1, ticket_url = ?2 WHERE thread = ?3",
		issue.Identifier, issue.URL, e.thread); err != nil {
		return exit.Database(err)
	}
	e.ticket, e.ticketURL = issue.Identifier, issue.URL
	return nil
}

// step runs a step through runStep; a failure is returned, or with
// --force printed and given to `skipped`.
func (e *threadEnding) step(step string, do func() error, skipped func(error)) error {
	err := runStep(e.conn, e.key, step, do)
	if err == nil || !e.force {
		return err
	}
	fmt.Fprintf(os.Stderr, "fleet: %v\n", err)
	skipped(err)
	return nil
}

// linear runs a Linear step; with --force a failure lists the command to
// finish it by hand.
func (e *threadEnding) linear(step string, do func() error, byHand string) error {
	return e.step(step, do, func(error) { e.missed = append(e.missed, byHand) })
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

// `fleet thread handoff`: a thread agent hands a piece of work to the
// channel of a repo or initiative, its own included, where a thread agent
// of that channel takes it on in a thread fleet opens there.

package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/Luolc/agent-fleet/internal/atb"
	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/fednet"
	"github.com/Luolc/agent-fleet/internal/herdr"
	"github.com/Luolc/agent-fleet/internal/identity"
)

// ThreadHandoffAbout and ThreadHandoffLongAbout are the help texts of
// `thread handoff`.
const (
	ThreadHandoffAbout     = "Hand work off to a repo's or initiative's channel, your own included, where a new thread agent takes it on (thread agents only)"
	ThreadHandoffLongAbout = "Hand work off to a repo's or initiative's channel, your own included, where a new thread agent takes it on (thread agents only).\n\n" +
		"Only a thread agent hands work off: a lead tells its home thread's agent, a worker its " +
		"lead. What goes over is the task in --file, nothing else: a job of yours keeps running " +
		"where it is. The target is a channel fleet already serves: --repo <R> is " +
		"`<repo prefix><R>`, --initiative <I> `<initiative prefix><I>` (the prefixes are the " +
		"scope's `channels`), --channel <C> the channel named C; naming your own thread's channel opens a new thread beside yours, which is " +
		"how a long thread is continued afresh. fleet takes the channel's id " +
		"as `ask-thread` does for a home thread: from the newest thread of it this scope has seen, " +
		"or else from fednet's routes (`fednet client routes -json`); it opens no channel and " +
		"changes no route.\n\n" +
		"When fednet's routes give the channel a machine other than this one (`fednet client " +
		"whoami`, fednet v0.9.0 or later), the work goes to that machine: fleet opens the thread " +
		"with `fednet client open-thread -owner route -from-thread <your thread>`, so it is that " +
		"machine's and fednet counts the hops from your thread, its first message fleet's line, " +
		"the task and a last line `— <you> · <your thread>`; fednet delivers that message to the " +
		"other machine (trigger `handoff`), whose fleet starts the thread agent there. At most " +
		"4000 characters go in that message. fleet then posts the line in your thread and the " +
		"mention, if any; it starts no agent here, records the thread only with the handoff, and " +
		"relates no tickets (the new one is in the other machine's ledger). The prefixes and " +
		"checkouts are the other machine's to check, not this one's.\n\n" +
		"The steps: fleet records the handoff (table `handoffs`), opens a thread in the target " +
		"channel with `fednet client open-thread`, its first message fleet's one line naming your " +
		"channel (`<#<id>>`, which Slack shows as a link), your thread, the task's title (as `fleet job start --help` says) and " +
		"you, and records it as a thread of that channel. Then it posts one line to your thread, " +
		"`已转到 <#<id>> 的[新线程](<link>)`, so your thread shows where the work went before " +
		"the new agent is up. Then it starts a thread agent there, as `fleet inbox` starts one " +
		"(a thread ticket created and claimed for it, when the scope has a Linear team), whose " +
		"first message, headed `[FROM: <you>]`, is `[HANDOFF from <you> in thread <your " +
		"thread>]` and the task: an agent's words, never recorded as the latest message a " +
		"person posted there. Then, with --mention, it posts " +
		"`你要求的转交在这里` in the new thread, mentioning each user given (fednet puts the " +
		"mentions first, so Slack notifies them and the notice leads into the new thread); and " +
		"it relates the two threads' " +
		"tickets (`atb linear relate`), taking both from the ledger; your ticket is not otherwise " +
		"touched. A thread is linked as `<fednet.slack_url>/archives/<channel>/p<ts without its " +
		"dot>`; without the scope's `fednet.slack_url`, neither line links a thread or shows its " +
		"key (`一个线程`). While a thread has no ticket the two are not related, said on stderr, and the " +
		"handoff stays unfinished: running it again once both have one relates them.\n\n" +
		"Each step done is recorded (table `steps`, `handoff:<id>`), so running it again with the " +
		"same task to the same target continues where it stopped and never opens a second thread; " +
		"once every step is done, it does nothing. The mention is a step too (`mention`), posted " +
		"at most once: the run that posts it uses its own --mention, a later run's --mention is " +
		"ignored, and a run without --mention skips the step, leaving it to a later run that has " +
		"one while the handoff is unfinished. A run killed between opening the thread and " +
		"recording it leaves that thread in Slack with only fleet's line, and the next run opens " +
		"another.\n\n" +
		"Exit: 0 when the new thread's agent has the task (or had it already); 1 when the caller " +
		"is not a live thread agent, a --mention is not a Slack user id (U or W, then capitals " +
		"and digits), the file is unreadable or empty, the target is not one directory name " +
		"(for --channel, not a Slack channel name), is for this machine and has a name " +
		"with neither of the scope's prefixes, the task is too long for another machine, " +
		"is not checked out on this machine, or fleet knows no id of " +
		"its channel, or the scope has no fednet socket (each before " +
		"anything is recorded); 2/3/4 as `send` for the delivery; 3 when the new thread agent " +
		"stops at an unknown screen; 5 when fednet's routes or `whoami` cannot be read (before anything is " +
		"recorded), when fednet refuses (its relay list, the hop limit) or fails to open the thread or to " +
		"post, or refuses a mention (a user not on the hub's user list, or a fednet too old for -mention: the " +
		"line in your thread is posted by then), or herdr, atb or the database fails. After a failure, run it " +
		"again to finish, without the refused --mention."
)

// ThreadHandoffArgs are the arguments of `thread handoff`: one of Repo,
// Initiative and Channel, the task's file, and the Slack user ids to
// mention in the new thread.
type ThreadHandoffArgs struct {
	Repo, Initiative, Channel, File string
	Mentions                        []string
}

// slackUser is the shape of a Slack user id.
var slackUser = regexp.MustCompile(`^[UW][A-Z0-9]+$`)

// slackChannel is the shape of a Slack channel's name, without the `#`.
var slackChannel = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// handoffRoute takes the task to the new thread; tests replace it.
var handoffRoute = toThread

// ThreadHandoff runs `thread handoff`.
func ThreadHandoff(h *herdr.Herdr, args ThreadHandoffArgs) (exit.Code, error) {
	me, err := identity.FromEnv()
	if err != nil {
		return 0, err
	}
	if err := handoffCaller(me); err != nil {
		return 0, err
	}
	for _, m := range args.Mentions {
		if !slackUser.MatchString(m) {
			return 0, exit.Refusedf("--mention %q is not a Slack user id, such as U012AB3CD: take it from the `user` of the person's message", m)
		}
	}
	task, err := readTask(args.File)
	if err != nil {
		return 0, err
	}
	cfg, err := config.LoadScope(me.Scope)
	if err != nil {
		return 0, err
	}
	mapping, err := handoffMapping(cfg.Channels, args)
	if err != nil {
		return 0, err
	}
	if cfg.FednetSocket == "" {
		return 0, exit.Refusedf("fednet.socket is not configured for scope %s, so fleet cannot open a thread", me.Scope)
	}
	if _, err := liveThreadOf(me); err != nil {
		return 0, err
	}
	conn, err := db.Open(me.Scope)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	from, err := threadByKey(conn, me.Thread)
	if err != nil {
		return 0, err
	}
	if from == nil {
		return 0, exit.Refusedf("the ledger has no thread %s", me.Thread)
	}
	ho := &handoff{conn: conn, cfg: cfg, me: me, from: from, mapping: mapping, task: task, mentions: args.Mentions}
	if err := ho.find(); err != nil {
		return 0, err
	}
	if ho.done {
		fmt.Fprintf(os.Stdout, "this task was already handed off to thread %s in #%s; nothing done\n", ho.to, mapping)
		return exit.Ok, nil
	}
	if err := ho.begin(); err != nil {
		return 0, err
	}
	if !ho.remote {
		defer markThread(conn, cfg.FednetSocket, ho.to)
	}
	return ho.finish(h)
}

// handoffCaller refuses every caller but a thread agent, naming the route
// its role has instead.
func handoffCaller(me *identity.Identity) error {
	switch {
	case me.Role == identity.Lead:
		return exit.Refusedf("a lead does not hand work off; tell your home thread's agent, which decides " +
			"(`fleet ask-thread --card <file>`)")
	case me.Role == identity.Worker && me.Parent == watchSender:
		return exit.Refusedf("a worker does not hand work off; fleet's watch started you, so write it to the " +
			"question file your task names")
	case me.Role == identity.Worker:
		return exit.Refusedf("a worker does not hand work off; tell your lead with `fleet send %s`", me.Parent)
	case me.Role != identity.Thread:
		return exit.Refusedf("only a thread agent hands work off")
	case me.Thread == "":
		return exit.Refusedf("FLEET_THREAD is not set: only a thread agent started by `fleet inbox` hands work off")
	}
	return nil
}

// handoffMapping is the target's mapping, `<repo prefix><R>`,
// `<initiative prefix><I>` or the channel's name.
func handoffMapping(ch config.Channels, args ThreadHandoffArgs) (string, error) {
	if args.Channel != "" {
		if !slackChannel.MatchString(args.Channel) {
			return "", exit.Refusedf("--channel %q is not a Slack channel's name, such as repo-example: lower-case "+
				"letters, digits, - and _, without the #", args.Channel)
		}
		return args.Channel, nil
	}
	if args.Repo != "" {
		return ch.RepoPrefix + args.Repo, CheckRepo(args.Repo)
	}
	return ch.InitiativePrefix + args.Initiative, CheckRepo(args.Initiative)
}

// routeOf reads fednet's routes and tells whether the target's new
// threads go to another machine: its route gives a machine that is not
// this one, as `fednet client whoami` names it. A route without a machine,
// or a channel the routes do not name, is this machine's. Routes fednet
// cannot give, or a whoami that fails, are an environment failure, since
// then fleet cannot tell where the channel's threads go.
func routeOf(socket, mapping string) (routes map[string]fednet.Route, remote bool, err error) {
	routes, err = fednet.Channels(socket)
	if err != nil {
		return nil, false, exit.Environmentf("%v; fleet cannot tell which machine takes #%s, so nothing is recorded", err, mapping)
	}
	machine := routes[mapping].Machine
	if machine == "" {
		return routes, false, nil
	}
	me, err := fednet.Whoami(socket)
	if err != nil {
		return nil, false, exit.Environmentf("%v; nothing is recorded", err)
	}
	return routes, machine != me, nil
}

// maxRemoteText is the most fednet's `open-thread` takes as the first
// message, in characters: all a handoff to another machine carries.
const maxRemoteText = 4000

// relaySignature is the last line fleet adds to what it sends another
// machine, naming the agent and its thread: labels the caller gives
// itself, which the receiving fleet only shows (relaySigned).
func relaySignature(agent, thread string) string {
	return "— " + agent + " · " + thread
}

// threadKey is the shape of a Slack thread key, CHANNEL/TS.
var threadKey = regexp.MustCompile(`^[A-Z0-9]+/[0-9]+\.[0-9]+$`)

// signatureLine is relaySignature's shape.
var signatureLine = regexp.MustCompile(`^— ([a-z0-9][a-z0-9-]*) · ([A-Z0-9]+/[0-9]+\.[0-9]+)$`)

// relaySigned is the agent and thread that the last line of `text` names
// as relaySignature writes it, or "" for both when it is not that line.
func relaySigned(text string) (agent, thread string) {
	text = strings.TrimRight(text, " \t\r\n")
	last := text[strings.LastIndex(text, "\n")+1:]
	m := signatureLine.FindStringSubmatch(strings.TrimSpace(last))
	if m == nil {
		return "", ""
	}
	return m[1], m[2]
}

// readTask is the task file's text, its end trimmed.
func readTask(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", exit.Refusedf("cannot read %s: %v", path, err)
	}
	text := strings.TrimRight(string(data), " \t\r\n")
	if strings.TrimSpace(text) == "" {
		return "", exit.Refusedf("the task file is empty")
	}
	return text, nil
}

// isDir is whether `path` is a directory.
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// handoff is one `thread handoff`: the caller and its thread, the
// target's mapping, fednet's routes and whether the target's threads go
// to another machine (`remote`), the task, the users to mention, and the
// ledger's row of it once recorded (`id`), with the thread opened for it
// (`to`) and whether every step is done.
type handoff struct {
	conn     *sql.DB
	cfg      *config.Scope
	me       *identity.Identity
	from     *threadRow
	mapping  string
	routes   map[string]fednet.Route
	remote   bool
	task     string
	mentions []string
	id       int64
	to       string
	done     bool
}

// begin tells whether the target's threads go to another machine: by its
// route while no thread is open for the handoff, else by whether this
// ledger recorded the thread (a thread for another machine is not
// recorded). A target of this machine is refused when its name the
// scope's prefixes do not serve or it is not checked out here. Then it
// opens the thread unless an earlier run did.
func (ho *handoff) begin() error {
	opening := ho.to == ""
	if opening {
		routes, remote, err := routeOf(ho.cfg.FednetSocket, ho.mapping)
		if err != nil {
			return err
		}
		ho.routes, ho.remote = routes, remote
	} else {
		to, err := threadByKey(ho.conn, ho.to)
		if err != nil {
			return err
		}
		ho.remote = to == nil
	}
	if err := ho.here(); err != nil {
		return err
	}
	if !opening {
		return nil
	}
	if err := ho.open(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "opened thread %s in #%s\n", ho.to, ho.mapping)
	return nil
}

// here refuses a target of this machine that the scope does not serve or
// that is not checked out here; a target of another machine is that
// machine's to check.
func (ho *handoff) here() error {
	if ho.remote {
		return nil
	}
	if ChannelMapping(ho.cfg.Channels, ho.mapping, false) == "" {
		return exit.Refusedf("fleet does not serve #%s on this machine: its name has neither of the scope's prefixes; "+
			"nothing is recorded", ho.mapping)
	}
	if dir := MappingDir(ho.cfg, ho.mapping); !isDir(dir) {
		return exit.Refusedf("#%s runs its agents in %s, which is not on this machine; nothing is recorded", ho.mapping, dir)
	}
	return nil
}

// find reads the newest handoff of this task from this thread agent to
// this target, if an earlier run recorded one.
func (ho *handoff) find() error {
	err := ho.conn.QueryRow("SELECT id, to_thread, done_at IS NOT NULL FROM handoffs WHERE from_thread = ?1 AND by = ?2 "+
		"AND mapping = ?3 AND task = ?4 ORDER BY id DESC LIMIT 1", ho.me.Thread, ho.me.Agent, ho.mapping, ho.task).
		Scan(&ho.id, &ho.to, &ho.done)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return dbErr(err)
}

// open finds the target channel's id, records the handoff unless an
// earlier run did, opens the thread, and records it with the handoff and
// as a thread of the target, in one transaction. A channel fleet knows no
// id of is refused before anything is recorded. For another machine the
// thread is opened as its route's (`open-thread -owner route`, from the
// caller's thread, so fednet counts the hops), its first message fleet's
// line, the task and relaySignature, which is all the other machine
// gets; it is refused when longer than fednet takes, and recorded only
// with the handoff, never as a thread of this scope.
func (ho *handoff) open() error {
	_, channel, err := channelOf(ho.conn, ho.cfg, ho.routes, ho.mapping)
	if err != nil {
		return err
	}
	if channel == "" {
		return exit.Refusedf("fleet knows no id of #%s: no thread of it has reached this scope and fednet's routes "+
			"do not name it; fleet opens no channel and changes no route, so ask the people to have it routed to this "+
			"machine; nothing is recorded", ho.mapping)
	}
	line := ho.openingLine()
	text := line
	if ho.remote {
		text = line + "\n\n" + ho.task + "\n\n" + relaySignature(ho.me.Agent, ho.me.Thread)
		if n := utf8.RuneCountInString(text); n > maxRemoteText {
			return exit.Refusedf("#%s is routed to another machine, which gets the task in the new thread's first "+
				"message: with fleet's lines it is %d characters, over fednet's %d; shorten the task or point to a "+
				"file in a repo; nothing is recorded", ho.mapping, n, maxRemoteText)
		}
	}
	if ho.id == 0 {
		res, err := ho.conn.Exec("INSERT INTO handoffs (from_thread, by, mapping, task, created_at) VALUES (?1, ?2, ?3, ?4, ?5)",
			ho.me.Thread, ho.me.Agent, ho.mapping, ho.task, db.Now())
		if err != nil {
			return exit.Database(err)
		}
		if ho.id, err = res.LastInsertId(); err != nil {
			return exit.Database(err)
		}
	}
	var thread string
	if ho.remote {
		thread, err = fednet.OpenThreadForRoute(ho.cfg.FednetSocket, channel, ho.me.Thread, text)
	} else {
		thread, err = fednet.OpenThread(ho.cfg.FednetSocket, channel, text)
	}
	var failure *exit.Failure
	if errors.As(err, &failure) {
		return exit.New(failure.Code, failure.Message+"; no thread is recorded, run thread handoff again")
	}
	if err != nil {
		return err
	}
	ho.to = thread
	return reserve(ho.conn, func(querier) error { return nil }, func(q querier) error {
		if !ho.remote {
			if err := insertThread(q, ho.cfg, thread, channel, ho.mapping); err != nil {
				return err
			}
		}
		if _, err := q.Exec("UPDATE handoffs SET to_thread = ?1 WHERE id = ?2", thread, ho.id); err != nil {
			return exit.Database(err)
		}
		return nil
	})
}

// openingLine is fleet's line that opens the new thread: where the work
// comes from, the task's title and the caller.
func (ho *handoff) openingLine() string {
	where := " <#" + ho.from.Channel + "> 的"
	if strings.HasPrefix(ho.from.Channel, "D") {
		where = "私信里的"
	}
	return fmt.Sprintf("从%s转来：%s (%s)", where+threadLink(ho.cfg.SlackURL, ho.me.Thread, "原线程", "一个线程"),
		WorkOrderTitle(ho.task), ho.me.Agent)
}

// finish runs the steps after the thread is open, each recorded under
// `handoff:<id>`: the line in the caller's thread, the task to the new
// thread's agent, the mention in the new thread, and the relation of the
// two tickets; for another machine's thread only the line and the
// mention, since fednet delivered the task with the thread and its ticket
// is in that machine's ledger. The line goes first, so the people in the caller's thread
// see where the work went while the new agent starts, and even when its
// start fails, fednet refuses a mention or Linear fails.
func (ho *handoff) finish(h *herdr.Herdr) (exit.Code, error) {
	key := fmt.Sprintf("handoff:%d", ho.id)
	channel, _, _ := strings.Cut(ho.to, "/")
	line := fmt.Sprintf("已转到 <#%s> 的%s", channel, threadLink(ho.cfg.SlackURL, ho.to, "新线程", "一个新线程"))
	if err := runStep(ho.conn, key, "post", func() error { return fednet.Post(ho.cfg.FednetSocket, ho.me.Thread, line) }); err != nil {
		return 0, again(err)
	}
	if !ho.remote {
		msg := inboundMessage{Thread: ho.to, Text: ho.task, Handoff: ho.me.Thread, By: ho.me.Agent}
		err := runStep(ho.conn, key, "deliver", func() error {
			code, err := handoffRoute(h, ho.conn, ho.me.Scope, ho.cfg, msg)
			if err == nil && code != exit.Ok {
				err = exit.New(code, fmt.Sprintf("the task did not reach the agent of thread %s (exit %d)", ho.to, code))
			}
			return err
		})
		if err != nil {
			return 0, again(err)
		}
	}
	if len(ho.mentions) > 0 {
		err := runStep(ho.conn, key, "mention", func() error {
			_, err := fednet.PostID(ho.cfg.FednetSocket, ho.to, "你要求的转交在这里", ho.mentions...)
			if errors.Is(err, fednet.ErrMention) {
				return exit.Environmentf("%v; run it again without that --mention", err)
			}
			return err
		})
		if err != nil {
			return 0, again(err)
		}
	}
	has := "its agent has the task"
	related := "; its ticket is in the other machine's ledger, so the tickets are not related"
	if ho.remote {
		has = "fednet hands the task to the machine its channel is routed to"
	} else {
		var err error
		if related, err = ho.relate(key); err != nil {
			return 0, again(err)
		}
	}
	if related == "" {
		fmt.Fprintf(os.Stdout, "handed off to thread %s in #%s: its agent has the task, and your thread was told; "+
			"the tickets are not related yet, run thread handoff again once both threads have one\n", ho.to, ho.mapping)
		return exit.Ok, nil
	}
	if _, err := ho.conn.Exec("UPDATE handoffs SET done_at = ?1 WHERE id = ?2", db.Now(), ho.id); err != nil {
		return 0, exit.Database(err)
	}
	fmt.Fprintf(os.Stdout, "handed off to thread %s in #%s: %s, and your thread was told%s\n",
		ho.to, ho.mapping, has, related)
	return exit.Ok, nil
}

// relate is the step that relates the new thread's ticket to the
// caller's thread's, both read from the ledger, and returns what to
// report. A thread without one is said on stderr and the step left
// undone, returning "", so the handoff stays unfinished and a rerun
// relates them.
func (ho *handoff) relate(key string) (string, error) {
	if ho.cfg.LinearTeam == "" {
		return "; thread tickets are off for this scope", nil
	}
	to, err := threadByKey(ho.conn, ho.to)
	if err != nil {
		return "", err
	}
	from, err := threadByKey(ho.conn, ho.me.Thread)
	if err != nil {
		return "", err
	}
	if from == nil || to == nil {
		return "", exit.Environmentf("the ledger lost thread %s or %s", ho.me.Thread, ho.to)
	}
	for _, t := range []*threadRow{from, to} {
		if t.Ticket == "" {
			fmt.Fprintf(os.Stderr, "fleet: the tickets are not related: thread %s has no ticket yet\n", t.Thread)
			return "", nil
		}
	}
	if err := runStep(ho.conn, key, "relate", func() error { return atb.Relate(to.Ticket, from.Ticket) }); err != nil {
		return "", err
	}
	return fmt.Sprintf("; %s is related to %s", to.Ticket, from.Ticket), nil
}

// threadLink is `text` linked to `thread` (`<channel>/<ts>`) in the
// workspace at `slackURL`, in Markdown, or `bare` without a workspace:
// the people click a link, never a thread key.
func threadLink(slackURL, thread, text, bare string) string {
	channel, ts, ok := strings.Cut(thread, "/")
	if slackURL == "" || !ok {
		return bare
	}
	return "[" + text + "](" + slackURL + "/archives/" + channel + "/p" + strings.ReplaceAll(ts, ".", "") + ")"
}

// again is a failed step's error, saying that a rerun finishes the
// handoff.
func again(err error) error {
	var failure *exit.Failure
	if errors.As(err, &failure) {
		return exit.New(failure.Code, failure.Message+"; the steps done are recorded, run thread handoff again to finish")
	}
	return err
}

// `fleet thread handoff`: a thread agent hands a piece of work to the
// channel of another repo or initiative, where a thread agent of that
// channel takes it on in a thread fleet opens there.

package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

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
	ThreadHandoffAbout     = "Hand work off to another repo's or initiative's channel, where a new thread agent takes it on (thread agents only)"
	ThreadHandoffLongAbout = "Hand work off to another repo's or initiative's channel, where a new thread agent takes it on (thread agents only).\n\n" +
		"Only a thread agent hands work off: a lead tells its home thread's agent, a worker its " +
		"lead. What goes over is the task in --file, nothing else: a job of yours keeps running " +
		"where it is. The target is a channel fleet already serves: --repo <R> is " +
		"`<repo prefix><R>`, --initiative <I> `<initiative prefix><I>` (the prefixes are the " +
		"scope's `channels`), and it must not be your own thread's. fleet takes the channel's id " +
		"as `ask-thread` does for a home thread: from the newest thread of it this scope has seen, " +
		"or else from fednet's routes (`fednet client routes -json`); it opens no channel and " +
		"changes no route.\n\n" +
		"The steps: fleet records the handoff (table `handoffs`), opens a thread in the target " +
		"channel with `fednet client open-thread`, its first message fleet's one line naming your " +
		"thread, you and the task's first line, and records it as a thread of that channel. Then " +
		"it starts a thread agent there, as `fleet inbox` starts one (a thread ticket created and " +
		"claimed for it, when the scope has a Linear team), whose first message, headed `[FROM: " +
		"<you>]`, is `[HANDOFF from <you> in thread <your thread>]` and the task: an agent's " +
		"words, never recorded as the latest message a person posted there. Then it posts one " +
		"line to your thread, `已转到 #<channel> 的线程 <key>`, and relates the two threads' " +
		"tickets (`atb linear relate`), taking both from the ledger; your ticket is not otherwise " +
		"touched. While a thread has no ticket the two are not related, said on stderr, and the " +
		"handoff stays unfinished: running it again once both have one relates them.\n\n" +
		"Each step done is recorded (table `steps`, `handoff:<id>`), so running it again with the " +
		"same task to the same target continues where it stopped and never opens a second thread; " +
		"once every step is done, it does nothing. A run killed between opening the thread and " +
		"recording it leaves that thread in Slack with only fleet's line, and the next run opens " +
		"another.\n\n" +
		"Exit: 0 when the new thread's agent has the task (or had it already); 1 when the caller " +
		"is not a live thread agent, the file is unreadable or empty, the target is not one " +
		"directory name, is your own thread's channel, is not checked out on this machine, or " +
		"fleet knows no id of its channel, or the scope has no fednet socket (each before " +
		"anything is recorded); 2/3/4 as `send` for the delivery; 3 when the new thread agent " +
		"stops at an unknown screen; 5 when fednet refuses or fails to open the thread or to " +
		"post, or herdr, atb or the database fails. After a failure, run it again to finish."
)

// ThreadHandoffArgs are the arguments of `thread handoff`: one of Repo
// and Initiative, and the task's file.
type ThreadHandoffArgs struct {
	Repo, Initiative, File string
}

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
	if from.Mapping == mapping {
		return 0, exit.Refusedf("#%s is your own thread's channel: there is nothing to hand off to", mapping)
	}
	if dir := MappingDir(cfg, mapping); !isDir(dir) {
		return 0, exit.Refusedf("#%s runs its agents in %s, which is not on this machine; nothing is recorded", mapping, dir)
	}
	ho := &handoff{conn: conn, cfg: cfg, me: me, from: from, mapping: mapping, task: task}
	if err := ho.find(); err != nil {
		return 0, err
	}
	if ho.done {
		fmt.Fprintf(os.Stdout, "this task was already handed off to thread %s in #%s; nothing done\n", ho.to, mapping)
		return exit.Ok, nil
	}
	if ho.to == "" {
		if err := ho.open(); err != nil {
			return 0, err
		}
		fmt.Fprintf(os.Stdout, "opened thread %s in #%s\n", ho.to, mapping)
	}
	defer markThread(conn, cfg.FednetSocket, ho.to)
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

// handoffMapping is the target's mapping, `<repo prefix><R>` or
// `<initiative prefix><I>`.
func handoffMapping(ch config.Channels, args ThreadHandoffArgs) (string, error) {
	if args.Repo != "" {
		return ch.RepoPrefix + args.Repo, CheckRepo(args.Repo)
	}
	return ch.InitiativePrefix + args.Initiative, CheckRepo(args.Initiative)
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
// target's mapping, the task, and the ledger's row of it once recorded
// (`id`), with the thread opened for it (`to`) and whether every step is
// done.
type handoff struct {
	conn    *sql.DB
	cfg     *config.Scope
	me      *identity.Identity
	from    *threadRow
	mapping string
	task    string
	id      int64
	to      string
	done    bool
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
// id of is refused before anything is recorded.
func (ho *handoff) open() error {
	_, channel, err := channelOf(ho.conn, ho.cfg, ho.mapping)
	if err != nil {
		return err
	}
	if channel == "" {
		return exit.Refusedf("fleet knows no id of #%s: no thread of it has reached this scope and fednet's routes "+
			"do not name it; fleet opens no channel and changes no route, so ask the people to have it routed to this "+
			"machine; nothing is recorded", ho.mapping)
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
	where := "#" + ho.from.Mapping
	if strings.HasPrefix(ho.from.Channel, "D") {
		where = "私信"
	}
	line := fmt.Sprintf("从%s的线程 %s 转来 (%s)：%s", where, ho.me.Thread, ho.me.Agent, WorkOrderTitle(ho.task))
	thread, err := fednet.OpenThread(ho.cfg.FednetSocket, channel, line)
	var failure *exit.Failure
	if errors.As(err, &failure) {
		return exit.New(failure.Code, failure.Message+"; no thread is recorded, run thread handoff again")
	}
	if err != nil {
		return err
	}
	ho.to = thread
	return reserve(ho.conn, func(querier) error { return nil }, func(q querier) error {
		if err := insertThread(q, ho.cfg, thread, channel, ho.mapping); err != nil {
			return err
		}
		if _, err := q.Exec("UPDATE handoffs SET to_thread = ?1 WHERE id = ?2", thread, ho.id); err != nil {
			return exit.Database(err)
		}
		return nil
	})
}

// finish runs the steps after the thread is open, each recorded under
// `handoff:<id>`: the task to the new thread's agent, the line in the
// caller's thread, and the relation of the two tickets. The line goes
// before the relation, so the people see the handoff even while Linear
// fails.
func (ho *handoff) finish(h *herdr.Herdr) (exit.Code, error) {
	key := fmt.Sprintf("handoff:%d", ho.id)
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
	line := fmt.Sprintf("已转到 #%s 的线程 %s", ho.mapping, ho.to)
	if err := runStep(ho.conn, key, "post", func() error { return fednet.Post(ho.cfg.FednetSocket, ho.me.Thread, line) }); err != nil {
		return 0, again(err)
	}
	related, err := ho.relate(key)
	if err != nil {
		return 0, again(err)
	}
	if related == "" {
		fmt.Fprintf(os.Stdout, "handed off to thread %s in #%s: its agent has the task, and your thread was told; "+
			"the tickets are not related yet, run thread handoff again once both threads have one\n", ho.to, ho.mapping)
		return exit.Ok, nil
	}
	if _, err := ho.conn.Exec("UPDATE handoffs SET done_at = ?1 WHERE id = ?2", db.Now(), ho.id); err != nil {
		return 0, exit.Database(err)
	}
	fmt.Fprintf(os.Stdout, "handed off to thread %s in #%s: its agent has the task, and your thread was told%s\n",
		ho.to, ho.mapping, related)
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

// again is a failed step's error, saying that a rerun finishes the
// handoff.
func again(err error) error {
	var failure *exit.Failure
	if errors.As(err, &failure) {
		return exit.New(failure.Code, failure.Message+"; the steps done are recorded, run thread handoff again to finish")
	}
	return err
}

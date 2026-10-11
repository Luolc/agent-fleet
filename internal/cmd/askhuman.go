// `fleet ask-human`: a lead (or a thread agent) asks the people in the
// job's home thread a question; fleet posts it, and the thread agent passes
// the answer back.

package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/fednet"
	"github.com/Luolc/agent-fleet/internal/herdr"
	"github.com/Luolc/agent-fleet/internal/identity"
)

// AskHumanAbout and AskHumanLongAbout are the help texts of `ask-human`.
const (
	AskHumanAbout     = "Ask the people in your job's home thread a question (leads and thread agents)"
	AskHumanLongAbout = "Ask the people in your job's home thread a question (leads and thread agents).\n\n" +
		"A lead has no other way to talk to people. The question is read from --file; it is " +
		"recorded as pending in the ledger (table `questions`), posted by fleet to the job's " +
		"home thread (`jobs.home_thread`, the thread of the thread agent that started the " +
		"job) with `fednet client post`, then delivered there as `fleet inbox` delivers a " +
		"message: to the thread's live agent, headed `[FROM: <you>]`, or to a thread agent " +
		"started for the thread (its ticket claimed again, the question as the session's " +
		"first message). When a person answers in the thread, the thread agent passes the " +
		"answer on with `fleet send`. A person's message in the thread marks the thread's " +
		"pending questions answered.\n\n" +
		"A single-repo job without a home thread (started by a caller without FLEET_ROLE) gets " +
		"one at its lead's first ask: fleet opens a thread with `fednet client open-thread` in " +
		"the channel of the job's repo (`<repo prefix><repo>`, or the initiative's channel for " +
		"an initiative's repo run in the initiative's checkout), or, when fleet knows no id " +
		"of that channel, in the general initiative's channel (`<initiative prefix><general " +
		"initiative>`); the question should then name the repo in its first line. fleet takes " +
		"a channel's id from the newest thread of it this scope has seen, or else from fednet's " +
		"routes (`fednet client routes -json`), the route with the channel's name (direct " +
		"messages left out in both); routes fednet cannot give count as naming none. The thread is recorded as the job's home thread, and the " +
		"question goes on as above. Until it is recorded nothing of the question is.\n\n" +
		"The post and the delivery are recorded in the ledger (table `steps`), so running it " +
		"again with the same question while it is pending continues where it stopped and " +
		"never posts it twice.\n\n" +
		"Approval cards (fednet's request-approval) are not supported yet: --approval is " +
		"refused. Ask for a go-ahead in plain text (\"reply yes to go ahead\").\n\n" +
		"A thread agent asking records the question on its own thread and fleet posts it " +
		"there; nothing is delivered.\n\n" +
		"Exit: 0 when the question is posted and the thread agent has it (a thread agent's " +
		"own: once it is posted); 1 with --approval, when the caller is a worker or a screen helper, the file " +
		"is unreadable or empty, the job is not open, a cross-repo job has no home thread (it " +
		"was started by a caller without FLEET_THREAD), fleet knows neither channel to open " +
		"a single-repo job's home thread in, or the scope has no fednet socket; " +
		"2/3/4 as `send` for the delivery; 3 when a new thread agent stops at an unknown " +
		"screen; 5 when opening the thread or the post fails, or herdr, atb or the database fails. Linear " +
		"unavailable while a thread agent had to be started is exit 5 too. After a failure " +
		"the question stays pending; ask again to finish."
)

// AskHumanArgs are the arguments of `ask-human`.
type AskHumanArgs struct {
	// File holds the question.
	File string
	// Approval asks for an approval card; not supported yet, refused.
	Approval bool
}

// AskHuman runs `ask-human`.
func AskHuman(h *herdr.Herdr, args AskHumanArgs) (exit.Code, error) {
	me, err := identity.FromEnv()
	if err != nil {
		return 0, err
	}
	if args.Approval {
		return 0, exit.Refusedf("approval cards are not supported yet; ask in plain text (\"reply yes to go ahead\") without --approval")
	}
	if me.Role == identity.Worker {
		return 0, exit.Refusedf("a worker does not ask people; tell your lead with `fleet send %s`", me.Parent)
	}
	if me.Role == identity.Unblock || me.Role == identity.Revisit {
		return 0, exit.Refusedf("an agent fleet's watch started does not ask people itself; ask the way your task says")
	}
	data, err := os.ReadFile(args.File)
	if err != nil {
		return 0, exit.Refusedf("cannot read %s: %v", args.File, err)
	}
	// Only the end is trimmed: an indented first line is Markdown.
	text := strings.TrimRight(string(data), " \t\r\n")
	if strings.TrimSpace(text) == "" {
		return 0, exit.Refusedf("the question file is empty")
	}
	conn, err := db.Open(me.Scope)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	thread, job, cfg, err := askThread(conn, me)
	if err != nil {
		return 0, err
	}
	id, err := pendingQuestion(conn, job, thread, me.Agent, text)
	if err != nil {
		return 0, err
	}
	key := fmt.Sprintf("ask-human:%d", id)
	posted, err := postQuestion(conn, key, cfg.FednetSocket, thread, text)
	if err != nil {
		return 0, err
	}
	if me.Role == identity.Thread {
		if posted {
			fmt.Fprintf(os.Stdout, "posted a pending question to thread %s\n", thread)
		} else {
			fmt.Fprintf(os.Stdout, "the question was already posted to thread %s; nothing done\n", thread)
		}
		return exit.Ok, nil
	}
	msg := inboundMessage{Thread: thread, Text: text, Question: me.Agent}
	delivered, err := deliverQuestion(h, conn, key, me.Scope, cfg, msg)
	if err != nil {
		return 0, err
	}
	if !posted && !delivered {
		fmt.Fprintf(os.Stdout, "the question was already posted to thread %s and delivered to its agent; nothing done\n", thread)
		return exit.Ok, nil
	}
	fmt.Fprintf(os.Stdout, "posted the question to thread %s; its agent passes the answer on\n", thread)
	return exit.Ok, nil
}

// postQuestion is the `post` step of an ask: true when it ran now, false
// when an earlier run recorded it. fednet's output is not shown, and the
// failure names only what the caller can do: ask again.
func postQuestion(conn *sql.DB, key, socket, thread, text string) (bool, error) {
	ran := false
	err := runStep(conn, key, "post", func() error {
		ran = true
		_, _, code, err := fednet.Relay(socket, thread, text, nil)
		if err == nil && code != 0 {
			err = exit.Environmentf("fednet client post failed (exit status: %d)", code)
		}
		return err
	})
	var failure *exit.Failure
	if errors.As(err, &failure) {
		return false, exit.New(failure.Code, failure.Message+"; the question is recorded, run ask-human again to post it")
	}
	return ran, err
}

// deliverQuestion is the `deliver` step of a lead's ask: true when it ran
// now, false when an earlier run recorded it. A failure says the question
// is posted already, so asking again only delivers it.
func deliverQuestion(h *herdr.Herdr, conn *sql.DB, key, scope string, cfg *config.Scope, msg inboundMessage) (bool, error) {
	ran := false
	err := runStep(conn, key, "deliver", func() error {
		ran = true
		code, err := toThread(h, conn, scope, cfg, msg)
		if err == nil && code != exit.Ok {
			err = exit.New(code, fmt.Sprintf("did not reach the agent of thread %s (exit %d)", msg.Thread, code))
		}
		return err
	})
	var failure *exit.Failure
	if errors.As(err, &failure) {
		return false, exit.New(failure.Code, failure.Message+"; the question is posted, run ask-human again to deliver it")
	}
	return ran, err
}

// askThread is the thread the caller asks in, as askedIn names it, with
// the scope's settings, which must name fednet's socket; a single-repo
// job without a home thread gets one opened first.
func askThread(conn *sql.DB, me *identity.Identity) (thread, job string, cfg *config.Scope, err error) {
	thread, job, row, err := askedIn(conn, me)
	if err != nil {
		return "", "", nil, err
	}
	if cfg, err = config.LoadScope(me.Scope); err != nil {
		return "", "", nil, err
	}
	if cfg.FednetSocket == "" {
		return "", "", nil, exit.Refusedf("fednet.socket is not configured for scope %s, so fleet cannot post the question", me.Scope)
	}
	if thread == "" {
		if thread, err = openHomeThread(conn, cfg, row); err != nil {
			return "", "", nil, err
		}
		fmt.Fprintf(os.Stdout, "opened thread %s as the home thread of job %s\n", thread, job)
	}
	return thread, job, cfg, nil
}

// askedIn is the thread the caller asks in, with the job (empty for a
// thread agent): a lead's open job's home thread, a thread agent's own.
// The thread is empty for a single-repo job without a home thread, which
// gets one when its lead first asks; `row` is a lead's job.
func askedIn(conn *sql.DB, me *identity.Identity) (thread, job string, row *jobRow, err error) {
	if me.Role != identity.Lead {
		if me.Thread == "" {
			return "", "", nil, exit.Refusedf("FLEET_THREAD is not set: a thread agent asks in its own thread")
		}
		return me.Thread, "", nil, nil
	}
	if me.Job == "" {
		return "", "", nil, exit.Refusedf("FLEET_JOB is not set")
	}
	row, err = openJob(conn, me.Job)
	if err != nil {
		return "", "", nil, err
	}
	if row == nil {
		return "", "", nil, exit.Refusedf("job %s is not open in scope %s", me.Job, me.Scope)
	}
	if row.HomeThread == "" && row.Repo == "" {
		return "", "", nil, exit.Refusedf("job %s has no home thread: it is a cross-repo job started by a caller "+
			"without FLEET_THREAD, so there is no thread to ask in", me.Job)
	}
	return row.HomeThread, me.Job, row, nil
}

// openHomeThread opens a thread for the questions of a single-repo job
// without a home thread, in its repo's channel, or the general
// initiative's when fleet knows no id of the repo's, and records it as
// the job's home thread and as a thread of that channel, so its agent
// starts in that channel's directory, and the next lookup finds the
// channel in the ledger. A channel's id is the one of the newest thread
// this scope has seen there, or else the one fednet's routes give the
// channel's name; direct messages, which have the general mapping too,
// are left out: their Slack ids start with D, and no thread opens there.
// Routes fednet cannot give count as naming no channel, with a line on
// stderr. Until the thread is recorded nothing of the question is.
func openHomeThread(conn *sql.DB, cfg *config.Scope, row *jobRow) (string, error) {
	tried := []string{jobMapping(cfg, row.Repo, row.LeadCwd), generalMapping(cfg.Channels)}
	var routes map[string]string
	var mapping, channel string
	for i, m := range tried {
		mapping = m
		err := conn.QueryRow("SELECT channel FROM threads WHERE mapping = ?1 AND channel != '' AND channel NOT LIKE 'D%' "+
			"ORDER BY id DESC LIMIT 1",
			mapping).Scan(&channel)
		if err == nil {
			break
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", exit.Database(err)
		}
		if i == 0 {
			if routes, err = fednet.Channels(cfg.FednetSocket); err != nil {
				fmt.Fprintf(os.Stderr, "fleet: %v; channel ids are taken from the ledger only\n", err)
			}
		}
		if channel = routes[mapping]; channel != "" {
			break
		}
	}
	if channel == "" {
		return "", exit.Refusedf("job %s has no home thread, and fleet cannot open one: no thread of #%s or #%s has "+
			"reached this scope, and fednet's routes name neither, so their ids are unknown (a message to the bot in "+
			"either, or a route with the channel's name, makes it known); nothing is recorded, run ask-human again then",
			row.Job, tried[0], tried[1])
	}
	text := fmt.Sprintf("Questions from job %s in %s, which started without a thread; fleet posts them below.", row.Job, row.Repo)
	thread, err := fednet.OpenThread(cfg.FednetSocket, channel, text)
	var failure *exit.Failure
	if errors.As(err, &failure) {
		return "", exit.New(failure.Code, failure.Message+"; nothing is recorded, run ask-human again")
	}
	if err != nil {
		return "", err
	}
	return thread, reserve(conn, func(querier) error { return nil }, func(q querier) error {
		if _, err := q.Exec("INSERT INTO threads (thread, slug, channel, mapping, cwd, created_at) VALUES (?1, ?2, ?3, ?4, ?5, ?6)",
			thread, ThreadSlug(thread), channel, mapping, MappingDir(cfg, mapping), db.Now()); err != nil {
			return exit.Database(err)
		}
		if _, err := q.Exec("UPDATE jobs SET home_thread = ?1 WHERE job = ?2 AND state = 'open'", thread, row.Job); err != nil {
			return exit.Database(err)
		}
		return nil
	})
}

// pendingQuestion is the id of the pending question with this text from
// this asker in this thread, recorded now unless an earlier run did.
func pendingQuestion(conn *sql.DB, job, thread, by, text string) (int64, error) {
	var id int64
	err := conn.QueryRow("SELECT id FROM questions WHERE job = ?1 AND thread = ?2 AND asked_by = ?3 AND text = ?4 "+
		"AND state = 'pending' ORDER BY id DESC LIMIT 1", job, thread, by, text).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, exit.Database(err)
	}
	res, err := conn.Exec("INSERT INTO questions (job, thread, asked_by, text, approval, state, asked_at) "+
		"VALUES (?1, ?2, ?3, ?4, 0, 'pending', ?5)", job, thread, by, text, db.Now())
	if err != nil {
		return 0, exit.Database(err)
	}
	if id, err = res.LastInsertId(); err != nil {
		return 0, exit.Database(err)
	}
	return id, nil
}

// toThread takes an agent's message to a thread as `route` does, with a
// dropped message (Linear unavailable) an environment failure: unlike
// fednet, the caller gets nothing from a drop.
func toThread(h *herdr.Herdr, conn *sql.DB, scope string, cfg *config.Scope, msg inboundMessage) (exit.Code, error) {
	code, dropped, err := route(h, conn, scope, cfg, msg)
	if err != nil || code != exit.Ok {
		return code, err
	}
	if dropped {
		return 0, exit.Environmentf("Linear is unavailable, so no thread agent could be started for %s; try again later",
			msg.Thread)
	}
	return exit.Ok, nil
}

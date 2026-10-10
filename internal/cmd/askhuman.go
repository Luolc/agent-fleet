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
		"The post and the delivery are recorded in the ledger (table `steps`), so running it " +
		"again with the same question while it is pending continues where it stopped and " +
		"never posts it twice.\n\n" +
		"Approval cards (fednet's request-approval) are not supported yet: --approval is " +
		"refused. Ask for a go-ahead in plain text (\"reply yes to go ahead\").\n\n" +
		"A thread agent asking records the question on its own thread and fleet posts it " +
		"there; nothing is delivered.\n\n" +
		"Exit: 0 when the question is posted and the thread agent has it (a thread agent's " +
		"own: once it is posted); 1 with --approval, when the caller is a worker, the file " +
		"is unreadable or empty, the job is not open, the job has no home thread (it was " +
		"started by a caller without FLEET_THREAD), or the scope has no fednet socket; " +
		"2/3/4 as `send` for the delivery; 3 when a new thread agent stops at an unknown " +
		"screen; 5 when the post fails, or herdr, atb or the database fails. Linear " +
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
	data, err := os.ReadFile(args.File)
	if err != nil {
		return 0, exit.Refusedf("cannot read %s: %v", args.File, err)
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return 0, exit.Refusedf("the question file is empty")
	}
	conn, err := db.Open(me.Scope)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	thread, job, err := askedIn(conn, me)
	if err != nil {
		return 0, err
	}
	cfg, err := config.LoadScope(me.Scope)
	if err != nil {
		return 0, err
	}
	if cfg.FednetSocket == "" {
		return 0, exit.Refusedf("fednet.socket is not configured for scope %s, so fleet cannot post the question", me.Scope)
	}
	id, err := pendingQuestion(conn, job, thread, me.Agent, text)
	if err != nil {
		return 0, err
	}
	key := fmt.Sprintf("ask-human:%d", id)
	if err := runStep(conn, key, "post", func() error { return fednet.Post(cfg.FednetSocket, thread, text) }); err != nil {
		var failure *exit.Failure
		if errors.As(err, &failure) {
			return 0, exit.New(failure.Code, failure.Message+"; the question is recorded, run ask-human again to post it")
		}
		return 0, err
	}
	if me.Role == identity.Thread {
		fmt.Fprintf(os.Stdout, "posted a pending question to thread %s\n", thread)
		return exit.Ok, nil
	}
	msg := inboundMessage{Thread: thread, Text: text, Question: me.Agent}
	if err := runStep(conn, key, "deliver", func() error {
		code, err := toThread(h, conn, me.Scope, cfg, msg)
		if err == nil && code != exit.Ok {
			err = exit.New(code, fmt.Sprintf("the question is posted but did not reach the agent of thread %s (exit %d); "+
				"run ask-human again to deliver it", thread, code))
		}
		return err
	}); err != nil {
		return 0, err
	}
	fmt.Fprintf(os.Stdout, "posted the question to thread %s; its agent passes the answer on\n", thread)
	return exit.Ok, nil
}

// askedIn is the thread the caller asks in, with the job (empty for a
// thread agent): a lead's open job's home thread, a thread agent's own.
func askedIn(conn *sql.DB, me *identity.Identity) (thread, job string, err error) {
	if me.Role != identity.Lead {
		if me.Thread == "" {
			return "", "", exit.Refusedf("FLEET_THREAD is not set: a thread agent asks in its own thread")
		}
		return me.Thread, "", nil
	}
	if me.Job == "" {
		return "", "", exit.Refusedf("FLEET_JOB is not set")
	}
	row, err := openJob(conn, me.Job)
	if err != nil {
		return "", "", err
	}
	if row == nil {
		return "", "", exit.Refusedf("job %s is not open in scope %s", me.Job, me.Scope)
	}
	if row.HomeThread == "" {
		return "", "", exit.Refusedf("job %s has no home thread: it was started by a caller without FLEET_THREAD, "+
			"so there is no thread to ask in", me.Job)
	}
	return row.HomeThread, me.Job, nil
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

// `fleet ask-human`: a lead (or a thread agent) asks the people in the
// job's home thread a question; the thread agent posts it and passes the
// answer back.

package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
	"github.com/Luolc/agent-fleet/internal/identity"
)

// AskHumanAbout and AskHumanLongAbout are the help texts of `ask-human`.
const (
	AskHumanAbout     = "Ask the people in your job's home thread a question, or for an approval (leads and thread agents)"
	AskHumanLongAbout = "Ask the people in your job's home thread a question, or for an approval (leads and thread agents).\n\n" +
		"A lead has no other way to talk to people. The question is read from --file; it is " +
		"recorded as pending in the ledger (table `questions`), then delivered to the job's " +
		"home thread (`jobs.home_thread`, the thread of the thread agent that started the " +
		"job) as `fleet inbox` delivers a message: to the thread's live agent, headed `[FROM: " +
		"<you>]`, or to a thread agent started for the thread (its ticket claimed again, the " +
		"question as the session's first message). The thread agent posts the question " +
		"(`fednet client post`; with --approval, as an approval card with `fednet client " +
		"request-approval`) and, when a person answers in the thread, passes the answer on " +
		"with `fleet send`. A person's message in the thread marks the thread's pending " +
		"questions answered.\n\n" +
		"A thread agent asking records the question on its own thread and posts it itself; " +
		"nothing is delivered.\n\n" +
		"Exit: 0 when the thread agent has the question (or, for a thread agent, once it is " +
		"recorded); 1 when the caller is a worker, the file is unreadable or empty, the job " +
		"is not open, or the job has no home thread (it was started by a caller without " +
		"FLEET_THREAD); 2/3/4 as `send` for the delivery; 3 when a new thread agent stops " +
		"at an unknown screen; 5 when herdr, atb or the database fails. Linear unavailable " +
		"while a thread agent had to be started is exit 5 too: the question stays pending " +
		"and is not posted, ask again later."
)

// AskHumanArgs are the arguments of `ask-human`.
type AskHumanArgs struct {
	// File holds the question.
	File string
	// Approval asks for an approval card instead of a plain question.
	Approval bool
}

// AskHuman runs `ask-human`.
func AskHuman(h *herdr.Herdr, args AskHumanArgs) (exit.Code, error) {
	me, err := identity.FromEnv()
	if err != nil {
		return 0, err
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
	conn, err := db.Open(me.Target)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	thread, job := me.Thread, ""
	if me.Role == identity.Lead {
		if me.Job == "" {
			return 0, exit.Refusedf("FLEET_JOB is not set")
		}
		row, err := openJob(conn, me.Job)
		if err != nil {
			return 0, err
		}
		if row == nil {
			return 0, exit.Refusedf("job %s is not open in target %s", me.Job, me.Target)
		}
		if row.HomeThread == "" {
			return 0, exit.Refusedf("job %s has no home thread: it was started by a caller without FLEET_THREAD, "+
				"so there is no thread to ask in", me.Job)
		}
		thread, job = row.HomeThread, me.Job
	} else if thread == "" {
		return 0, exit.Refusedf("FLEET_THREAD is not set: a thread agent asks in its own thread")
	}
	if _, err := conn.Exec("INSERT INTO questions (job, thread, asked_by, text, approval, state, asked_at) "+
		"VALUES (?1, ?2, ?3, ?4, ?5, 'pending', ?6)", job, thread, me.Agent, text, args.Approval, db.Now()); err != nil {
		return 0, exit.Database(err)
	}
	if me.Role == identity.Thread {
		fmt.Fprintf(os.Stdout, "recorded a pending question in thread %s; post it there\n", thread)
		return exit.Ok, nil
	}
	cfg, err := config.LoadTarget(me.Target)
	if err != nil {
		return 0, err
	}
	msg := inboundMessage{Thread: thread, Text: text, Question: me.Agent, Approval: args.Approval}
	return toThread(h, conn, me.Target, cfg, msg)
}

// toThread takes an agent's message to a thread as `route` does, with a
// dropped message (Linear unavailable) an environment failure: unlike
// fednet, the caller gets nothing from a drop.
func toThread(h *herdr.Herdr, conn *sql.DB, target string, cfg *config.Target, msg inboundMessage) (exit.Code, error) {
	code, dropped, err := route(h, conn, target, cfg, msg)
	if err != nil || code != exit.Ok {
		return code, err
	}
	if dropped {
		return 0, exit.Environmentf("Linear is unavailable, so no thread agent could be started for %s; try again later",
			msg.Thread)
	}
	return exit.Ok, nil
}

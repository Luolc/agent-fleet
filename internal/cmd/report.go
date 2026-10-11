// `fleet report`: how often agents asked people, and how long people took
// to answer, from the ledger's questions.

package cmd

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"

	"github.com/Luolc/agent-fleet/internal/exit"
)

// ReportAbout and ReportLongAbout are the help texts of `report`.
const (
	ReportAbout     = "Count the questions agents put to people and how long the answers took (read-only)"
	ReportLongAbout = "Count the questions agents put to people and how long the answers took (read-only).\n\n" +
		"Reads only the scope's ledger. A question counts once: a rerun of `fleet ask-human` while it " +
		"is pending records nothing new. One line per job, open and ended, in start order, with its " +
		"state (open, its outcome, or unknown when the ledger has no row for the job), including the " +
		"jobs that asked nothing; then the questions thread agents asked themselves, one line per thread; then " +
		"the total. For each: questions asked, answered, pending and closed (given up on: its job " +
		"ended, or `fleet watch` reclaimed the session that asked it), and the median and longest " +
		"wait of the answered ones.\n\n" +
		"A question is answered when the next message a person posts in its thread arrives, and that " +
		"message marks every question pending there answered: the wait runs to that message, which " +
		"is not necessarily the answer to the question.\n\n" +
		"Exit: 0; 1 when the scope is not a scope name; 5 when the database fails or the ledger does " +
		"not exist."
)

// ReportArgs are the arguments of `report`.
type ReportArgs struct {
	// JSON asks for machine-readable output: {"jobs": [...], "threads": [...], "total": {...}}.
	JSON bool
}

// asked counts one group's questions. The waits are in seconds and null
// when nothing was answered.
type asked struct {
	Asked          int    `json:"asked"`
	Answered       int    `json:"answered"`
	Pending        int    `json:"pending"`
	Closed         int    `json:"closed"`
	WaitMedianSecs *int64 `json:"wait_median_secs"`
	WaitMaxSecs    *int64 `json:"wait_max_secs"`
	waits          []int64
}

// jobAsked is one job's line: its state is `open` or its outcome.
type jobAsked struct {
	Job       string `json:"job"`
	State     string `json:"state"`
	StartedAt int64  `json:"started_at"`
	asked
}

// threadAsked is one thread's line, for the questions its thread agent asked.
type threadAsked struct {
	Thread string `json:"thread"`
	Ticket string `json:"ticket"`
	asked
}

// attention is the whole of `report`.
type attention struct {
	Jobs    []*jobAsked    `json:"jobs"`
	Threads []*threadAsked `json:"threads"`
	Total   *asked         `json:"total"`
}

// add counts one question.
func (a *asked) add(state string, wait *int64) {
	a.Asked++
	switch state {
	case "pending":
		a.Pending++
		return
	case "closed":
		a.Closed++
		return
	}
	a.Answered++
	if wait != nil {
		a.waits = append(a.waits, *wait)
	}
}

// settle sets the median and longest wait.
func (a *asked) settle() {
	if len(a.waits) == 0 {
		return
	}
	slices.Sort(a.waits)
	n := len(a.waits)
	median := a.waits[n/2]
	if n%2 == 0 {
		median = (a.waits[n/2-1] + a.waits[n/2]) / 2
	}
	a.WaitMedianSecs, a.WaitMaxSecs = &median, &a.waits[n-1]
}

// attentionOf reads the jobs and questions of the ledger.
func attentionOf(conn *sql.DB) (*attention, error) {
	out := &attention{Jobs: []*jobAsked{}, Threads: []*threadAsked{}, Total: &asked{}}
	rows, err := conn.Query("SELECT job, state, outcome, started_at FROM jobs ORDER BY id")
	if err != nil {
		return nil, exit.Database(err)
	}
	defer rows.Close()
	for rows.Next() {
		var j jobAsked
		var outcome string
		if err := rows.Scan(&j.Job, &j.State, &outcome, &j.StartedAt); err != nil {
			return nil, exit.Database(err)
		}
		if j.State == "ended" && outcome != "" {
			j.State = outcome
		}
		out.Jobs = append(out.Jobs, &j)
	}
	if err := rows.Err(); err != nil {
		return nil, exit.Database(err)
	}
	questions, err := conn.Query("SELECT q.job, q.thread, COALESCE(t.ticket, ''), q.state, q.asked_at, " +
		"q.answered_at - q.asked_at FROM questions q LEFT JOIN threads t ON t.thread = q.thread ORDER BY q.id")
	if err != nil {
		return nil, exit.Database(err)
	}
	defer questions.Close()
	for questions.Next() {
		var job, thread, ticket, state string
		var askedAt int64
		var wait sql.NullInt64
		if err := questions.Scan(&job, &thread, &ticket, &state, &askedAt, &wait); err != nil {
			return nil, exit.Database(err)
		}
		var waitSecs *int64
		if wait.Valid {
			waitSecs = &wait.Int64
		}
		out.Total.add(state, waitSecs)
		if job == "" {
			out.thread(thread, ticket).add(state, waitSecs)
		} else {
			out.job(job, askedAt).add(state, waitSecs)
		}
	}
	if err := questions.Err(); err != nil {
		return nil, exit.Database(err)
	}
	for _, j := range out.Jobs {
		j.settle()
	}
	for _, t := range out.Threads {
		t.settle()
	}
	out.Total.settle()
	return out, nil
}

// job is the line of the latest job named `name` started by `at`, since a
// name is unique only among open jobs.
func (a *attention) job(name string, at int64) *asked {
	var found *jobAsked
	for _, j := range a.Jobs {
		if j.Job == name && (found == nil || j.StartedAt <= at) {
			found = j
		}
	}
	if found == nil {
		found = &jobAsked{Job: name, State: "unknown", StartedAt: at}
		a.Jobs = append(a.Jobs, found)
	}
	return &found.asked
}

// thread is the line of a thread agent's questions in `thread`, added on
// its first question.
func (a *attention) thread(thread, ticket string) *asked {
	for _, t := range a.Threads {
		if t.Thread == thread {
			return &t.asked
		}
	}
	t := &threadAsked{Thread: thread, Ticket: ticket}
	a.Threads = append(a.Threads, t)
	return &t.asked
}

// AttentionReport runs `report`.
func AttentionReport(args ReportArgs) (exit.Code, error) {
	conn, err := OpenLedger()
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	report, err := attentionOf(conn)
	if err != nil {
		return 0, err
	}
	if args.JSON {
		var buf bytes.Buffer
		encoder := json.NewEncoder(&buf)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			return 0, exit.Environmentf("json: %v", err)
		}
		fmt.Fprint(os.Stdout, buf.String())
		return exit.Ok, nil
	}
	if len(report.Jobs) == 0 {
		fmt.Fprintln(os.Stdout, "no jobs")
	} else {
		rows := make([][]string, 0, len(report.Jobs))
		for _, j := range report.Jobs {
			rows = append(rows, append([]string{j.Job, j.State}, j.cells()...))
		}
		printAligned([]string{"JOB", "STATE", "ASKED", "ANSWERED", "PENDING", "CLOSED", "WAIT-MEDIAN", "WAIT-MAX"}, rows)
	}
	fmt.Fprintln(os.Stdout)
	if len(report.Threads) == 0 {
		fmt.Fprintln(os.Stdout, "no questions from thread agents")
	} else {
		rows := make([][]string, 0, len(report.Threads))
		for _, t := range report.Threads {
			rows = append(rows, append([]string{t.Thread, orDash(t.Ticket)}, t.cells()...))
		}
		printAligned([]string{"THREAD", "TICKET", "ASKED", "ANSWERED", "PENDING", "CLOSED", "WAIT-MEDIAN", "WAIT-MAX"}, rows)
	}
	fmt.Fprintln(os.Stdout)
	t := report.Total
	fmt.Fprintf(os.Stdout, "total: %d asked, %d answered, %d pending, %d closed; wait median %s, longest %s\n",
		t.Asked, t.Answered, t.Pending, t.Closed, durationOrDash(t.WaitMedianSecs), durationOrDash(t.WaitMaxSecs))
	return exit.Ok, nil
}

// cells are the counts and waits as table cells.
func (a *asked) cells() []string {
	return []string{strconv.Itoa(a.Asked), strconv.Itoa(a.Answered), strconv.Itoa(a.Pending), strconv.Itoa(a.Closed),
		durationOrDash(a.WaitMedianSecs), durationOrDash(a.WaitMaxSecs)}
}

func durationOrDash(secs *int64) string {
	if secs == nil {
		return "-"
	}
	return Duration(*secs)
}

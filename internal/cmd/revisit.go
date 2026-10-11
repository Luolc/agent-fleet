// What `watch` does for a parent issue that stays unchanged
// (docs/design.md): a revisit agent decides, by the issue's project's
// instructions, whether to close it, continue it with a new sub-issue, or
// ask the people.

package cmd

import (
	"database/sql"
	_ "embed" // the revisit agent's prompt
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/Luolc/agent-fleet/internal/atb"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/identity"
)

//go:embed revisit_prompt.md
var revisitPrompt string

// revisitPrefix starts the name of every revisit agent.
const revisitPrefix = "revisit-"

// revisitName is the revisit agent of `issue`: one name per issue, so at
// most one is live for it.
func revisitName(issue string) string {
	return revisitPrefix + strings.ToLower(issue)
}

// readParents reads from Linear the parent issues of the scope's jobs
// that no open job has, those in a started state; Linear is not asked
// when there are none. linearErr is a failed query, which skips only the
// parent rule; err is the ledger's.
func readParents(conn *sql.DB) (parents []atb.Parent, linearErr, err error) {
	var issues []string
	err = eachRow(conn, "SELECT DISTINCT parent_issue FROM jobs WHERE parent_issue != '' AND parent_issue NOT IN "+
		"(SELECT parent_issue FROM jobs WHERE state = 'open') ORDER BY parent_issue",
		func(rows *sql.Rows) error {
			var issue string
			if err := rows.Scan(&issue); err != nil {
				return err
			}
			issues = append(issues, issue)
			return nil
		})
	if err != nil || len(issues) == 0 {
		return nil, nil, err
	}
	parents, linearErr = atb.StartedIssues(issues)
	return parents, linearErr, nil
}

// revisit runs the parent rule. First the revisit agents that are done
// are closed, their questions asked. Then, unless Linear could not be
// read, each parent issue in a started state with sub-issues, unchanged
// for `parent_stale`, gets a revisit agent, the longest unchanged first and
// at most `parent_agents` in a run; none while one is live for it or a
// question from it waits, nor once one was started since the issue last
// changed (step `<updatedAt>` of `revisit:<issue>`).
func (r *watchRun) revisit(inHerdr map[string]InHerdr, parents []atb.Parent, linearErr error) error {
	u, err := r.helpers(inHerdr)
	if err != nil {
		return err
	}
	rows, err := stopRows(r.conn)
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for _, row := range rows {
		if row.Role != identity.Revisit.String() {
			continue
		}
		if u.outOfTime() {
			return nil
		}
		busy, err := u.closeDone(row, func(question string) error { return u.askAboutParent(row, question) })
		u.failedRevisit(row.Name, err)
		live[row.Name] = busy || err != nil
	}
	if linearErr != nil {
		fmt.Fprintf(os.Stderr, "fleet watch: the parent issues are skipped this run, Linear could not be read: %v\n", linearErr)
		r.got(exit.Environment)
		return nil
	}
	started := 0
	for _, p := range r.staleParents(parents) {
		if u.outOfTime() {
			return nil
		}
		name := revisitName(p.Identifier)
		if live[name] {
			fmt.Fprintf(os.Stdout, "revisit: %s has not changed; %s is on it\n", p.Identifier, name)
			continue
		}
		var pending int
		if err := r.conn.QueryRow("SELECT count(*) FROM questions WHERE asked_by = ?1 AND state = 'pending'",
			name).Scan(&pending); err != nil {
			return exit.Database(err)
		}
		if pending > 0 {
			fmt.Fprintf(os.Stdout, "revisit: %s has not changed; the people were asked about it and have not answered\n",
				p.Identifier)
			continue
		}
		if started >= r.cfg.Watch.ParentAgents {
			fmt.Fprintf(os.Stdout, "revisit: %s has not changed; it waits, since at most %d revisit agent(s) start in a run\n",
				p.Identifier, r.cfg.Watch.ParentAgents)
			continue
		}
		tried := false
		err := runStep(r.conn, "revisit:"+p.Identifier, p.UpdatedAt.UTC().Format(time.RFC3339Nano), func() error {
			tried = true
			return u.startRevisit(p, name)
		})
		u.failedRevisit(name, err)
		if tried {
			started++
		} else if err == nil {
			fmt.Fprintf(os.Stdout, "revisit: %s has not changed since a revisit agent looked at it\n", p.Identifier)
		}
	}
	return nil
}

// failedRevisit records a failure for the revisit agent `name`.
func (u *unblocking) failedRevisit(name string, err error) {
	if err != nil {
		u.failed(fmt.Errorf("revisit %s: %w", name, err))
	}
}

// staleParents are the issues of `parents` in a started state, with
// sub-issues, unchanged for parent_stale, the longest unchanged first.
func (r *watchRun) staleParents(parents []atb.Parent) []atb.Parent {
	var stale []atb.Parent
	for _, p := range parents {
		if p.State.Type == "started" && len(p.Children.Nodes) > 0 &&
			r.now-p.UpdatedAt.Unix() >= secs(r.cfg.Watch.ParentStale) {
			stale = append(stale, p)
		}
	}
	slices.SortStableFunc(stale, func(a, b atb.Parent) int { return a.UpdatedAt.Compare(b.UpdatedAt) })
	return stale
}

// parentThread is the home thread of the latest job on `issue` that has
// one, "" when none has.
func parentThread(conn *sql.DB, issue string) (string, error) {
	var thread string
	err := conn.QueryRow("SELECT home_thread FROM jobs WHERE parent_issue = ?1 AND home_thread != '' "+
		"ORDER BY id DESC LIMIT 1", issue).Scan(&thread)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", exit.Database(err)
	}
	return thread, nil
}

// startRevisit starts the revisit agent `name` for the parent issue `p`.
func (u *unblocking) startRevisit(p atb.Parent, name string) error {
	body, err := u.revisitBody(p, name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(u.dir, 0o755); err != nil {
		return exit.IO(err)
	}
	id := &identity.Identity{Agent: name, Role: identity.Revisit, Scope: u.scope}
	if err := u.startInThreads(id, p.Identifier, body); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "revisit: %s has not changed for %s: started %s\n", p.Identifier,
		Duration(u.now-p.UpdatedAt.Unix()), name)
	return nil
}

// revisitBody is the revisit agent's first message: the prompt, the issue,
// its sub-issues, fleet's jobs on it, and last its project's instructions
// as Linear has them.
func (u *unblocking) revisitBody(p atb.Parent, name string) (string, error) {
	thread, err := parentThread(u.conn, p.Identifier)
	if err != nil {
		return "", err
	}
	ask := fmt.Sprintf("There is no thread to ask in, so ask on the issue: write the question to a file in your "+
		"directory (what you found, the choices, and what you recommend), run `atb linear comment %s --body-file "+
		"<file>`, then `atb linear edit %s --add-label needs-user`.", p.Identifier, p.Identifier)
	if thread != "" && u.cfg.FednetSocket != "" {
		ask = fmt.Sprintf("Write your question to %s: what you found, the choices, and what you recommend, in a "+
			"short paragraph. fleet posts it to the thread %s, where its latest job on the issue reported, and once "+
			"the people answer, that thread's agent carries out what they decide.", u.questionFile(name), thread)
	}
	create := "atb linear create --team <team> --parent " + p.Identifier + " --title <title> --description-file <file>"
	if p.Team != nil {
		create = strings.Replace(create, "<team>", p.Team.Key, 1)
	}
	if p.Project != nil {
		create = strings.Replace(create, " --parent", " --project '"+p.Project.Name+"' --parent", 1)
	}
	text := strings.NewReplacer("{{agent}}", name, "{{scope}}", u.scope, "{{dir}}", u.cfg.Tilde(u.dir),
		"{{issue}}", p.Identifier, "{{unchanged}}", Duration(u.now-p.UpdatedAt.Unix()), "{{create}}", create,
		"{{ask}}", ask).Replace(revisitPrompt)
	at := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	text += fmt.Sprintf("\n## The issue\n\n%s: %s (%s), state %s, last changed %s.\n", p.Identifier, p.Title, p.URL,
		p.State.Name, at(p.UpdatedAt))
	text += "\n## Its sub-issues\n\n"
	for _, c := range p.Children.Nodes {
		text += fmt.Sprintf("- %s: %s (%s), state %s (%s), last changed %s\n", c.Identifier, c.Title, c.URL,
			c.State.Name, c.State.Type, at(c.UpdatedAt))
	}
	text += "\n## fleet's jobs on it\n\n"
	err = eachRow(u.conn, "SELECT job, state, outcome, started_at, ended_at, home_thread FROM jobs "+
		"WHERE parent_issue = ?1 ORDER BY id", func(rows *sql.Rows) error {
		var job, state, outcome, home string
		var startedAt int64
		var endedAt sql.NullInt64
		if err := rows.Scan(&job, &state, &outcome, &startedAt, &endedAt, &home); err != nil {
			return err
		}
		line := fmt.Sprintf("- %s: %s", job, state)
		if outcome != "" {
			line += ", " + outcome
		}
		line += ", started " + at(time.Unix(startedAt, 0))
		if endedAt.Valid {
			line += ", ended " + at(time.Unix(endedAt.Int64, 0))
		}
		if home != "" {
			line += ", reporting to thread " + home
		}
		text += line + "\n"
		return nil
	}, p.Identifier)
	if err != nil {
		return "", err
	}
	if p.Project == nil {
		return text + "\n## Its project\n\nThe issue is in no project, so there are no instructions: decide by the " +
			"issue and its sub-issues.\n", nil
	}
	instructions := strings.TrimSpace(strings.TrimSpace(p.Project.Description) + "\n\n" + strings.TrimSpace(p.Project.Content))
	if instructions == "" {
		instructions = "(The project has no description.)"
	}
	return text + fmt.Sprintf("\n## Its project: %s\n\nIts instructions, the project's description as Linear has "+
		"it, from here to the end of this message:\n\n%s\n", p.Project.Name, instructions), nil
}

// askAboutParent asks the people the question of the revisit agent `row`
// in the home thread of its issue's latest job, records it as pending
// from the agent, and gives it to the thread's agent, which carries out
// their answer; a delivery that fails is printed, since the question is
// out.
func (u *unblocking) askAboutParent(row stopRow, question string) error {
	thread, err := parentThread(u.conn, row.ParentIssue)
	if err != nil {
		return err
	}
	if thread == "" {
		return exit.Refusedf("no home thread to ask the people in about %s", row.ParentIssue)
	}
	text := fmt.Sprintf("%s looked at the parent issue %s, which has not changed for a while and has no job open on "+
		"it, and asks:\n\n%s", row.Name, row.ParentIssue, question)
	if err := u.post(thread, "", row.Name, text); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "revisit: %s: asked the people in thread %s\n", row.ParentIssue, thread)
	notice := fmt.Sprintf("fleet watch, rule `parent issue unchanged`: %s asked the people in thread %s about the "+
		"parent issue %s, and fleet posted it there:\n\n%s\n\nWhen they answer, carry out what they decide: close "+
		"the issue, open a sub-issue, or start a job on it. Nobody else will; the question is reminded of but never "+
		"times out.\n", row.Name, thread, row.ParentIssue, question)
	msg := inboundMessage{Thread: thread, Text: notice, Watch: true}
	if code, err := toThread(u.h, u.conn, u.scope, u.cfg, msg); err != nil || code != exit.Ok {
		fmt.Fprintf(os.Stderr, "fleet: revisit %s: the question is posted, but did not reach the agent of thread %s "+
			"(exit %d): %v\n", row.Name, thread, code, err)
	}
	return nil
}

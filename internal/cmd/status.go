// `fleet status`: who is running and who owes work. Also holds the ledger
// and herdr reads that `watch` shares.

package cmd

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
)

// StatusAbout and StatusLongAbout are the help texts of `status`.
const (
	StatusAbout     = "Show who is running and who owes work (read-only)"
	StatusLongAbout = "Show who is running and who owes work (read-only).\n\n" +
		"Joins the ledger's live rows (state starting or active) with `herdr agent list`. For each " +
		"agent: role, job, parent, age, herdr status, and time since `fleet watch` last saw its " +
		"status, state_change_seq or screen change (`-` before watch has looked at it). Flags:\n" +
		"  owes-work  a lead or worker that has not run `fleet done` but is idle or done in herdr\n" +
		"  blocked    herdr reports it blocked by an interactive prompt\n" +
		"  missing    in the ledger but gone from herdr\n" +
		"  suspect    `fleet watch` currently counts it as stuck\n\n" +
		"Exit: 0; 1 when no repo is given; 5 when herdr or the database fails, or the ledger does " +
		"not exist."
)

// StatusArgs are the arguments of `status`.
type StatusArgs struct {
	// Job, when set, keeps only this job's agents.
	Job *string
	// JSON asks for machine-readable output: a JSON array, one object per agent.
	JSON bool
	// Repo is the dataset repo whose ledger to read (`owner/name` or `name`);
	// the ledger is ~/scratch/<name>/fleet.db. Defaults to FLEET_REPO, which
	// every agent has.
	Repo *string
}

// Live is a ledger row that has not ended, with the columns `status` and
// `watch` use.
type Live struct {
	ID             int64
	Name           string
	Role           string
	Job            string
	Parent         string
	State          string
	StartedAt      int64
	LastStatus     sql.NullString
	LastSeq        sql.NullInt64
	LastScreenHash sql.NullString
	LastChangeAt   sql.NullInt64
	Suspect        bool
}

// InHerdr is what `herdr agent list` says about one agent.
type InHerdr struct {
	Status string
	Seq    int64
}

// line is one line of `status` output.
type line struct {
	Name            string   `json:"name"`
	Role            string   `json:"role"`
	Job             string   `json:"job"`
	Parent          string   `json:"parent"`
	State           string   `json:"state"`
	AgeSecs         int64    `json:"age_secs"`
	HerdrStatus     *string  `json:"herdr_status"`
	SinceChangeSecs *int64   `json:"since_change_secs"`
	Flags           []string `json:"flags"`
}

// OpenLedger opens the existing ledger of `repo` (or FLEET_REPO). A missing
// ledger is an error, not an empty one: it usually means a wrong repo name.
func OpenLedger(repo *string) (*sql.DB, error) {
	name := os.Getenv("FLEET_REPO")
	if repo != nil {
		name = *repo
	}
	if name == "" {
		return nil, exit.Refusedf("no dataset repo: pass --repo <owner/name> (FLEET_REPO is not set)")
	}
	path, err := db.Path(name)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		return nil, exit.Environmentf(
			"no ledger at %s; is the repo name right, and has the ledger been created?", path)
	}
	return db.OpenAt(path)
}

// LiveRows are the live rows, optionally of one job, in job then start
// order.
func LiveRows(conn *sql.DB, job *string) ([]Live, error) {
	var jobArg any
	if job != nil {
		jobArg = *job
	}
	rows, err := conn.Query(
		"SELECT id, name, role, job, parent, state, started_at, last_status, last_seq, "+
			"last_screen_hash, last_change_at, suspect FROM agents "+
			"WHERE state != 'ended' AND (?1 IS NULL OR job = ?1) ORDER BY job, started_at, id",
		jobArg)
	if err != nil {
		return nil, exit.Database(err)
	}
	defer rows.Close()
	var lives []Live
	for rows.Next() {
		var l Live
		if err := rows.Scan(&l.ID, &l.Name, &l.Role, &l.Job, &l.Parent, &l.State, &l.StartedAt,
			&l.LastStatus, &l.LastSeq, &l.LastScreenHash, &l.LastChangeAt, &l.Suspect); err != nil {
			return nil, exit.Database(err)
		}
		lives = append(lives, l)
	}
	if err := rows.Err(); err != nil {
		return nil, exit.Database(err)
	}
	return lives, nil
}

// HerdrAgents is every agent herdr knows in this session, by name (`herdr
// agent list`).
func HerdrAgents(h *herdr.Herdr) (map[string]InHerdr, error) {
	result, err := h.CallOK("agent", "list")
	if err != nil {
		return nil, err
	}
	list, ok := herdr.Lookup(result, "agents").([]any)
	if !ok {
		return nil, exit.Environmentf("herdr agent list: no agents array in the reply")
	}
	agents := make(map[string]InHerdr)
	for _, entry := range list {
		agent, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		name, ok := agent["name"].(string)
		if !ok {
			continue
		}
		status, ok := agent["agent_status"].(string)
		if !ok {
			continue
		}
		number, ok := agent["state_change_seq"].(json.Number)
		if !ok {
			continue
		}
		seq, err := number.Int64()
		if err != nil {
			continue
		}
		agents[name] = InHerdr{Status: status, Seq: seq}
	}
	return agents, nil
}

// Status runs `status`.
func Status(h *herdr.Herdr, args StatusArgs) (exit.Code, error) {
	conn, err := OpenLedger(args.Repo)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	rows, err := LiveRows(conn, args.Job)
	if err != nil {
		return 0, err
	}
	inHerdr, err := HerdrAgents(h)
	if err != nil {
		return 0, err
	}
	now := db.Now()
	lines := make([]line, 0, len(rows))
	for _, live := range rows {
		var agent *InHerdr
		if a, ok := inHerdr[live.Name]; ok {
			agent = &a
		}
		lines = append(lines, statusLine(live, agent, now))
	}
	if args.JSON {
		var buf bytes.Buffer
		encoder := json.NewEncoder(&buf)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(lines); err != nil {
			return 0, exit.Environmentf("json: %v", err)
		}
		fmt.Fprint(os.Stdout, buf.String())
	} else {
		printTable(lines)
	}
	return exit.Ok, nil
}

func statusLine(live Live, agent *InHerdr, now int64) line {
	var herdrStatus *string
	if agent != nil {
		status := agent.Status
		herdrStatus = &status
	}
	owesWork := live.Role == "lead" || live.Role == "worker"
	flags := []string{}
	switch {
	case herdrStatus == nil:
		flags = append(flags, "missing")
	case (*herdrStatus == "idle" || *herdrStatus == "done") && owesWork:
		flags = append(flags, "owes-work")
	case *herdrStatus == "blocked":
		flags = append(flags, "blocked")
	}
	if live.Suspect {
		flags = append(flags, "suspect")
	}
	var sinceChange *int64
	if live.LastChangeAt.Valid {
		since := now - live.LastChangeAt.Int64
		sinceChange = &since
	}
	return line{
		Name:            live.Name,
		Role:            live.Role,
		Job:             live.Job,
		Parent:          live.Parent,
		State:           live.State,
		AgeSecs:         now - live.StartedAt,
		HerdrStatus:     herdrStatus,
		SinceChangeSecs: sinceChange,
		Flags:           flags,
	}
}

func printTable(lines []line) {
	if len(lines) == 0 {
		fmt.Fprintln(os.Stdout, "no live agents")
		return
	}
	cells := make([][8]string, 0, len(lines))
	for _, l := range lines {
		herdrStatus, sinceChange := "-", "-"
		if l.HerdrStatus != nil {
			herdrStatus = *l.HerdrStatus
		}
		if l.SinceChangeSecs != nil {
			sinceChange = Duration(*l.SinceChangeSecs)
		}
		cells = append(cells, [8]string{
			l.Name, l.Role, orDash(l.Job), orDash(l.Parent), Duration(l.AgeSecs),
			herdrStatus, sinceChange, strings.Join(l.Flags, ","),
		})
	}
	header := [8]string{"NAME", "ROLE", "JOB", "PARENT", "AGE", "HERDR", "CHANGED", "FLAGS"}
	var widths [8]int
	for i, h := range header {
		widths[i] = len(h)
	}
	for _, row := range cells {
		for i, cell := range row {
			widths[i] = max(widths[i], utf8.RuneCountInString(cell))
		}
	}
	print := func(row [8]string) {
		text := make([]string, 0, len(row))
		for i, cell := range row {
			text = append(text, fmt.Sprintf("%-*s", widths[i], cell))
		}
		fmt.Fprintln(os.Stdout, strings.TrimRightFunc(strings.Join(text, "  "), unicode.IsSpace))
	}
	print(header)
	for _, row := range cells {
		print(row)
	}
}

func orDash(text string) string {
	if text == "" {
		return "-"
	}
	return text
}

// Duration is a short human duration: `45s`, `12m`, `3h05m`, `2d04h`.
func Duration(secs int64) string {
	secs = max(secs, 0)
	switch {
	case secs < 60:
		return fmt.Sprintf("%ds", secs)
	case secs < 3600:
		return fmt.Sprintf("%dm", secs/60)
	case secs < 86400:
		return fmt.Sprintf("%dh%02dm", secs/3600, secs%3600/60)
	default:
		return fmt.Sprintf("%dd%02dh", secs/86400, secs%86400/3600)
	}
}

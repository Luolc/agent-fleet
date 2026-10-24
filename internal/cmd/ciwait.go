// `fleet ci wait`: wait for a GitHub Actions run with one watcher per run
// on the machine. The caller holding the run's lock is the watcher and the
// only one to ask GitHub; it writes what it saw to the run's record, which
// every other caller reads.

package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/gh"
)

// CIAbout, CIWaitAbout and CIWaitLongAbout are the help texts of `ci` and
// `ci wait`.
const (
	CIAbout         = "Wait for GitHub Actions runs, one watcher per run on the machine"
	CIWaitAbout     = "Wait for a GitHub Actions run to end; one caller per run asks GitHub, the others read its record"
	CIWaitLongAbout = "Wait for a GitHub Actions run to end; one caller per run asks GitHub, the others read its record.\n\n" +
		"The run is --run, or the newest run of --workflow (its file name, such as ci.yml, or its id) for the " +
		"commit --sha, of the event --event when given, as GitHub lists them when looked up; a run of the SHA that " +
		"has not appeared yet is waited for up to 10 minutes, and a lookup answers the callers that come within a " +
		"minute of it, a later caller looking up again. On this machine, whatever the scope, one caller per run is its watcher: the one holding " +
		"the lock $XDG_STATE_HOME/fleet/ci/<OWNER>/<NAME>/run-<ID>.lock (a SHA's lookup has a lock of its own " +
		"beside it). Only the watcher asks GitHub, through `gh api` as the account gh's configuration and " +
		"environment select; it writes what it saw to run-<ID>.json beside the lock after every look. Every other " +
		"caller reads that record each second and asks GitHub nothing. The lock goes with the process, so when " +
		"the watcher ends or is killed, the next caller to try takes it over.\n\n" +
		"The watcher looks at the run, and at the jobs of its latest attempt, every --interval (default 30s, at " +
		"least 15s): two requests a look. GitHub's rate limit (HTTP 429, or 403 that says so) is waited out: as " +
		"long as Retry-After says, else until X-RateLimit-Reset when none remain, else 1 minute, doubled at each " +
		"hit in a row up to 16 minutes; every watcher on the machine holds off as long (the file " +
		"$XDG_STATE_HOME/fleet/ci/hold). A reply of 5xx, or a call with no reply, is tried again after --interval, " +
		"doubled each time up to 5 minutes, 5 tries in all; so is a call gh does not answer within 60 s, or within " +
		"what is left of --timeout.\n\n" +
		"The wait ends when the run completes, or as soon as a job of its latest attempt has failed, been " +
		"cancelled or timed out while the run still runs (a job with continue-on-error counts as failed here). " +
		"--attempt N waits for attempt N or later: after a re-run, pass the new attempt, or a record of the " +
		"earlier attempt is taken as the answer. The record is one JSON object: repo, run_id, workflow, head_sha, " +
		"event, status, conclusion, run_attempt, url, updated_at, ended_at (updated_at once completed), jobs (each " +
		"name, status, conclusion, completed_at), checked_at and watcher_pid; nothing from gh's output but these " +
		"fields, and no credential. --scope does not apply: a run's record is the machine's.\n\n" +
		"Prints `run <URL> attempt <N>: <conclusion>` (`failing` while the run still runs), then one line per job, " +
		"tab-separated: its name and its conclusion, or its status while it has none; --json prints the record " +
		"instead.\n\n" +
		"Exit: 0 when the run ended in success; 1 on a usage error; 2 when --timeout passed first (the run goes on, " +
		"and so does another caller's wait); 4 when there is no such run or workflow, or no run of the workflow " +
		"for the SHA appeared within 10 minutes; 5 when gh cannot be run, GitHub refused for another reason than " +
		"its rate limit, or kept failing, or the record cannot be written; 7 when the run ended in anything but " +
		"success, or a job failed while it still runs."
)

// Paces of `ci wait`. Variables so the test binary can shorten them.
var (
	// CIIntervalFloor is the shortest --interval.
	CIIntervalFloor = 15 * time.Second
	// CIRateLimitFloor is the wait at a rate limit that says no wait.
	CIRateLimitFloor = time.Minute
	// CIReadEvery is how often a caller that is not the watcher reads the record.
	CIReadEvery = time.Second
	// CIRunAppears is how long a lookup by SHA waits for the run to appear.
	CIRunAppears = 10 * time.Minute
	// CILookupFresh is how long a lookup by SHA answers later callers.
	CILookupFresh = time.Minute
)

const (
	ciDefaultInterval = 30 * time.Second
	ciRateLimitCap    = 16 * time.Minute
	ciErrorCap        = 5 * time.Minute
	ciErrorTries      = 5
)

// CIWaitArgs are the arguments of `ci wait`: the repo and either the run
// (Run > 0) or a commit, workflow and optional event to find it by.
type CIWaitArgs struct {
	Repo     string
	Run      int64
	SHA      string
	Workflow string
	Event    string
	Attempt  int
	Timeout  time.Duration
	Interval time.Duration
	JSON     bool
}

var (
	ciRepo     = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9_.-]+$`)
	ciSHA      = regexp.MustCompile(`^[0-9a-f]{40}$`)
	ciWorkflow = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]*$`)
	ciEvent    = regexp.MustCompile(`^[a-z_]+$`)
)

// ciJob is a job of the run's latest attempt.
type ciJob struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	CompletedAt string `json:"completed_at"`
}

// ciRecord is the run as the watcher last saw it.
type ciRecord struct {
	Repo       string  `json:"repo"`
	RunID      int64   `json:"run_id"`
	Workflow   string  `json:"workflow"`
	HeadSHA    string  `json:"head_sha"`
	Event      string  `json:"event"`
	Status     string  `json:"status"`
	Conclusion string  `json:"conclusion"`
	RunAttempt int     `json:"run_attempt"`
	URL        string  `json:"url"`
	UpdatedAt  string  `json:"updated_at"`
	EndedAt    string  `json:"ended_at"`
	Jobs       []ciJob `json:"jobs"`
	CheckedAt  string  `json:"checked_at"`
	WatcherPID int     `json:"watcher_pid"`
}

// failedJobs are the jobs that ended in failure.
func (r *ciRecord) failedJobs() []string {
	var failed []string
	for _, j := range r.Jobs {
		switch j.Conclusion {
		case "failure", "cancelled", "timed_out", "startup_failure":
			failed = append(failed, j.Name)
		}
	}
	return failed
}

// settles says whether the record answers a wait for `attempt` or later.
func (r *ciRecord) settles(attempt int) bool {
	return r.RunAttempt >= attempt && (r.Status == "completed" || len(r.failedJobs()) > 0)
}

// ciWait is one caller's wait.
type ciWait struct {
	args     CIWaitArgs
	root     string
	dir      string
	deadline time.Time
	// limited counts the rate limits hit in a row.
	limited int
	// last is what the caller last knew of the run, for a timeout's message.
	last string
}

// CIWait runs `ci wait`.
func CIWait(stdout io.Writer, args CIWaitArgs) (exit.Code, error) {
	if err := checkCIWait(&args); err != nil {
		return 0, err
	}
	root, err := ciRoot()
	if err != nil {
		return 0, err
	}
	owner, name, _ := strings.Cut(args.Repo, "/")
	w := &ciWait{args: args, root: root, dir: filepath.Join(root, owner, name), last: "nothing seen yet"}
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		return 0, exit.IO(err)
	}
	if args.Timeout > 0 {
		w.deadline = time.Now().Add(args.Timeout)
	}
	run := args.Run
	if run == 0 {
		if run, err = w.resolve(); err != nil {
			return 0, err
		}
	}
	data, err := w.shared(filepath.Join(w.dir, fmt.Sprintf("run-%d", run)), func(data []byte) bool {
		var rec ciRecord
		if json.Unmarshal(data, &rec) != nil {
			return false
		}
		w.last = fmt.Sprintf("run %s attempt %d %s, by its record", rec.URL, rec.RunAttempt, rec.Status)
		return rec.settles(args.Attempt)
	}, func(base string) ([]byte, error) { return w.watchRun(base, run) })
	if err != nil {
		return 0, err
	}
	var rec ciRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return 0, exit.Environmentf("the record of run %d is not its JSON: %v", run, err)
	}
	if args.JSON {
		fmt.Fprintf(stdout, "%s\n", data)
	} else {
		outcome := rec.Conclusion
		if rec.Status != "completed" {
			outcome = "failing"
		}
		fmt.Fprintf(stdout, "run %s attempt %d: %s\n", rec.URL, rec.RunAttempt, outcome)
		for _, j := range rec.Jobs {
			state := j.Conclusion
			if state == "" {
				state = j.Status
			}
			fmt.Fprintf(stdout, "%s\t%s\n", j.Name, state)
		}
	}
	if rec.Status == "completed" && rec.Conclusion == "success" {
		return exit.Ok, nil
	}
	return exit.Failed, nil
}

// checkCIWait refuses arguments that do not name one run, and fills in the
// defaults.
func checkCIWait(args *CIWaitArgs) error {
	owner, name, _ := strings.Cut(args.Repo, "/")
	if !ciRepo.MatchString(args.Repo) || name == "." || name == ".." || owner == "" {
		return exit.Refusedf("--repo %q is not OWNER/NAME", args.Repo)
	}
	byRun := args.Run != 0
	bySHA := args.SHA != "" || args.Workflow != "" || args.Event != ""
	switch {
	case byRun && bySHA:
		return exit.Refusedf("--run names the run: --sha, --workflow and --event go only without it")
	case byRun && args.Run < 0:
		return exit.Refusedf("--run %d is not a run id", args.Run)
	case !byRun && (args.SHA == "" || args.Workflow == ""):
		return exit.Refusedf("give --run, or --sha and --workflow")
	case !byRun && !ciSHA.MatchString(args.SHA):
		return exit.Refusedf("--sha %q is not a full commit SHA (40 lower-case hex digits)", args.SHA)
	case !byRun && !ciWorkflow.MatchString(args.Workflow):
		return exit.Refusedf("--workflow %q is not a workflow's file name or id", args.Workflow)
	case args.Event != "" && !ciEvent.MatchString(args.Event):
		return exit.Refusedf("--event %q is not an event's name", args.Event)
	}
	if args.Attempt == 0 {
		args.Attempt = 1
	}
	if args.Attempt < 0 {
		return exit.Refusedf("--attempt %d is not an attempt number", args.Attempt)
	}
	if args.Interval == 0 {
		args.Interval = ciDefaultInterval
	}
	if args.Interval < CIIntervalFloor {
		return exit.Refusedf("--interval %v is below the shortest, %v", args.Interval, CIIntervalFloor)
	}
	if args.Timeout < 0 {
		return exit.Refusedf("--timeout %v is negative", args.Timeout)
	}
	return nil
}

// ciRoot is `$XDG_STATE_HOME/fleet/ci`, with `~/.local/state` when
// XDG_STATE_HOME is unset or empty.
func ciRoot() (string, error) {
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home := os.Getenv("HOME")
		if home == "" {
			return "", exit.Environmentf("neither XDG_STATE_HOME nor HOME is set, cannot locate the records of CI runs")
		}
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, "fleet", "ci"), nil
}

// shared waits until the file `<base>.json` settles: it reads the file
// every CIReadEvery and, whenever no other process holds `<base>.lock`,
// takes the lock and runs watch, which asks GitHub until it has written a
// settled file and returns it.
func (w *ciWait) shared(base string, settled func([]byte) bool, watch func(base string) ([]byte, error)) ([]byte, error) {
	for {
		if data, ok, err := readSettled(base, settled); err != nil || ok {
			return data, err
		}
		unlock, busy, err := tryLock(base + ".lock")
		if err != nil {
			return nil, err
		}
		if !busy {
			// The last watcher may have written the answer just before it let go.
			data, ok, err := readSettled(base, settled)
			if err == nil && !ok {
				data, err = watch(base)
			}
			unlock()
			return data, err
		}
		if err := w.sleep(CIReadEvery); err != nil {
			return nil, err
		}
	}
}

// readSettled reads `<base>.json` and says whether it settles; a missing
// file does not.
func readSettled(base string, settled func([]byte) bool) ([]byte, bool, error) {
	data, err := os.ReadFile(base + ".json")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, exit.IO(err)
	}
	return data, settled(data), nil
}

// tryLock takes the lock file at path, or says another process holds it.
// The lock goes with the process, so a killed watcher leaves none behind.
func tryLock(path string) (unlock func(), busy bool, err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, exit.IO(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, true, nil
		}
		return nil, false, exit.IO(err)
	}
	return func() { _ = f.Close() }, false, nil
}

// writeFile replaces path with data at once, so a reader never sees half
// of it.
func writeFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return exit.IO(err)
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return exit.IO(err)
	}
	return nil
}

// sleep waits d, or fails with exit 2 once the deadline passes.
func (w *ciWait) sleep(d time.Duration) error {
	if !w.deadline.IsZero() {
		left := time.Until(w.deadline)
		if left <= d {
			time.Sleep(max(left, 0))
			return w.timedOut()
		}
	}
	time.Sleep(d)
	return nil
}

// timedOut is the exit 2 failure of a wait past its --timeout.
func (w *ciWait) timedOut() error {
	return exit.New(exit.Unknown, fmt.Sprintf("timed out after %v; last seen: %s", w.args.Timeout, w.last))
}

// ciLookup is what a lookup by SHA found, and when.
type ciLookup struct {
	RunID     int64 `json:"run_id"`
	CheckedAt int64 `json:"checked_at_ms"`
}

// resolve finds the run of the SHA, shared as a run is: one caller looks it
// up and writes its id to `sha-<SHA>-<workflow>@<event>.json` (no event
// after the `@` without --event), which answers the callers that come
// within CILookupFresh; a later one looks up again, since a newer run of
// the SHA may have started.
func (w *ciWait) resolve() (int64, error) {
	base := filepath.Join(w.dir, fmt.Sprintf("sha-%s-%s@%s", w.args.SHA, w.args.Workflow, w.args.Event))
	data, err := w.shared(base, func(data []byte) bool {
		var found ciLookup
		return json.Unmarshal(data, &found) == nil && found.RunID > 0 &&
			time.Since(time.UnixMilli(found.CheckedAt)) < CILookupFresh
	}, w.lookUp)
	if err != nil {
		return 0, err
	}
	var found ciLookup
	_ = json.Unmarshal(data, &found)
	return found.RunID, nil
}

// lookUp asks GitHub for the newest run of the workflow for the SHA until
// one appears, and writes its id.
func (w *ciWait) lookUp(base string) ([]byte, error) {
	gone := time.Now().Add(CIRunAppears)
	for {
		newest, err := w.newestRun()
		if err != nil {
			return nil, err
		}
		if newest > 0 {
			data, _ := json.Marshal(ciLookup{RunID: newest, CheckedAt: time.Now().UnixMilli()})
			if err := writeFile(base+".json", data); err != nil {
				return nil, err
			}
			return data, nil
		}
		w.last = fmt.Sprintf("no run of %s for %s yet", w.args.Workflow, w.args.SHA)
		if time.Now().Add(w.args.Interval).After(gone) {
			return nil, exit.New(exit.NotFound, fmt.Sprintf("no run of workflow %s in %s for %s appeared within %v",
				w.args.Workflow, w.args.Repo, w.args.SHA, CIRunAppears))
		}
		if err := w.sleep(w.args.Interval); err != nil {
			return nil, err
		}
	}
}

// newestRun is the id of the newest run of the workflow for the SHA, 0
// while there is none.
func (w *ciWait) newestRun() (int64, error) {
	path := fmt.Sprintf("repos/%s/actions/workflows/%s/runs?head_sha=%s&per_page=20", w.args.Repo, w.args.Workflow, w.args.SHA)
	if w.args.Event != "" {
		path += "&event=" + w.args.Event
	}
	resp, err := w.get(path)
	if err != nil {
		return 0, err
	}
	if resp.Status == 404 {
		return 0, exit.New(exit.NotFound, fmt.Sprintf("GitHub has no workflow %s in %s", w.args.Workflow, w.args.Repo))
	}
	if resp.Status != 200 {
		return 0, refusal(resp)
	}
	var list struct {
		Runs []struct {
			ID int64 `json:"id"`
		} `json:"workflow_runs"`
	}
	if err := json.Unmarshal(resp.Body, &list); err != nil {
		return 0, exit.Environmentf("GitHub's list of runs is not its JSON: %v", err)
	}
	var newest int64
	for _, r := range list.Runs {
		newest = max(newest, r.ID)
	}
	return newest, nil
}

// watchRun looks at the run every interval and writes the record, until
// the record settles.
func (w *ciWait) watchRun(base string, run int64) ([]byte, error) {
	for {
		rec, err := w.look(run)
		if err != nil {
			return nil, err
		}
		data, _ := json.Marshal(rec)
		if err := writeFile(base+".json", data); err != nil {
			return nil, err
		}
		w.last = fmt.Sprintf("run %s attempt %d %s", rec.URL, rec.RunAttempt, rec.Status)
		if rec.settles(w.args.Attempt) {
			return data, nil
		}
		if err := w.sleep(w.args.Interval); err != nil {
			return nil, err
		}
	}
}

// look asks GitHub for the run, and for its jobs once it is at the attempt
// waited for.
func (w *ciWait) look(run int64) (*ciRecord, error) {
	resp, err := w.get(fmt.Sprintf("repos/%s/actions/runs/%d", w.args.Repo, run))
	if err != nil {
		return nil, err
	}
	if resp.Status == 404 {
		return nil, exit.New(exit.NotFound, fmt.Sprintf("GitHub has no run %d in %s", run, w.args.Repo))
	}
	if resp.Status != 200 {
		return nil, refusal(resp)
	}
	var r struct {
		ID         int64   `json:"id"`
		Name       string  `json:"name"`
		HeadSHA    string  `json:"head_sha"`
		Event      string  `json:"event"`
		Status     string  `json:"status"`
		Conclusion *string `json:"conclusion"`
		RunAttempt int     `json:"run_attempt"`
		HTMLURL    string  `json:"html_url"`
		UpdatedAt  string  `json:"updated_at"`
	}
	if err := json.Unmarshal(resp.Body, &r); err != nil || r.ID != run {
		return nil, exit.Environmentf("GitHub's reply about run %d is not the run's JSON", run)
	}
	rec := &ciRecord{Repo: w.args.Repo, RunID: run, Workflow: r.Name, HeadSHA: r.HeadSHA, Event: r.Event,
		Status: r.Status, RunAttempt: r.RunAttempt, URL: r.HTMLURL, UpdatedAt: r.UpdatedAt, Jobs: []ciJob{},
		WatcherPID: os.Getpid()}
	if r.Conclusion != nil {
		rec.Conclusion = *r.Conclusion
	}
	if r.Status == "completed" {
		rec.EndedAt = r.UpdatedAt
	}
	if r.RunAttempt >= w.args.Attempt {
		if rec.Jobs, err = w.jobs(run); err != nil {
			return nil, err
		}
	}
	rec.CheckedAt = time.Now().UTC().Format(time.RFC3339)
	return rec, nil
}

// jobs are the jobs of the run's latest attempt.
func (w *ciWait) jobs(run int64) ([]ciJob, error) {
	resp, err := w.get(fmt.Sprintf("repos/%s/actions/runs/%d/jobs?per_page=100", w.args.Repo, run))
	if err != nil {
		return nil, err
	}
	if resp.Status != 200 {
		return nil, refusal(resp)
	}
	var list struct {
		Jobs []struct {
			Name        string  `json:"name"`
			Status      string  `json:"status"`
			Conclusion  *string `json:"conclusion"`
			CompletedAt *string `json:"completed_at"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(resp.Body, &list); err != nil {
		return nil, exit.Environmentf("GitHub's list of the jobs of run %d is not its JSON: %v", run, err)
	}
	jobs := []ciJob{}
	for _, j := range list.Jobs {
		job := ciJob{Name: j.Name, Status: j.Status}
		if j.Conclusion != nil {
			job.Conclusion = *j.Conclusion
		}
		if j.CompletedAt != nil {
			job.CompletedAt = *j.CompletedAt
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

// refusal is the failure for a reply GitHub gave instead of the answer.
func refusal(resp *gh.Response) error {
	return exit.Environmentf("GitHub answered HTTP %d: %s", resp.Status, resp.Message())
}

// get asks GitHub once the machine's hold-off is over, waiting out its
// rate limit and trying again after a 5xx or no reply; any other reply is
// returned as it is.
func (w *ciWait) get(path string) (*gh.Response, error) {
	failures := 0
	for {
		if err := w.holdOff(); err != nil {
			return nil, err
		}
		resp, reason, err := w.try(path)
		switch {
		case err != nil:
			return nil, err
		case resp != nil:
			return resp, nil
		case reason != "":
			failures++
			if err := w.retry(reason, failures); err != nil {
				return nil, err
			}
		}
	}
}

// try asks GitHub once: the reply, or why to try again, or neither after a
// rate limit, which the hold-off waits out.
func (w *ciWait) try(path string) (*gh.Response, string, error) {
	timeout := gh.CallTimeout
	if !w.deadline.IsZero() {
		left := time.Until(w.deadline)
		if left <= 0 {
			return nil, "", w.timedOut()
		}
		timeout = min(timeout, left)
	}
	resp, err := gh.Get(path, timeout)
	var none *gh.NoResponse
	if errors.As(err, &none) {
		return nil, none.Message, nil
	}
	if err != nil {
		return nil, "", err
	}
	if resp.Status >= 500 {
		return nil, fmt.Sprintf("GitHub answered HTTP %d: %s", resp.Status, resp.Message()), nil
	}
	if limited, err := w.rateLimited(resp); limited || err != nil {
		return nil, "", err
	}
	w.limited = 0
	return resp, "", nil
}

// retry waits before the next try after a failed one, the interval doubled
// at each failure in a row, or gives up after the last try.
func (w *ciWait) retry(reason string, failures int) error {
	if failures >= ciErrorTries {
		return exit.Environmentf("%s (%d tries)", reason, failures)
	}
	wait := min(w.args.Interval<<(failures-1), ciErrorCap)
	if !w.deadline.IsZero() && time.Until(w.deadline) <= wait {
		return w.sleep(wait)
	}
	fmt.Fprintf(os.Stderr, "fleet: %s; trying again in %v\n", reason, wait)
	return w.sleep(wait)
}

// rateLimited says whether the reply is GitHub's rate limit and, when it
// is, holds the machine off for as long as it asks, else for the floor
// doubled at each hit in a row.
func (w *ciWait) rateLimited(resp *gh.Response) (bool, error) {
	now := time.Now()
	limited, wait := resp.RateLimited(now)
	if !limited {
		return false, nil
	}
	w.limited++
	if wait == 0 {
		wait = min(CIRateLimitFloor<<min(w.limited-1, 10), ciRateLimitCap)
	}
	if err := w.hold(now.Add(wait)); err != nil {
		return true, err
	}
	fmt.Fprintf(os.Stderr, "fleet: GitHub's rate limit (HTTP %d), waiting %v\n", resp.Status, wait)
	return true, nil
}

// holdPath is the machine's hold-off: until when no watcher asks GitHub.
func (w *ciWait) holdPath() string {
	return filepath.Join(w.root, "hold")
}

// hold makes the machine's hold-off last at least until `until`, under
// `hold.lock` so two watchers rate-limited at once keep the longer wait.
func (w *ciWait) hold(until time.Time) error {
	f, err := os.OpenFile(w.holdPath()+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return exit.IO(err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return exit.IO(err)
	}
	if current, ok := w.holdUntil(); ok && current.After(until) {
		return nil
	}
	return writeFile(w.holdPath(), []byte(strconv.FormatInt(until.UnixMilli(), 10)+"\n"))
}

// holdUntil reads the machine's hold-off; a missing or unreadable file is
// none.
func (w *ciWait) holdUntil() (time.Time, bool) {
	data, err := os.ReadFile(w.holdPath())
	if err != nil {
		return time.Time{}, false
	}
	ms, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.UnixMilli(ms), true
}

// holdOff waits until the machine's hold-off is over.
func (w *ciWait) holdOff() error {
	until, ok := w.holdUntil()
	if !ok {
		return nil
	}
	if left := time.Until(until); left > 0 {
		return w.sleep(left)
	}
	return nil
}

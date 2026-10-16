// `fleet watch`'s configured watches: command sources and `tell`, against
// the fakes of watch_test.go.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sourceWorld is a thread watch world whose scope config has `watches`,
// with a command source at <dir>/source: it copies its stdin to
// <dir>/source-in.json, the names of its environment to <dir>/source-env
// and prints <dir>/subjects; with <dir>/source-fail it prints that file
// on stderr and exits 3. The ledger has the thread C1/1.0 with its live
// agent thread-c1, and the open job j reporting to it with its live lead
// j-lead.
func sourceWorld(t *testing.T, watches string, prompts map[string]string) *watchWorld {
	t.Helper()
	w := watchThreadWorld(t)
	source := `#!/bin/sh
dir='` + w.dir + `'
cat > "$dir/source-in.json"
env | sed 's/=.*//' | sort > "$dir/source-env"
if [ -f "$dir/source-fail" ]; then cat "$dir/source-fail" >&2; exit 3; fi
cat "$dir/subjects" 2>/dev/null
exit 0
`
	config := filepath.Join(w.dir, "home", ".config", "fleet")
	files := map[string]string{filepath.Join(w.dir, "source"): source,
		filepath.Join(config, scope+".json"): `{"fednet": {"socket": "/run/example/fednet.sock"}, "watches": ` +
			strings.ReplaceAll(watches, "SOURCE", filepath.Join(w.dir, "source")) + `}`}
	for name, body := range prompts {
		files[filepath.Join(config, name)] = body
	}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w.exec("INSERT INTO threads (thread, slug, created_at) VALUES ('C1/1.0', 'c1-1-0', 0)",
		"INSERT INTO agents (name, role, thread, pane_id, state, started_at) VALUES ('thread-c1', 'thread', 'C1/1.0', 'p1', 'active', 0)",
		"INSERT INTO jobs (job, lead_cwd, home_thread, state, started_at) VALUES ('j', '/c', 'C1/1.0', 'open', 5)")
	w.insert(watchRow{name: "j-lead", role: "lead", job: "j", parent: "thread-c1"})
	w.herdrList("thread-c1 idle 1", "j-lead idle 1")
	w.screen("j-lead", claude("⏺ Waiting", "1s", "2hr 59m"))
	w.lastMessage("C1/1.0", t0)
	return w
}

// subjects sets what the source prints.
func (w *watchWorld) subjects(lines ...string) {
	w.t.Helper()
	w.file("subjects", strings.Join(lines, "\n")+"\n")
}

// told is the targets of the prompts from `from` on, each with the
// subject keys named in its text.
func (w *watchWorld) told(from int) []string {
	var got []string
	for _, p := range w.prompts()[from:] {
		_, rest, _ := strings.Cut(p[1], ", subject `")
		key, _, _ := strings.Cut(rest, "`")
		got = append(got, p[0]+":"+key)
	}
	return got
}

func TestACommandWatchTellsTheAgentEachSubjectConcerns(t *testing.T) {
	w := sourceWorld(t, `{"ci": {"source": {"command": ["SOURCE", "--all"]}, "do": "tell", "prompt": "ci.md",
		"env": ["EXAMPLE_TOKEN"], "per_run": 3}}`, map[string]string{"ci.md": "Look at {{key}} first.\n"})
	w.subjects(`{"key": "a", "text": "PR 1 is red", "agent": "thread-c1"}`,
		`{"key": "b", "text": "PR 2 waits", "job": "j"}`,
		`{"key": "c", "text": "the thread has news", "thread": "C1/1.0"}`)

	w.watchAt(0, 0, "EXAMPLE_TOKEN=tok-example", "EXAMPLE_OTHER=x")
	if got := strings.Join(w.told(0), " "); got != "thread-c1:a j-lead:b thread-c1:c" {
		t.Fatalf("told %s: %q", got, w.prompts())
	}
	want := "[FROM: watch]\nfleet watch, watch `ci`, subject `b`:\n\nPR 2 waits\n\nLook at b first."
	if got := w.prompts()[1][1]; got != want {
		t.Errorf("message:\n%s\nwant:\n%s", got, want)
	}
	var input struct {
		Version int    `json:"version"`
		Scope   string `json:"scope"`
		Now     int64  `json:"now"`
		Jobs    []struct {
			Job, State, HomeThread string
			EndedAt                *int64 `json:"ended_at"`
		} `json:"jobs"`
	}
	data, _ := os.ReadFile(filepath.Join(w.dir, "source-in.json"))
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	if input.Version != 1 || input.Scope != scope || input.Now != t0 || len(input.Jobs) != 1 || input.Jobs[0].Job != "j" ||
		input.Jobs[0].State != "open" || input.Jobs[0].EndedAt != nil {
		t.Errorf("stdin %s", data)
	}
	env, _ := os.ReadFile(filepath.Join(w.dir, "source-env"))
	if !strings.Contains(string(env), "EXAMPLE_TOKEN\n") || strings.Contains(string(env), "EXAMPLE_OTHER") {
		t.Errorf("the source's environment:\n%s", env)
	}
	if got := w.query("SELECT group_concat(key || ':' || acts, ' ') FROM watch_subjects WHERE watch = 'ci'"); got != "a:1 b:1 c:1" {
		t.Errorf("subjects %s", got)
	}

	// The same texts: nobody is told again; a changed text is told again.
	w.watchAt(5*time.Minute, 0)
	if got := len(w.prompts()); got != 3 {
		t.Fatalf("told again: %q", w.told(3))
	}
	w.subjects(`{"key": "b", "text": "PR 2 still waits, now with a conflict", "job": "j"}`,
		`{"key": "c", "text": "the thread has news", "thread": "C1/1.0"}`)
	w.watchAt(10*time.Minute, 0)
	if got := strings.Join(w.told(3), " "); got != "j-lead:b" {
		t.Errorf("told %s", got)
	}
	// The subject its source no longer gives is dropped.
	if got := w.query("SELECT group_concat(key || ':' || acts, ' ') FROM watch_subjects WHERE watch = 'ci'"); got != "b:2 c:1" {
		t.Errorf("subjects %s", got)
	}
}

func TestACommandWatchKeepsItsLimitsAndAsksThePeopleOnce(t *testing.T) {
	w := sourceWorld(t, `{"nag": {"source": {"command": ["SOURCE"]}, "do": "tell", "every": "5m", "again": "1h", "max": 2},
		"quiet-threads": {"off": true}}`, nil)
	w.subjects(`{"key": "x", "text": "x is late", "agent": "j-lead"}`, `{"key": "y", "text": "y is late", "agent": "j-lead"}`)
	for _, step := range []struct {
		at   time.Duration
		told string
	}{
		{0, "j-lead:x"},       // one a run
		{4 * time.Minute, ""}, // `every` has not passed
		{5 * time.Minute, "j-lead:y"},
		{30 * time.Minute, ""}, // `again` has not passed for either
		{time.Hour, "j-lead:x"},
		{time.Hour + 5*time.Minute, "j-lead:y"},
	} {
		before := len(w.prompts())
		w.watchAt(step.at, 0)
		if got := strings.Join(w.told(before), " "); got != step.told {
			t.Fatalf("at +%v told [%s], want [%s]: %q", step.at, got, step.told, w.prompts()[before:])
		}
	}
	// Two acts each: the people are asked once instead, one a run.
	before := len(w.prompts())
	w.watchAt(2*time.Hour+5*time.Minute, 0)
	w.watchAt(2*time.Hour+10*time.Minute, 0)
	w.watchAt(4*time.Hour, 0)
	if got := w.query("SELECT group_concat(asked_by || ' ' || job || ' ' || thread || ' ' || state, ', ') FROM questions"); got != "watch  C1/1.0 pending, watch  C1/1.0 pending" {
		t.Errorf("questions %s", got)
	}
	if got := w.log("fednet.log"); !strings.Contains(got, "client ask -socket /run/example/fednet.sock -thread C1/1.0 -ref example:1 ") ||
		!strings.Contains(got, "card {\"question\":\"fleet watch `nag`: subject `x` had the watch's max of 2 act(s)\","+
			"\"context\":\"Its source still gives it:\\n\\nx is late\\n\\n") ||
		!strings.Contains(got, `"options":[{"short":"Leave it","long":"fleet does nothing more about it while its source gives it.",`+
			`"recommended":true},{"short":"Look into it","long":"The thread's agent looks into it and reports here."}]}`) {
		t.Errorf("fednet: %s", got)
	}
	if got := w.query("SELECT group_concat(card || ' ' || card_id, ', ') FROM questions"); got != "1 card-1, 1 card-2" {
		t.Errorf("cards %s", got)
	}
	prompts := w.prompts()[before:]
	if len(prompts) != 2 || prompts[0][0] != "thread-c1" || !strings.Contains(prompts[0][1], "Nothing to pass on") ||
		!strings.Contains(prompts[0][1], "Card: https://slack.example.test/card-1") {
		t.Errorf("%q", prompts)
	}
	// The questions are reminded of but never time out.
	w.watchAt(80*time.Hour, 0)
	if got := w.query("SELECT group_concat(state, ' ') FROM questions"); got != "pending pending" {
		t.Errorf("questions %s", got)
	}
}

func TestAWatchWhoseSourceFailsIsSkippedAndTheOthersRun(t *testing.T) {
	w := sourceWorld(t, `{"bad": {"source": {"command": ["SOURCE"]}, "do": "tell", "env": ["EXAMPLE_TOKEN"]},
		"good": {"source": {"command": ["sh", "-c", "echo '{\"key\": \"g\", \"text\": \"fine\", \"agent\": \"j-lead\"}'"]}, "do": "tell"}}`, nil)
	w.file("source-fail", "cannot log in with tok-example-secret\n")
	out := w.watchAt(0, 5, "EXAMPLE_TOKEN=tok-example-secret")
	if !strings.Contains(out.stderr, "fleet watch: watch bad skipped: its source exited 3: cannot log in with [redacted EXAMPLE_TOKEN]") ||
		strings.Contains(out.stderr, "tok-example-secret") {
		t.Errorf("stderr: %s", out.stderr)
	}
	if got := strings.Join(w.told(0), " "); got != "j-lead:g" {
		t.Errorf("told %s", got)
	}
	// A value across the 200 bytes kept of the line is taken out first.
	w.file("source-fail", strings.Repeat("x", 190)+" with tok-example-secret\n")
	out = w.watchAt(30*time.Second, 5, "EXAMPLE_TOKEN=tok-example-secret")
	if !strings.Contains(out.stderr, strings.Repeat("x", 190)+" with [re") || strings.Contains(out.stderr, "tok-") {
		t.Errorf("stderr: %s", out.stderr)
	}
	// Output that is not one subject per line fails it the same way.
	w.file("source-fail", "")
	w.subjects(`{"key": "a", "text": "no target"}`)
	out = w.watchAt(time.Minute, 5)
	if !strings.Contains(out.stderr, "watch bad skipped: its source's line 1 names no agent, job or thread to tell") {
		t.Errorf("stderr: %s", out.stderr)
	}
	w.subjects(`{"key": "a", "text": "PR 1", "agent": "j-lead", "url": "https://example.test"}`)
	out = w.watchAt(2*time.Minute, 5)
	if !strings.Contains(out.stderr, `its source's line 1 is not a subject: json: unknown field "url"`) {
		t.Errorf("stderr: %s", out.stderr)
	}
	if got := w.query("SELECT count(*) FROM watch_subjects WHERE watch = 'bad'"); got != "0" {
		t.Errorf("subjects of the failed source: %s", got)
	}
	// A tell that did not land is tried again next run.
	w.subjects(`{"key": "a", "text": "PR 1", "agent": "j-lead"}`)
	w.reply(`{"error":{"code":"agent_not_found","message":"gone"}}`)
	w.watchAt(3*time.Minute, 4)
	w.reply(`{"result":{"type":"agent_prompted"}}`)
	before := len(w.prompts())
	w.watchAt(4*time.Minute, 0)
	if got := strings.Join(w.told(before), " "); got != "j-lead:a" {
		t.Errorf("told %s", got)
	}
}

func TestADefaultWatchCanBeTurnedOffOrGivenAPrompt(t *testing.T) {
	w := sourceWorld(t, `{"stale-workers": {"off": true}, "quiet-threads": {"prompt": "quiet.md"}}`,
		map[string]string{"quiet.md": "Post a line in {{key}}.\n"})
	w.insert(watchRow{name: "j-w", role: "worker", job: "j", parent: "j-lead"})
	out := w.watchAt(30*time.Minute, 0)
	if !strings.Contains(out.stdout, "suspect: j-w (worker, job j, parent j-lead): gone from herdr") {
		t.Errorf("stdout: %s", out.stdout)
	}
	prompts := w.prompts()
	if len(prompts) != 1 || prompts[0][0] != "thread-c1" {
		t.Fatalf("%q", prompts)
	}
	if !strings.Contains(prompts[0][1], "rule `thread quiet, jobs open`") || !strings.HasSuffix(prompts[0][1], "\n\nPost a line in C1/1.0.") {
		t.Errorf("%s", prompts[0][1])
	}
}

func TestADefaultWatchWithABrokenPromptIsTakenUpAgainNextRun(t *testing.T) {
	w := sourceWorld(t, `{"stale-workers": {"every": "1h", "prompt": "stale.md"}, "quiet-threads": {"off": true}}`, nil)
	w.insert(watchRow{name: "j-w", role: "worker", job: "j", parent: "j-lead"})
	out := w.watchAt(0, 1)
	if !strings.Contains(out.stderr, "watches.stale-workers.prompt (") || len(w.prompts()) != 0 {
		t.Fatalf("%+v %q", out, w.prompts())
	}
	w.file("home/.config/fleet/stale.md", "Look at {{key}}'s workers.\n")
	w.watchAt(5*time.Minute, 0)
	if prompts := w.prompts(); len(prompts) != 1 || prompts[0][0] != "j-lead" || !strings.HasSuffix(prompts[0][1], "Look at j-lead's workers.") {
		t.Errorf("%q", prompts)
	}
}

// The keys of the rules that left fleet are accepted and named on each
// run; the rest of the run goes on.
func TestWatchNamesTheRetiredKeysAndGoesOn(t *testing.T) {
	w := sourceWorld(t, `{"quiet-threads": {"off": true}}`, nil)
	w.file("home/.config/fleet/"+scope+".json", `{"fednet": {"socket": "/run/example/fednet.sock"},
		"watch": {"parent_stale": "24h"}, "prompts": {"revisit": {"replace": "missing.md"}}}`)
	out := w.watchAt(0, 0)
	for _, key := range []string{"watch.parent_stale", "prompts.revisit"} {
		if !strings.Contains(out.stderr, "note: "+key+" in the scope's config is no longer used and is ignored") {
			t.Errorf("%s not named: %s", key, out.stderr)
		}
	}
}

// A screen helper live at the upgrade is closed as a watch worker is: its
// question, read where it wrote it, is asked about the agent it helped,
// and while that question waits no worker is started for the agent.
func TestWatchClosesAScreenHelperOfBeforeAndWaitsForItsQuestion(t *testing.T) {
	w := sourceWorld(t, `{"quiet-threads": {"off": true}}`, nil)
	w.insert(watchRow{name: "j-w", role: "worker", job: "j", parent: "j-lead"})
	id := w.query("SELECT id FROM agents WHERE name = 'j-w'")
	helper := "unblock-" + id
	w.exec("INSERT INTO agents (name, role, parent, pane_id, state, started_at) VALUES ('" + helper + "', 'unblock', 'j-w', 'p9', 'active', 0)")
	w.herdrList("thread-c1 idle 1", "j-lead idle 1", "j-w blocked 2", helper+" idle 3")
	w.screen("j-w", "⏺ fake reply\n"+rule+"\n Bash command\n\n   rm -rf build\n\n Do you want to proceed?\n ❯ 1. Yes\n   2. No\n")
	if err := os.MkdirAll(filepath.Join(w.dir, "home", ".local", "state", "fleet", scope+"-unblock"), 0o755); err != nil {
		t.Fatal(err)
	}
	w.file("home/.local/state/fleet/"+scope+"-unblock/"+helper+"-question.md", "Press 1? 0xQUESTION\n")

	out := w.watchAt(time.Minute, 0)
	if got := w.query("SELECT state FROM agents WHERE name = '" + helper + "'"); got != "ended" {
		t.Errorf("helper row %s", got)
	}
	if !strings.Contains(w.log("fednet.log"), `card {"question":"Press 1? 0xQUESTION","context":"j-w (a worker of job j, `+
		`lead j-lead) is stopped at a screen that fleet's guidance does not cover; its helper `+helper+` asks this.\n\n`+
		`Press 1? 0xQUESTION"}`) {
		t.Errorf("fednet: %s", w.log("fednet.log"))
	}
	if got := w.query("SELECT asked_by || ' ' || state FROM questions"); got != helper+" pending" {
		t.Errorf("question %s", got)
	}
	if !strings.Contains(out.stdout, "unblock: j-w is stopped at a screen; the people were asked and have not answered") {
		t.Errorf("stdout: %s", out.stdout)
	}
}

// A watch worker's question is reminded of but never times out, and the
// thread's session is not reclaimed for it. A watch worker is no job's:
// gone from herdr it is closed, never a suspect, and `watch` is told
// nothing.
func TestAWatchWorkersQuestionNeverTimesOutAndItIsNoSuspect(t *testing.T) {
	w := sourceWorld(t, `{"quiet-threads": {"off": true}}`, nil)
	w.exec(fmt.Sprintf("INSERT INTO questions (job, thread, asked_by, text, state, asked_at) VALUES "+
		"('', 'C1/1.0', 'watch-fix-p1', 'Rebase or close?', 'pending', %d)", t0),
		"INSERT INTO agents (name, role, parent, task, pane_id, state, started_at) VALUES "+
			"('watch-fix-p2', 'worker', 'watch', '{\"watch\": \"fix\", \"key\": \"p2\"}', 'p8', 'active', 0)")
	w.lastMessage("C1/1.0", t0+100*3600)
	out := w.watchAt(100*time.Hour, 0)
	if got := w.query("SELECT state || ' ' || reminders FROM questions"); got != "pending 3" {
		t.Errorf("question %s", got)
	}
	if got := w.query("SELECT coalesce(reclaim_at, 'none') FROM agents WHERE name = 'thread-c1'"); got != "none" {
		t.Errorf("reclaim_at %s", got)
	}
	if strings.Contains(out.stdout, "suspect: watch-fix-p2") || w.query("SELECT state FROM agents WHERE name = 'watch-fix-p2'") != "ended" {
		t.Errorf("stdout: %s", out.stdout)
	}
	for _, p := range w.prompts() {
		if p[0] == "watch" {
			t.Errorf("told watch: %q", p[1])
		}
	}
}

// fixWorker is the live worker watch-fix-p1 of the worker watch `fix`,
// done with its turn, its subject p1 of job j, with `question` in its
// question file.
func fixWorker(t *testing.T, question string) *watchWorld {
	t.Helper()
	w := sourceWorld(t, `{"fix": {"source": {"command": ["SOURCE"]}, "do": "worker"}, "quiet-threads": {"off": true}}`, nil)
	w.exec("INSERT INTO agents (name, role, parent, task, pane_id, state, started_at) VALUES ('watch-fix-p1', 'worker', " +
		"'watch', '{\"watch\": \"fix\", \"key\": \"p1\", \"job\": \"j\"}', 'p8', 'active', 0)")
	w.herdrList("thread-c1 idle 1", "j-lead idle 1", "watch-fix-p1 idle 8")
	dir := filepath.Join(w.dir, "home", ".local", "state", "fleet", scope+"-watch")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "watch-fix-p1-question.md"), []byte(question), 0o644); err != nil {
		t.Fatal(err)
	}
	return w
}

// A watch worker's question is asked on a card, the card it wrote with
// a line naming it before the context, and only a press on that card
// answers it: a message in the thread does not. The press is kept on the
// question and reaches the thread's agent, naming who asked.
func TestAWatchWorkersQuestionIsAnsweredOnlyByAPressOnItsCard(t *testing.T) {
	w := fixWorker(t, `{"question": "Rebase PR 9 or close it?", "context": "PR 9 conflicts.", `+
		`"options": [{"short": "Rebase", "recommended": true}, {"short": "Close"}]}`+"\n")
	w.watchAt(time.Minute, 0)
	if got := w.log("fednet.log"); !strings.Contains(got, "client ask -socket /run/example/fednet.sock -thread C1/1.0 -ref example:1 ") ||
		!strings.Contains(got, `card {"context":"watch-fix-p1, the worker of fleet watch `+"`fix`"+` for subject `+"`p1`"+
			`, asks this.\n\nPR 9 conflicts.","options":[{"short":"Rebase","recommended":true},{"short":"Close"}],`+
			`"question":"Rebase PR 9 or close it?"}`) {
		t.Errorf("fednet: %s", got)
	}
	if got := w.query("SELECT asked_by || ' ' || job || ' ' || state || ' ' || card || ' ' || card_id FROM questions"); got != "watch-fix-p1 j pending 1 card-1" {
		t.Errorf("question %s", got)
	}

	message := task(w.world, "m1.json", `{"msg_id":"m1","payload":{"type":"message","thread":"C1/1.0","text":"close it",`+
		`"user":"U0EXAMPLE1","ts":"1800000100.000","scope":"`+scope+`"}}`)
	if out := w.inbox(message); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if got := w.query("SELECT state FROM questions"); got != "pending" {
		t.Errorf("a message answered the card: %s", got)
	}

	before := len(w.prompts())
	press := `{"question_id":"` + scope + `:1","card_id":"card-1","outcome":"answered","choice":{"index":1,"short":"Close"},` +
		`"note":"it is stale","by":"U0EXAMPLE1","at":"2027-01-15T08:00:00Z"}`
	if out := w.inbox(w.answerEvent("a1", press)); out.code != 0 {
		t.Fatalf("%+v", out)
	}
	if got := w.query("SELECT state || ' ' || outcome || ' ' || answer FROM questions"); got != `answered answered {"question_id":1,`+
		`"card_id":"card-1","outcome":"answered","choice":{"index":1,"short":"Close"},"note":"it is stale","by":"U0EXAMPLE1",`+
		`"at":"2027-01-15T08:00:00Z"}` {
		t.Errorf("question %s", got)
	}
	prompts := w.prompts()[before:]
	if len(prompts) != 1 || prompts[0][0] != "thread-c1" || !strings.Contains(prompts[0][1], "[FROM: inbox]\n[ANSWER #1]\n"+
		"A press on the card fleet posted in thread C1/1.0 for watch-fix-p1 (question #1), not one of yours") {
		t.Errorf("%q", prompts)
	}
}

// A card fednet refuses is refused again: the worker is closed all the
// same, its question closed, and the run fails saying why.
func TestAWatchWorkersCardFednetRefusesClosesTheWorker(t *testing.T) {
	w := fixWorker(t, "Rebase PR 9 or close it? 0xQUESTION\n")
	w.file("card-refused", "x")
	out := w.watchAt(time.Minute, 5)
	if !strings.Contains(out.stderr, "fednet refused the card, question #1 is closed: ask: context is 3001 characters") {
		t.Errorf("stderr: %s", out.stderr)
	}
	if got := w.query("SELECT state FROM questions") + " " + w.query("SELECT state FROM agents WHERE name = 'watch-fix-p1'"); got != "closed ended" {
		t.Errorf("question and worker %s", got)
	}
	if _, err := os.Stat(filepath.Join(w.dir, "home", ".local", "state", "fleet", scope+"-watch", "watch-fix-p1-question.md")); !os.IsNotExist(err) {
		t.Errorf("question file left: %v", err)
	}
}

// A screen card whose post failed is nothing anyone waits on: no reminder
// lists it and `thread end` is not refused for it; once its agent is no
// longer stopped, it is closed and never posted.
func TestAnUnpostedScreenCardIsNotWaitedOnAndGoesWithItsScreen(t *testing.T) {
	w := watchThreadWorld(t)
	w.exec("INSERT INTO threads (thread, slug, created_at) VALUES ('C1/1.0', 'c1-1-0', 0)",
		"INSERT INTO agents (name, role, thread, pane_id, state, started_at) VALUES ('thread-c1', 'thread', 'C1/1.0', 'p1', 'active', 0)")
	w.herdrList("thread-c1 blocked 1")
	w.screen("thread-c1", "⏺ fake reply\n"+rule+"\n You have hit your usage limit · resets 3pm (UTC)\n\n ❯ 1. Stop and wait "+
		"for limit to reset\n   2. Request extra usage\n")
	w.lastMessage("C1/1.0", t0+3600)
	w.file("ask-exit", "5")
	w.watchAt(time.Minute, 5)
	w.watchAt(time.Hour, 5)
	if got := w.query("SELECT count(*) || ' ' || state || ' ' || card_id FROM questions"); got != "1 pending " {
		t.Errorf("questions %q", got)
	}
	if got := w.log("fednet.log"); strings.Count(got, "client ask ") != 2 || strings.Contains(got, "Reminder") {
		t.Errorf("fednet: %s", got)
	}
	summary := filepath.Join(w.dir, "summary.md")
	w.file("summary.md", "Nothing open.\n")
	out := w.run("", []string{"thread", "end", "--summary-file", summary}, "FLEET_AGENT=thread-c1", "FLEET_ROLE=thread",
		"FLEET_SCOPE="+scope, "FLEET_THREAD=C1/1.0")
	if strings.Contains(out.stderr, "still waits") {
		t.Errorf("thread end refused: %+v", out)
	}

	w.file("ask-exit", "")
	w.herdrList()
	out = w.watchAt(2*time.Hour, 0)
	if got := w.query("SELECT state || ' ' || card_id FROM questions"); got != "closed " {
		t.Errorf("question %q: %+v", got, out)
	}
	if got := strings.Count(w.log("fednet.log"), "client ask "); got != 2 {
		t.Errorf("%d asks", got)
	}
}

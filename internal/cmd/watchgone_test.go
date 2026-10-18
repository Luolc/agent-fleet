package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/db"
)

// One run of the gone rule at `now` over the live leads and workers, none
// of them in herdr: their readings saved, then those gone long enough
// ended.
func goneRun(t *testing.T, r *watchRun, now int64) {
	t.Helper()
	r.now, r.start = now, time.Now()
	rows, err := LiveRows(r.conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	var all []watched
	for _, live := range rows {
		all = append(all, observe(live, nil, now, 600))
	}
	if err := saveReadings(r.conn, all); err != nil {
		t.Fatal(err)
	}
	r.endGone(all)
}

// An active lead or worker gone from herdr is ended once it has been gone
// for goneSecs, counted from the run that first found it gone, not from
// its last change; a `starting` row is left to its starter.
func TestAnAgentGoneFromHerdrIsEndedAfterTheGrace(t *testing.T) {
	conn, err := db.OpenAt(filepath.Join(t.TempDir(), "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	const t0 int64 = 1700000000
	// Its last change long before it went, as for an agent idle for an
	// hour that the machine then lost.
	if _, err := conn.Exec("INSERT INTO agents (name, role, job, parent, state, started_at, last_status, last_change_at) VALUES "+
		"('j-lead', 'lead', 'j', 'thread-1', 'active', ?1, 'idle', ?1), "+
		"('j-w', 'worker', 'j', 'j-lead', 'active', ?1, 'idle', ?1), "+
		"('j-new', 'worker', 'j', 'j-lead', 'starting', ?1, NULL, NULL)", t0-3600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ParseScope([]byte(`{}`), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	r := &watchRun{conn: conn, cfg: cfg}
	states := func() string {
		t.Helper()
		var got string
		if err := conn.QueryRow("SELECT group_concat(name || '=' || state, ' ') FROM (SELECT name, state FROM agents ORDER BY id)").
			Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	goneRun(t, r, t0)
	goneRun(t, r, t0+goneSecs-1)
	if got := states(); got != "j-lead=active j-w=active j-new=starting" {
		t.Errorf("within the grace: %s", got)
	}
	goneRun(t, r, t0+goneSecs)
	if got := states(); got != "j-lead=ended j-w=ended j-new=starting" {
		t.Errorf("past the grace: %s", got)
	}
	if r.code != 0 {
		t.Errorf("exit %d", r.code)
	}
}

// A gone worker whose `done` already commented on and released its work
// order (recorded under `worker-end:<id>`, its delivery then failed) gets
// neither again, which atb would refuse; one without those records gets
// both.
func TestAGoneWorkerSkipsTheStepsItsDoneRecorded(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("TMPDIR", t.TempDir())
	log := filepath.Join(bin, "atb.log")
	script := "#!/bin/sh\necho \"$1 $2 $3\" >> " + log + "\n[ \"$2 $3\" = 'release EX-1' ] && exit 1\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "atb"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	conn, err := db.OpenAt(filepath.Join(t.TempDir(), "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	const t0 int64 = 1700000000
	if _, err := conn.Exec("INSERT INTO agents (id, name, role, job, parent, state, issue, started_at, last_status, last_change_at) VALUES "+
		"(1, 'j-done', 'worker', 'j', 'j-lead', 'active', 'EX-1', ?1, 'idle', ?1), "+
		"(2, 'j-w', 'worker', 'j', 'j-lead', 'active', 'EX-2', ?1, 'idle', ?1)", t0-3600); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec("INSERT INTO steps (key, step, done_at) VALUES ('worker-end:1', 'comment-work-order', ?1), "+
		"('worker-end:1', 'release-work-order', ?1)", t0-60); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ParseScope([]byte(`{}`), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	r := &watchRun{conn: conn, cfg: cfg}
	goneRun(t, r, t0)
	goneRun(t, r, t0+goneSecs)
	var states string
	if err := conn.QueryRow("SELECT group_concat(name || '=' || state, ' ') FROM (SELECT name, state FROM agents ORDER BY id)").
		Scan(&states); err != nil {
		t.Fatal(err)
	}
	if states != "j-done=ended j-w=ended" {
		t.Errorf("rows: %s", states)
	}
	if got, _ := os.ReadFile(log); string(got) != "linear comment EX-2\nlinear release EX-2\n" {
		t.Errorf("atb calls %q", got)
	}
	if r.code != 0 {
		t.Errorf("exit %d", r.code)
	}
}

package cmd

import (
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

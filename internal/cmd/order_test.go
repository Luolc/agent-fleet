package cmd

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/db"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
)

// A later message of a thread does not overtake an earlier one fednet is
// still retrying (2026-10-30: two messages a person sent a minute apart
// reached the agent in reverse order). The earlier one goes first; while it
// cannot, the later one waits, with the same exit code.
func TestALaterMessageWaitsBehindAnEarlierOne(t *testing.T) {
	dir := fakeClaudeHerdr(t)
	write(t, dir, map[string]string{"status": "blocked", "screen": boxScreen("", "")})
	conn := typedLedger(t)
	first := eventOf(t, strings.Replace(filesEvent, `"msg_id":"m1"`, `"msg_id":"e1"`, 1))
	data, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	now := db.Now()
	for _, row := range [][]any{{"e0", now - 7200, `{"msg_id":"e0"}`}, {"e1", now - 60, string(data)}, {"e2", now, nil}} {
		if _, err := conn.Exec("INSERT INTO inbox (msg_id, thread, state, received_at, event) VALUES (?1, 'C1/1.0', "+
			"'reserved', ?2, ?3)", row...); err != nil {
			t.Fatal(err)
		}
	}
	later := eventOf(t, strings.Replace(filesEvent, `"msg_id":"m1"`, `"msg_id":"e2"`, 1))
	code, err := takeEarlier(herdr.New(nil), conn, "main", &config.Scope{}, later)
	if err != nil || code != exit.Blocked {
		t.Fatalf("takeEarlier: %v, exit %d, want 3", err, code)
	}
	var kept []string
	if err := eachRow(conn, "SELECT msg_id || ' ' || state || ' ' || (event IS NOT NULL) FROM inbox ORDER BY id", func(rows *sql.Rows) error {
		var line string
		err := rows.Scan(&line)
		kept = append(kept, line)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(kept, ", "); got != "e0 reserved 0, e1 reserved 1, e2 reserved 1" {
		t.Errorf("rows: %s; want the hour-old event dropped, the others kept and nothing delivered", got)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "log"))
	if n := strings.Count(string(log), "agent prompt"); n != 1 {
		t.Errorf("%d prompts, want one, for e1 only", n)
	}
}

func TestADeliveredOrDroppedMessageKeepsNoEvent(t *testing.T) {
	conn := typedLedger(t)
	for _, id := range []string{"d1", "d2", "d3"} {
		if _, err := conn.Exec("INSERT INTO inbox (msg_id, thread, state, received_at, event) VALUES (?1, 'C1/1.0', "+
			"'reserved', 0, '{}')", id); err != nil {
			t.Fatal(err)
		}
	}
	for id, state := range map[string]string{"d1": "delivered", "d2": "dropped", "d3": "typed"} {
		if err := setMessage(conn, id, state); err != nil {
			t.Fatal(err)
		}
	}
	var kept string
	if err := conn.QueryRow("SELECT group_concat(msg_id) FROM inbox WHERE event IS NOT NULL").Scan(&kept); err != nil ||
		kept != "d3" {
		t.Errorf("events kept for %q (%v), want d3 only", kept, err)
	}
}

package cmd

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/Luolc/agent-fleet/internal/db"
)

func TestRunStepRecordsOnlyWhatSucceededAndSkipsIt(t *testing.T) {
	conn, err := db.OpenAt(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	runs := 0
	fail := errors.New("refused")
	count := func() func() error { return func() error { runs++; return nil } }
	if err := runStep(conn, "k:1", "a", func() error { runs++; return fail }); !errors.Is(err, fail) {
		t.Fatalf("err = %v", err)
	}
	for _, step := range []string{"a", "a", "b"} {
		if err := runStep(conn, "k:1", step, count()); err != nil {
			t.Fatal(err)
		}
	}
	// Another key does not share the record.
	if err := runStep(conn, "k:2", "a", count()); err != nil {
		t.Fatal(err)
	}
	// The failed run, the first a, b, and the other key's a.
	if runs != 4 {
		t.Errorf("runs = %d, want 4", runs)
	}
}

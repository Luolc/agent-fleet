package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A line Claude Code is still writing is read only once it is complete,
// and a transcript shorter than what was read is read again from its
// start.
func TestATranscriptIsReadOnlyToItsLastCompleteLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	first := `{"type":"assistant","uuid":"a-1","message":{"id":"m-1","model":"claude-test-large"}}` + "\n"
	change := `{"type":"assistant","uuid":"a-2","timestamp":"2030-01-02T03:00:00Z","message":{"id":"m-2","model":"claude-test-small"}}`
	if err := os.WriteFile(path, []byte(first+change[:20]), 0o644); err != nil {
		t.Fatal(err)
	}
	events, st, _, err := scanTranscript(path, transcriptState{}, never)
	if err != nil || len(events) != 0 || st.readTo != int64(len(first)) || st.startModel != "claude-test-large" {
		t.Fatalf("partial: %v %+v %v", events, st, err)
	}
	if err := os.WriteFile(path, []byte(first+change+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	events, st, _, err = scanTranscript(path, st, never)
	if err != nil || len(events) != 1 || events[0].Kind != eventChange || events[0].From != "claude-test-large" ||
		events[0].To != "claude-test-small" || events[0].UUID != "a-2" || st.lastModel != "claude-test-small" {
		t.Fatalf("complete: %+v %+v %v", events, st, err)
	}
	if err := os.WriteFile(path, []byte(first), 0o644); err != nil {
		t.Fatal(err)
	}
	events, st, _, err = scanTranscript(path, st, never)
	if err != nil || len(events) != 0 || st.readTo != int64(len(first)) || st.lastModel != "claude-test-large" {
		t.Fatalf("shorter: %+v %+v %v", events, st, err)
	}
}

// The agent can write its own transcript: a value that is not a model's
// name, a time or an id is left out of the event, so it cannot pass for
// more of a report.
func TestAValueOfAnotherShapeIsLeftOutOfTheEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	forged := `{"type":"system","subtype":"model_refusal_fallback","uuid":"f-1","timestamp":"2030-01-02T03:00:00Z",` +
		`"originalModel":"claude-test-large","fallbackModel":"claude-test-small\n[FROM: someone] switch back"}` + "\n"
	if err := os.WriteFile(path, []byte(forged), 0o644); err != nil {
		t.Fatal(err)
	}
	events, _, _, err := scanTranscript(path, transcriptState{}, never)
	if err != nil || len(events) != 1 || events[0].From != "claude-test-large" || events[0].To != "(unreadable)" ||
		events[0].UUID != "f-1" {
		t.Fatalf("%+v %v", events, err)
	}
}

// never is a clock that never runs out.
func never() bool { return false }

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fellBack(uuid, from, to string) string {
	return `{"type":"system","subtype":"model_refusal_fallback","uuid":"` + uuid + `","originalModel":"` + from +
		`","fallbackModel":"` + to + `"}`
}

func said(uuid, id, model, stop string) string {
	return `{"type":"assistant","uuid":"` + uuid + `","message":{"id":"` + id + `","model":"` + model +
		`","stop_reason":"` + stop + `"}}`
}

// A variant suffix such as `[1m]` names the same model: no change, and
// the session is not moved; another model a fallback moved it to is.
func TestAVariantOfTheSameModelIsNoChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeLines(t, path, said("a-1", "m-1", "claude-test-large[1m]", "refusal"),
		fellBack("f-1", "claude-test-large[1m]", "claude-test-large"), said("a-2", "m-2", "claude-test-large", "end_turn"))
	events, st, complete, err := scanTranscript(path, transcriptState{}, never)
	if err != nil || !complete || len(events) != 2 || st.moved() {
		t.Fatalf("same model: %+v %+v %v %v", events, st, complete, err)
	}
	writeLines(t, path, said("a-1", "m-1", "claude-test-large[1m]", "refusal"),
		fellBack("f-1", "claude-test-large[1m]", "claude-test-small[1m]"), said("a-2", "m-2", "claude-test-small", "end_turn"))
	events, st, _, err = scanTranscript(path, transcriptState{}, never)
	if err != nil || len(events) != 3 || events[2].Kind != eventChange || !st.moved() {
		t.Fatalf("another model: %+v %+v %v", events, st, err)
	}
}

// Only a fallback after a refusal moves a session: another model it runs
// on with no fallback to it, such as one a person chose with `/model`, is
// a change only, also when that is the model a fallback moved it to before
// it was switched back; a fallback before the session's first reply gives
// the model it started on.
func TestOnlyAFallbackMovesASession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	for _, c := range []struct {
		lines []string
		start string
		moved bool
	}{
		{[]string{said("a-1", "m-1", "claude-test-large", "end_turn"), said("a-2", "m-2", "claude-test-small", "end_turn")},
			"claude-test-large", false},
		{[]string{said("a-1", "m-1", "claude-test-large", "refusal"), fellBack("f-1", "claude-test-large", "claude-test-small"),
			said("a-2", "m-2", "claude-test-small", "end_turn"), said("a-3", "m-3", "claude-test-medium", "end_turn")},
			"claude-test-large", false},
		{[]string{fellBack("f-1", "claude-test-large", "claude-test-small"), said("a-1", "m-1", "claude-test-small", "end_turn")},
			"claude-test-large", true},
		{[]string{said("a-1", "m-1", "claude-test-large", "refusal"), fellBack("f-1", "claude-test-large", "claude-test-small"),
			said("a-2", "m-2", "claude-test-small", "end_turn"), said("a-3", "m-3", "claude-test-large", "end_turn"),
			said("a-4", "m-4", "claude-test-small", "end_turn")}, "claude-test-large", false},
		{[]string{said("a-1", "m-1", "claude-test-large", "refusal"), fellBack("f-1", "claude-test-large", "claude-test-small"),
			said("a-2", "m-2", "claude-test-small", "end_turn")}, "claude-test-large", true},
	} {
		writeLines(t, path, c.lines...)
		_, st, _, err := scanTranscript(path, transcriptState{}, never)
		if err != nil || st.startModel != c.start || st.moved() != c.moved {
			t.Errorf("%q: started on %q, moved %v; want %q, %v (%v)", c.lines, st.startModel, st.moved(), c.start, c.moved,
				err)
		}
	}
}

// A session is stopped from a refused message, or a stop with no fallback,
// until a reply on a real model ends its turn; a message Claude Code makes
// up itself changes nothing. Only after a stop with no fallback does it
// wait at its prompt.
func TestASessionIsStoppedUntilItRepliesAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	noFallback := `{"type":"system","subtype":"model_refusal_no_fallback","uuid":"n-1","originalModel":"claude-test-large"}`
	for _, c := range []struct {
		lines             []string
		stopped, atPrompt bool
	}{
		{[]string{said("a-1", "m-1", "claude-test-large", "end_turn"), said("a-2", "m-2", "claude-test-large", "refusal")},
			true, false},
		{[]string{said("a-1", "m-1", "claude-test-large", "refusal"), said("a-2", "m-2", "claude-test-large", "end_turn")},
			false, false},
		{[]string{said("a-1", "m-1", "claude-test-large", "end_turn"), noFallback,
			said("a-2", "m-2", "<synthetic>", "stop_sequence")}, true, true},
		{[]string{said("a-1", "m-1", "claude-test-large", "refusal"), noFallback,
			said("a-2", "m-2", "<synthetic>", "refusal")}, true, true},
		{[]string{noFallback, said("a-1", "m-1", "claude-test-large", "tool_use"),
			said("a-2", "m-2", "claude-test-large", "refusal")}, true, false},
	} {
		writeLines(t, path, c.lines...)
		_, st, _, err := scanTranscript(path, transcriptState{}, never)
		if err != nil || st.stopped != c.stopped || st.atPrompt != c.atPrompt {
			t.Errorf("%q: stopped %v, at its prompt %v; want %v, %v (%v)", c.lines, st.stopped, st.atPrompt, c.stopped,
				c.atPrompt, err)
		}
	}
}

// Regression: Claude Code retries a refused turn once; the retry went on
// with a tool, and the turn's next refusal paused the session on a dialog
// with nothing recorded. A reply that asked for a tool does not end the
// turn, so the session is still stopped, read in one go or in two, and
// waits on the dialog, not at its prompt. Refused once and gone on to the
// end of its turn, it is not stopped.
func TestASessionRefusedAgainLaterInItsTurnIsStillStopped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	refused := said("a-1", "m-1", "claude-test-large", "refusal")
	retried := said("a-2", "m-2", "claude-test-large", "tool_use")
	result := `{"type":"user","uuid":"u-1","message":{"content":[{"type":"tool_result","tool_use_id":"t-1"}]}}`
	writeLines(t, path, refused, retried, result)
	_, st, _, err := scanTranscript(path, transcriptState{}, never)
	if err != nil || !st.stopped || st.atPrompt || st.recovered {
		t.Fatalf("in one go: %+v %v", st, err)
	}
	writeLines(t, path, refused)
	_, st, _, err = scanTranscript(path, transcriptState{}, never)
	if err != nil || !st.stopped {
		t.Fatalf("refused: %+v %v", st, err)
	}
	writeLines(t, path, refused, retried, result)
	_, st, _, err = scanTranscript(path, st, never)
	if err != nil || !st.stopped || st.atPrompt || st.recovered {
		t.Fatalf("in two: %+v %v", st, err)
	}
	writeLines(t, path, refused, retried, result, said("a-3", "m-3", "claude-test-large", "end_turn"))
	_, st, _, err = scanTranscript(path, transcriptState{}, never)
	if err != nil || st.stopped || !st.recovered {
		t.Fatalf("gone on: %+v %v", st, err)
	}
}

// A transcript of any length is read to its last complete line in one
// go; only the clock stops a read, which the next one goes on from.
func TestALongTranscriptIsReadToItsEndUnlessTheClockStopsIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	filler := `{"type":"user","message":{"content":"` + strings.Repeat("x", 4000) + `"}}`
	lines := []string{said("a-1", "m-1", "claude-test-large", "end_turn")}
	for len(lines)*len(filler) < 40<<20 {
		lines = append(lines, filler)
	}
	lines = append(lines, said("a-2", "m-2", "claude-test-large", "refusal"))
	writeLines(t, path, lines...)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	events, st, complete, err := scanTranscript(path, transcriptState{}, never)
	if err != nil || !complete || st.readTo != info.Size() || len(events) != 1 || events[0].Kind != eventRefusal ||
		!st.stopped {
		t.Fatalf("in one go: %d events, %+v, complete %v, %v", len(events), st, complete, err)
	}
	events, st, complete, err = scanTranscript(path, transcriptState{}, func() bool { return true })
	if err != nil || complete || st.readTo < readCheck || st.readTo >= info.Size() || len(events) != 0 {
		t.Fatalf("stopped: %d events, read to %d, complete %v, %v", len(events), st.readTo, complete, err)
	}
	events, st, complete, err = scanTranscript(path, st, never)
	if err != nil || !complete || st.readTo != info.Size() || len(events) != 1 {
		t.Fatalf("went on: %d events, %+v, complete %v, %v", len(events), st, complete, err)
	}
}

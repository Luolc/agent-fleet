package atb

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Luolc/agent-fleet/internal/exit"
)

func TestIdentifiersAreChecked(t *testing.T) {
	for _, good := range []string{"EX-7", "LL2-123", "A-1"} {
		if err := CheckIdentifier(good); err != nil {
			t.Errorf("%s: %v", good, err)
		}
	}
	for _, bad := range []string{"", "ex-7", "EX7", "EX-", `EX-7") { x }`, "EX-7\n"} {
		if err := CheckIdentifier(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestAFailureKeepsAtbsExitAndRedactedStderrOutOfTheMessage(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"error: refused for key $LINEAR_API_KEY, token lin_oauth_0xT0K, Bearer 0xB3AR\" >&2\nexit 3\n"
	if err := os.WriteFile(filepath.Join(dir, "atb"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	t.Setenv("LINEAR_API_KEY", "0xPLAINK3Y")
	err := Claim("EX-1", "a", "s", "x")
	var failure *Failure
	if !errors.As(err, &failure) {
		t.Fatalf("%v is not a Failure", err)
	}
	if failure.ExitCode != 3 || failure.Class != "exit 3" || failure.Op != "atb linear claim EX-1" || !Held(err) {
		t.Errorf("failure = %+v", failure)
	}
	if want := "error: refused for key [redacted], token [redacted], Bearer [redacted]"; failure.Stderr != want {
		t.Errorf("stderr = %q, want %q", failure.Stderr, want)
	}
	var code *exit.Failure
	if !errors.As(err, &code) || code.Code != exit.Environment || strings.Contains(err.Error(), "refused") {
		t.Errorf("message = %q", err.Error())
	}
	if got := redact(strings.Repeat("é", maxStderr)); len(got) > maxStderr+len(" [cut]") || !strings.HasSuffix(got, " [cut]") {
		t.Errorf("cut: %d bytes", len(got))
	}
}

// A key that atb prints across the cut (its first 5 bytes before it) is
// replaced whole before the cut, so no part of it is kept or shown.
func TestAKeyAcrossTheCutIsRedactedWhole(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nhead -c " + strconv.Itoa(maxStderr-5) + " /dev/zero | tr '\\0' a >&2\n" +
		"echo \"$LINEAR_API_KEY tail\" >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "atb"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	t.Setenv("LINEAR_API_KEY", "0xPLAINREV")
	err := Claim("EX-1", "a", "s", "x")
	var failure *Failure
	if !errors.As(err, &failure) {
		t.Fatalf("%v is not a Failure", err)
	}
	var shown bytes.Buffer
	WriteStderr(&shown, err)
	for _, text := range []string{failure.Stderr, shown.String()} {
		if strings.Contains(text, "0xPL") || !strings.HasSuffix(strings.TrimSpace(text), "aaa[reda [cut]") {
			t.Errorf("text ends %q", text[len(text)-30:])
		}
	}
}

func TestWriteStderrShowsTheRedactedStderrWithoutControlCharacters(t *testing.T) {
	err := &Failure{Failure: exit.Environmentf("atb linear claim EX-1 failed"), Op: "atb linear claim EX-1",
		Stderr: "error: no holder\n\x1b[2Jsee\tthe log"}
	var out bytes.Buffer
	WriteStderr(&out, err)
	if want := "atb linear claim EX-1 printed on stderr (credentials redacted):\n  error: no holder\n  [2Jsee\tthe log\n"; out.String() != want {
		t.Errorf("wrote %q, want %q", out.String(), want)
	}
	out.Reset()
	WriteStderr(&out, exit.Environmentf("not atb's"))
	if out.Len() != 0 {
		t.Errorf("wrote %q for an error that is not atb's", out.String())
	}
}

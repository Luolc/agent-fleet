package cmd

import (
	"strings"
	"testing"
)

const rule = "────────────────────────────────────────"

func claudeScreen(transcript, spinner, notice, footer string) string {
	return transcript + "\n\n" + spinner + "\n  ⎿  Tip: press ? for shortcuts\n" + notice + "\n" + rule +
		"\n❯ \n" + rule + "\n" + footer + "\n  ⏵⏵ bypass permissions on\n"
}

func TestSpinnerNoticeBoxAndFooterDoNotChangeTheHash(t *testing.T) {
	a := claudeScreen(
		"⏺ Running the build",
		"✻ Thinking… (12s · esc to interrupt)",
		"                                        3% until auto-compact",
		"  Session: 9.0% | Reset: 2hr 59m",
	)
	b := claudeScreen(
		"⏺ Running the build",
		"✶ Thinking… (14m 3s · ↑ 1.2k tokens · esc to interrupt)",
		"                                        2% until auto-compact",
		"  Session: 9.5% | Reset: 2hr 31m",
	)
	if got := FilterClaudeScreen(a); got != "⏺ Running the build" {
		t.Errorf("FilterClaudeScreen = %q", got)
	}
	if Hash(FilterClaudeScreen(a)) != Hash(FilterClaudeScreen(b)) {
		t.Errorf("hashes differ:\n%q\n%q", FilterClaudeScreen(a), FilterClaudeScreen(b))
	}
}

func TestATranscriptChangeChangesTheHash(t *testing.T) {
	footer := "  Session: 9.0% | Reset: 2hr 59m"
	a := claudeScreen("⏺ step 1", "✻ Working…", "", footer)
	b := claudeScreen("⏺ step 1\n⏺ step 2", "✻ Working…", "", footer)
	if Hash(FilterClaudeScreen(a)) == Hash(FilterClaudeScreen(b)) {
		t.Errorf("same hash for %q and %q", FilterClaudeScreen(a), FilterClaudeScreen(b))
	}
}

func TestAPermissionPromptIsKept(t *testing.T) {
	text := "⏺ about to run\n" + rule + "\n Bash command\n Do you want to proceed?\n ❯ 1. Yes\n   2. No\n"
	if got := FilterClaudeScreen(text); !strings.Contains(got, "Do you want to proceed?") {
		t.Errorf("FilterClaudeScreen = %q", got)
	}
}

func TestHashIsFNV1a(t *testing.T) {
	if got := Hash(""); got != "cbf29ce484222325" {
		t.Errorf("Hash(\"\") = %s", got)
	}
	if got := Hash("a"); got != "af63dc4c8601ec8c" {
		t.Errorf("Hash(\"a\") = %s", got)
	}
}

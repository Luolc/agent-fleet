package cmd

import (
	"strings"
	"testing"
)

func TestThreadSlugIsStableShortAndSafe(t *testing.T) {
	for key, want := range map[string]string{
		"C0123/1700000000.123": "c0123-1700000000-123",
		"D0ABC/1.1":            "d0abc-1-1",
		"///":                  "x",
	} {
		if got := ThreadSlug(key); got != want {
			t.Errorf("ThreadSlug(%q) = %q, want %q", key, got, want)
		}
	}
	long := "C0ABCDEFGHIJ/1700000000.123456"
	got := ThreadSlug(long)
	if len("thread-"+got) > 32 || got != ThreadSlug(long) || !strings.HasPrefix(got, "c0abcdefghij-170") {
		t.Errorf("ThreadSlug(%q) = %q", long, got)
	}
	if ThreadSlug(long) == ThreadSlug("C0ABCDEFGHIJ/1700000000.123457") {
		t.Error("two long keys share a slug")
	}
	if err := CheckAgentName("thread-" + got); err != nil {
		t.Error(err)
	}
}

func TestWithoutLeadingMentionsDropsOnlyTheMentionsAtTheStart(t *testing.T) {
	for text, want := range map[string]string{
		"<@UEXAMPLEBOT> import the A table":                  "import the A table",
		"<@UEXAMPLEBOT> <@W0ABC|bot>  import <@UEXAMPLEBOT>": "import <@UEXAMPLEBOT>",
		"  <@UEXAMPLEBOT>\nimport the A table":               "import the A table",
		"import <@UEXAMPLEBOT> the A table":                  "import <@UEXAMPLEBOT> the A table",
		"<@UEXAMPLEBOT>":                                     "",
		"plain":                                              "plain",
	} {
		if got := WithoutLeadingMentions(text); got != want {
			t.Errorf("%q: got %q, want %q", text, got, want)
		}
	}
}

func TestThreadKeyOfTakesAKeyOrASlackLink(t *testing.T) {
	for in, want := range map[string]string{
		"C0B/1700000009.000100": "C0B/1700000009.000100",
		"https://ws.example.test/archives/C0B/p1700000020000200?thread_ts=1700000009.000100&cid=C0B": "C0B/1700000009.000100",
		"https://ws.example.test/archives/C0B/p1700000009000100":                                     "C0B/1700000009.000100",
	} {
		if got, ok := ThreadKeyOf(in); got != want || !ok {
			t.Errorf("ThreadKeyOf(%q) = %q, %v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{
		"repo-x",
		"C0B",
		"https://ws.example.test/archives/C0B",
		"https://ws.example.test/archives/p1700000009000100",
		"https://ws.example.test/archives/C0B/1700000009000100",
		"https://ws.example.test/archives/C0B/p17000x0009000100",
		"https://ws.example.test/archives/C0B/p1700000009000100?thread_ts=soon",
		"https://ws.example.test/team/C0B/p1700000009000100",
	} {
		if got, ok := ThreadKeyOf(in); ok {
			t.Errorf("ThreadKeyOf(%q) = %q, want none", in, got)
		}
	}
}

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

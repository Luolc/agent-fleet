package atb

import "testing"

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

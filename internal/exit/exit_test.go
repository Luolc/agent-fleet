package exit

import "testing"

func TestExitCodesMatchTheTable(t *testing.T) {
	codes := []Code{Ok, Refused, Unknown, Blocked, NotFound, Environment}
	for want, code := range codes {
		if int(code) != want {
			t.Errorf("code %d = %d", want, code)
		}
	}
}

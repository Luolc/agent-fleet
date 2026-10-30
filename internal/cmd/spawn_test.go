package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/Luolc/agent-fleet/internal/atb"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/identity"
)

// What a stopped spawn says is left of the work order follows how far its
// Linear steps got: none created, one whose create did not finish, one
// never claimed, one whose claim atb did not confirm.
func TestOrderLeftSaysWhatIsLeftOfTheWorkOrder(t *testing.T) {
	c := &spawnChecked{id: &identity.Identity{Agent: "item-1-a"}, job: &jobRow{ParentIssue: "EX-10"}}
	failed := func(op string, code int, class string) error {
		return &atb.Failure{Failure: exit.Environmentf("%s failed", op), Op: op, ExitCode: code, Class: class}
	}
	cases := []struct {
		name, issue string
		err         error
		want        string
	}{
		{"create refused", "", failed("atb linear create", 1, "exit 1"), "no work order was created"},
		{"create timed out", "", failed("atb linear create", -1, "timed out"),
			"atb linear create did not finish (timed out), so it may have made a work order under EX-10"},
		{"ledger refused", "EX-12", exit.Database(errors.New("made-up")), "the work order EX-12 is claimed by nobody"},
		{"claim timed out", "EX-12", failed("atb linear claim EX-12", -1, "timed out"),
			"atb did not confirm the claim of item-1-a on the work order EX-12, so it may be there: " +
				"`fleet worker end item-1-a --issue EX-12` releases it"},
	}
	for _, tc := range cases {
		if got := c.orderLeft(tc.err, tc.issue); !strings.HasPrefix(got, tc.want) {
			t.Errorf("%s: %q, want it to start %q", tc.name, got, tc.want)
		}
	}
}

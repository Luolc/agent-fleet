package identity

import "testing"

func TestTheWorkOrderTravelsAsFleetIssue(t *testing.T) {
	t.Setenv("FLEET_AGENT", "item-1-a")
	t.Setenv("FLEET_ROLE", "worker")
	t.Setenv("FLEET_ISSUE", "EX-7")
	id, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if id.Issue != "EX-7" {
		t.Errorf("Issue = %q, want EX-7", id.Issue)
	}
	pairs := id.EnvPairs()
	if last := pairs[len(pairs)-1]; last != [2]string{"FLEET_ISSUE", "EX-7"} {
		t.Errorf("last pair = %q, want FLEET_ISSUE=EX-7", last)
	}
}

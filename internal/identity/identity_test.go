package identity

import "testing"

func TestTheWorkOrderAndTargetTravelInTheEnvironment(t *testing.T) {
	t.Setenv("FLEET_AGENT", "item-1-a")
	t.Setenv("FLEET_ROLE", "worker")
	t.Setenv("FLEET_ISSUE", "EX-7")
	t.Setenv("FLEET_TARGET", "example-dataset")
	id, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if id.Issue != "EX-7" || id.Target != "example-dataset" {
		t.Errorf("Issue = %q, Target = %q", id.Issue, id.Target)
	}
	pairs := id.EnvPairs()
	if last := pairs[len(pairs)-1]; last != [2]string{"FLEET_ISSUE", "EX-7"} {
		t.Errorf("last pair = %q, want FLEET_ISSUE=EX-7", last)
	}
	if pairs[3] != [2]string{"FLEET_TARGET", "example-dataset"} {
		t.Errorf("target pair = %q", pairs[3])
	}
}

func TestTheTargetDefaultsAndIsADirectoryName(t *testing.T) {
	t.Setenv("FLEET_TARGET", "")
	if got, err := Target(); err != nil || got != DefaultTarget {
		t.Errorf("empty FLEET_TARGET: %q, %v", got, err)
	}
	if got, err := CheckTarget("example-dataset"); err != nil || got != "example-dataset" {
		t.Errorf("a name: %q, %v", got, err)
	}
	for _, bad := range []string{"a/b", ".", "..", "/"} {
		if _, err := CheckTarget(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestTheRolesAreThreadLeadAndWorker(t *testing.T) {
	for value, want := range map[string]Role{"thread": Thread, "lead": Lead, "worker": Worker} {
		role, ok := ParseRole(value)
		if !ok || role != want || role.String() != value {
			t.Errorf("%s: %v %v", value, role, ok)
		}
	}
	for _, gone := range []string{"orchestra", "human-interface", ""} {
		if _, ok := ParseRole(gone); ok {
			t.Errorf("%q is still a role", gone)
		}
	}
}

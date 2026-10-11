package identity

import "testing"

func TestTheWorkOrderAndScopeTravelInTheEnvironment(t *testing.T) {
	t.Setenv("FLEET_AGENT", "item-1-a")
	t.Setenv("FLEET_ROLE", "worker")
	t.Setenv("FLEET_ISSUE", "EX-7")
	t.Setenv("FLEET_SCOPE", "example")
	id, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if id.Issue != "EX-7" || id.Scope != "example" {
		t.Errorf("Issue = %q, Scope = %q", id.Issue, id.Scope)
	}
	pairs := id.EnvPairs()
	if last := pairs[len(pairs)-1]; last != [2]string{"FLEET_ISSUE", "EX-7"} {
		t.Errorf("last pair = %q, want FLEET_ISSUE=EX-7", last)
	}
	if pairs[3] != [2]string{"FLEET_SCOPE", "example"} {
		t.Errorf("scope pair = %q", pairs[3])
	}
}

func TestTheScopeDefaultsToMainAndNamesTheSession(t *testing.T) {
	t.Setenv("FLEET_SCOPE", "")
	if got, err := Scope(); err != nil || got != "main" {
		t.Errorf("empty FLEET_SCOPE: %q, %v", got, err)
	}
	if got, err := CheckScope("example-scope"); err != nil || got != "example-scope" {
		t.Errorf("a name: %q, %v", got, err)
	}
	if got := Session("main"); got != "fleet-main" {
		t.Errorf("Session(main) = %q", got)
	}
	for _, bad := range []string{"a/b", "..", "-x", "Main", "a_b"} {
		if _, err := CheckScope(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestTheRolesAreThreadLeadWorkerUnblockAndRevisit(t *testing.T) {
	for value, want := range map[string]Role{"thread": Thread, "lead": Lead, "worker": Worker, "unblock": Unblock, "revisit": Revisit} {
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

func TestTheThreadKeyTravelsOnlyInAThreadAgentsPane(t *testing.T) {
	t.Setenv("FLEET_AGENT", "thread-c0123-1700000000-123")
	t.Setenv("FLEET_ROLE", "thread")
	t.Setenv("FLEET_THREAD", "C0123/1700000000.123")
	id, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if id.Thread != "C0123/1700000000.123" {
		t.Errorf("Thread = %q", id.Thread)
	}
	pairs := id.EnvPairs()
	if last := pairs[len(pairs)-1]; last != [2]string{"FLEET_THREAD", "C0123/1700000000.123"} {
		t.Errorf("last pair = %q, want FLEET_THREAD", last)
	}
	lead := &Identity{Agent: "item-1-lead", Role: Lead, Thread: "C0123/1700000000.123"}
	for _, pair := range lead.EnvPairs() {
		if pair[0] == "FLEET_THREAD" {
			t.Error("a lead's pane got FLEET_THREAD")
		}
	}
}

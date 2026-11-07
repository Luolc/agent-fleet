package cmd

import "testing"

func TestSessionStartFor(t *testing.T) {
	d, err := sessionStartFor("darwin", "main", 501, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := d.argv; len(got) != 3 || got[0] != "launchctl" || got[1] != "kickstart" || got[2] != "gui/501/fleet.scope.main" {
		t.Fatalf("darwin argv = %v", got)
	}
	l, err := sessionStartFor("linux", "main", 1000, "/run/user/1000")
	if err != nil {
		t.Fatal(err)
	}
	if got := l.argv; len(got) != 4 || got[0] != "systemctl" || got[3] != "fleet-scope@main.service" {
		t.Fatalf("linux argv = %v", got)
	}
	if _, err := sessionStartFor("linux", "main", 1000, ""); err == nil {
		t.Fatal("linux without XDG_RUNTIME_DIR must be refused")
	}
	if _, err := sessionStartFor("darwin", "main", 501, ""); err != nil {
		t.Fatal("darwin needs no XDG_RUNTIME_DIR")
	}
}

func TestParseSysctlLoadavg(t *testing.T) {
	if got, err := parseSysctlLoadavg("{ 1.23 1.45 1.67 }\n"); err != nil || got != 1.23 {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, bad := range []string{"", "{ }", "{ x 1 1 }"} {
		if _, err := parseSysctlLoadavg(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

const vmStat = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                               1000.
Pages active:                            50000.
Pages inactive:                           3000.
Pages speculative:                        2000.
Pages wired down:                        90000.
`

func TestParseVMStatAvailableKiB(t *testing.T) {
	// (1000 + 3000 + 2000) pages of 16 KiB
	if got, err := parseVMStatAvailableKiB(vmStat); err != nil || got != 6000*16 {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, bad := range []string{"", "Pages free: 1.\n", "(page size of 16384 bytes)\nPages free: 1.\n"} {
		if _, err := parseVMStatAvailableKiB(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestCountChildren(t *testing.T) {
	ps := "    1     0\n  100     1\n  200   100\n  201   100\n  300   200\n"
	if got, err := countChildren(ps, 100); err != nil || got != 2 {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, err := countChildren(ps, 201); err != nil || got != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, bad := range []string{"", "1 x\n", "1\n"} {
		if _, err := countChildren(bad, 1); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

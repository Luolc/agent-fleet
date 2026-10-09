package cmd

import "testing"

func TestDurationsAreShort(t *testing.T) {
	cases := map[int64]string{
		45:                   "45s",
		12*60 + 5:            "12m",
		3*3600 + 5*60:        "3h05m",
		2*86400 + 4*3600:     "2d04h",
		-7:                   "0s",
		86400*10 + 3600*23:   "10d23h",
		3600*23 + 59*60 + 59: "23h59m",
	}
	for secs, want := range cases {
		if got := Duration(secs); got != want {
			t.Errorf("Duration(%d) = %q, want %q", secs, got, want)
		}
	}
}

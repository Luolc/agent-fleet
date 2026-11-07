// Parsers for what the macOS tools print, kept out of the darwin file so
// they are built and tested on every platform.

package cmd

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/Luolc/agent-fleet/internal/exit"
)

// parseSysctlLoadavg is the 1-minute load average in the output of `sysctl
// -n vm.loadavg`: `{ 1.23 1.45 1.67 }`.
func parseSysctlLoadavg(out string) (float64, error) {
	fields := strings.Fields(strings.Trim(strings.TrimSpace(out), "{}"))
	if len(fields) > 0 {
		if load, err := strconv.ParseFloat(fields[0], 64); err == nil {
			return load, nil
		}
	}
	return 0, exit.Environmentf("cannot parse the output of `sysctl -n vm.loadavg`")
}

var vmStatPageSize = regexp.MustCompile(`page size of (\d+) bytes`)

// parseVMStatAvailableKiB approximates Linux's MemAvailable from the
// output of `vm_stat`: the pages free, inactive and speculative (memory
// the system hands out without paging an application out), times the page
// size on the header line.
func parseVMStatAvailableKiB(out string) (uint64, error) {
	m := vmStatPageSize.FindStringSubmatch(out)
	if m == nil {
		return 0, exit.Environmentf("cannot find the page size in the output of `vm_stat`")
	}
	pageSize, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil || pageSize == 0 {
		return 0, exit.Environmentf("cannot parse the page size in the output of `vm_stat`")
	}
	var pages uint64
	for _, key := range []string{"Pages free", "Pages inactive", "Pages speculative"} {
		value, ok := vmStatCount(out, key)
		if !ok {
			return 0, exit.Environmentf("cannot find %q in the output of `vm_stat`", key)
		}
		pages += value
	}
	return pages * pageSize / 1024, nil
}

// vmStatCount is the number on the `<key>:   <n>.` line of `vm_stat`.
func vmStatCount(out, key string) (uint64, bool) {
	for _, line := range lines(out) {
		rest, ok := strings.CutPrefix(line, key+":")
		if !ok {
			continue
		}
		value, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(rest), "."), 10, 64)
		return value, err == nil
	}
	return 0, false
}

// countChildren is how many processes `pid` has as their parent in the
// output of `ps -A -o pid=,ppid=`.
func countChildren(out string, pid int64) (int, error) {
	n, seen := 0, 0
	for _, line := range lines(out) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return 0, fmt.Errorf("cannot parse a line of `ps -A -o pid=,ppid=`: %q", line)
		}
		_, err1 := strconv.ParseInt(fields[0], 10, 64)
		parent, err2 := strconv.ParseInt(fields[1], 10, 64)
		if err1 != nil || err2 != nil {
			return 0, fmt.Errorf("cannot parse a line of `ps -A -o pid=,ppid=`: %q", line)
		}
		seen++
		if parent == pid {
			n++
		}
	}
	if seen == 0 {
		return 0, fmt.Errorf("`ps -A -o pid=,ppid=` listed no process")
	}
	return n, nil
}

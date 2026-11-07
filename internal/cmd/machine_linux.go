//go:build linux

// The machine's readings on Linux: /proc.

package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Luolc/agent-fleet/internal/exit"
)

// readMachine is the 1-minute load average and MemAvailable in KiB.
func readMachine() (load float64, availableKiB uint64, err error) {
	loadavg, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, exit.IO(err)
	}
	meminfo, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, exit.IO(err)
	}
	if load, err = parseLoadavg(string(loadavg)); err != nil {
		return 0, 0, err
	}
	availableKiB, err = memAvailableKiB(string(meminfo))
	return load, availableKiB, err
}

// childProcesses is how many child processes `pid` has, as each of its
// threads' /proc children file lists them.
func childProcesses(pid int64) (int, error) {
	files, err := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/children", pid))
	if err != nil {
		return 0, err
	}
	if len(files) == 0 {
		return 0, fmt.Errorf("no /proc/%d/task/*/children", pid)
	}
	n := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return 0, err
		}
		n += len(strings.Fields(string(data)))
	}
	return n, nil
}

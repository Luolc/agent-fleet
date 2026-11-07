//go:build darwin

// The machine's readings on darwin: the tools macOS ships, since there is
// no /proc.

package cmd

import (
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/herdr"
)

// readMachine is the 1-minute load average (`sysctl -n vm.loadavg`) and
// the available memory in KiB (parseVMStatAvailableKiB of `vm_stat`).
func readMachine() (load float64, availableKiB uint64, err error) {
	out, err := readTool("sysctl", "-n", "vm.loadavg")
	if err != nil {
		return 0, 0, err
	}
	if load, err = parseSysctlLoadavg(out); err != nil {
		return 0, 0, err
	}
	if out, err = readTool("vm_stat"); err != nil {
		return 0, 0, err
	}
	availableKiB, err = parseVMStatAvailableKiB(out)
	return load, availableKiB, err
}

// childProcesses is how many child processes `pid` has, counted in the
// list of every process with its parent.
func childProcesses(pid int64) (int, error) {
	out, err := readTool("ps", "-A", "-o", "pid=,ppid=")
	if err != nil {
		return 0, err
	}
	return countChildren(out, pid)
}

// readTool is the output of a tool macOS ships.
func readTool(program string, argv ...string) (string, error) {
	out, _, err := herdr.Exec(program, nil, program, argv...)
	if err != nil {
		return "", exit.Environmentf("%s failed (%s)", program, herdr.Status(err))
	}
	return string(out), nil
}

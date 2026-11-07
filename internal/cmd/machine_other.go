//go:build !linux && !darwin

// The machine's readings on a platform fleet has no reader for: refused,
// never passed.

package cmd

import (
	"fmt"
	"runtime"

	"github.com/Luolc/agent-fleet/internal/exit"
)

func readMachine() (float64, uint64, error) {
	return 0, 0, exit.Environmentf("fleet cannot read the load and memory on %s; set resource_check to false in the repo's config", runtime.GOOS)
}

func childProcesses(pid int64) (int, error) {
	return 0, fmt.Errorf("fleet cannot list child processes on %s", runtime.GOOS)
}

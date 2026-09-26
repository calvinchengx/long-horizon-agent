//go:build !unix

package verify

import (
	"os"
	"syscall"
)

// newProcessGroupAttr: no POSIX process groups here; the check is killed on its own.
func newProcessGroupAttr() *syscall.SysProcAttr { return nil }

// killProcessGroup kills the check's process (its children are not reachable as a group).
func killProcessGroup(p *os.Process) {
	if p != nil {
		_ = p.Kill()
	}
}

// processExitCode is the process's exit status.
func processExitCode(ps *os.ProcessState) int {
	if ps == nil {
		return -1
	}
	return ps.ExitCode()
}

//go:build unix

package verify

import (
	"os"
	"syscall"
)

// newProcessGroupAttr starts the trusted check as the leader of a new process group.
func newProcessGroupAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// killProcessGroup SIGKILLs the check's whole process group.
func killProcessGroup(p *os.Process) {
	if p != nil && p.Pid > 0 {
		_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
	}
}

// processExitCode is Popen.returncode: the exit status, or -N for a death by signal N.
func processExitCode(ps *os.ProcessState) int {
	if ps == nil {
		return -1
	}
	if status, ok := ps.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return -int(status.Signal())
	}
	return ps.ExitCode()
}

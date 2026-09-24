//go:build unix

package execution

import (
	"os"
	"syscall"
)

func newSessionAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// killGroup SIGKILLs the child's whole process group (it leads a new session).
func killGroup(p *os.Process) {
	if p != nil && p.Pid > 0 {
		_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
	}
}

func killTree(p *os.Process) {
	killGroup(p)
	if p != nil {
		_ = p.Kill()
	}
}

// exitCode is Popen.returncode: the exit status, or -signal for a signal-killed child.
func exitCode(ps *os.ProcessState) int {
	if ps == nil {
		return -1
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -int(ws.Signal())
	}
	return ps.ExitCode()
}

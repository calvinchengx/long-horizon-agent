//go:build !unix

package execution

import (
	"os"
	"syscall"
)

func newSessionAttr() *syscall.SysProcAttr { return nil }

func killGroup(*os.Process) {}

func killTree(p *os.Process) {
	if p != nil {
		_ = p.Kill()
	}
}

func exitCode(ps *os.ProcessState) int {
	if ps == nil {
		return -1
	}
	return ps.ExitCode()
}

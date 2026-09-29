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

// processGone is never true off unix: an orphan is kept rather than a live sandbox removed.
func processGone(int) bool { return false }

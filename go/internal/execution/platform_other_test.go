//go:build !unix

package execution

import (
	"errors"
	"os"
)

// mkfifo: no FIFOs here (the test that needs one skips).
func mkfifo(string, uint32) error { return errors.New("no FIFOs on this platform") }

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}

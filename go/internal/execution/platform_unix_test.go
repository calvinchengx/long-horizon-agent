//go:build unix

package execution

import "syscall"

func mkfifo(path string, mode uint32) error { return syscall.Mkfifo(path, mode) }

func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

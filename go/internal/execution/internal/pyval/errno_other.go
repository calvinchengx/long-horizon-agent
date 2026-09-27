//go:build !unix

package pyval

import "syscall"

var strerrorTable = map[syscall.Errno]string{}

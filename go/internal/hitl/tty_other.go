//go:build !darwin && !linux

package hitl

import (
	"io"
	"os"
)

// IsTerminal reports whether r is a character device (an approximation of isatty).
func IsTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok || f == nil {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

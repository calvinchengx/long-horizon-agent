//go:build !unix

package durable

import (
	"errors"
	"os"
)

// flockFile is unsupported off unix (Python's lock is fcntl.flock, unix only too).
func flockFile(*os.File) (bool, error) {
	return false, errors.New("workdir locks need flock (unix only)")
}

func funlockFile(*os.File) error { return nil }

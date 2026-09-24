//go:build !unix

package execution

import "os"

func openNoFollow(name string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(name, flag, perm)
}

func setBlocking(*os.File) error { return nil }

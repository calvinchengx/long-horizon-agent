//go:build !unix

package coordination

import "os"

// Without flock the in-process LeaseBroker lock is the only serialization.
func tryLock(*os.File) (bool, error) { return true, nil }

func unlock(*os.File) {}

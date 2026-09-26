package org

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Runs persist to the mission store (DefaultServices): never the developer's per-user one.
	dir, err := os.MkdirTemp("", "lha-org-store-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_DATA_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

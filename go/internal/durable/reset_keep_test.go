package durable

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// python: tests/unit/test_durable_units.py::test_reset_keep_setting_keeps_operator_paths.
func TestResetWorkdirKeepsTheOperatorsPaths(t *testing.T) {
	dir := initRepo(t)
	_ = os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("target/\n.remote.git/\nbuild/\n"), 0o644)
	if _, err := state.CommitAll(context.Background(), dir, "ignore outputs"); err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{"target", ".remote.git", "build"} {
		_ = os.MkdirAll(filepath.Join(dir, kept), 0o755)
		_ = os.WriteFile(filepath.Join(dir, kept, "f"), []byte("x"), 0o644)
	}
	s, err := config.LoadFrom([]string{"LHA_RESET_KEEP=target, .remote.git"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Activities{Settings: s}).resetWorkdir(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	for kept, want := range map[string]bool{"target": true, ".remote.git": true, "build": false} {
		if _, err := os.Stat(filepath.Join(dir, kept, "f")); (err == nil) != want {
			t.Errorf("%s kept=%v, want %v", kept, err == nil, want)
		}
	}
	bad, _ := config.LoadFrom([]string{"LHA_RESET_KEEP=.git"}, "")
	if err := (&Activities{Settings: bad}).resetWorkdir(context.Background(), dir); err == nil {
		t.Fatal("an invalid LHA_RESET_KEEP was accepted")
	}
}

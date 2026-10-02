package durable

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
)

// Ported from python/tests/unit/test_object_store_prune.py: only old, well-named objects go.
func TestPruneObjectsDeletesOnlyOldObjects(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, strings.Repeat("a", 64))
	fresh := filepath.Join(root, strings.Repeat("b", 64))
	stray := filepath.Join(root, "notes.txt")
	for _, p := range []string{old, fresh, stray} {
		if err := os.WriteFile(p, []byte("12345"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-40 * 24 * time.Hour)
	for _, p := range []string{old, stray} {
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatal(err)
		}
	}
	dry, err := PruneObjects(root, 30, true)
	if err != nil || dry != (PruneResult{Count: 1, Bytes: 5, Kept: 1}) {
		t.Fatal(dry, err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("dry run deleted", err)
	}
	got, err := PruneObjects(root, 30, false)
	if err != nil || got != dry {
		t.Fatal(got, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old object kept")
	}
	for _, p := range []string{fresh, stray} {
		if _, err := os.Stat(p); err != nil {
			t.Fatal(p, "deleted")
		}
	}
	if none, err := PruneObjects(filepath.Join(root, "missing"), 30, false); err != nil || none != (PruneResult{}) {
		t.Fatal(none, err)
	}
	if _, err := PruneObjects(root, 0, false); err == nil {
		t.Fatal("accepted 0 days")
	}
}

// TestWorkerSweepsTheObjectStoreAtStart is python's test_worker_sweeps_the_object_store_at_start.
func TestWorkerSweepsTheObjectStoreAtStart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "objects")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(root, strings.Repeat("a", 64))
	if err := os.WriteFile(old, make([]byte, 10), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(root, strings.Repeat("b", 64))
	if err := os.WriteFile(fresh, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	off, err := config.LoadFrom([]string{"LHA_OBJECT_STORE_ROOT=" + root}, "")
	if err != nil {
		t.Fatal(err)
	}
	if res, err := SweepObjects(off); err != nil || res != nil {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("retention off must not delete")
	}
	on, err := config.LoadFrom([]string{"LHA_OBJECT_STORE_ROOT=" + root, "LHA_OBJECT_RETENTION_DAYS=7"}, "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := SweepObjects(on)
	if err != nil || res == nil || *res != (PruneResult{Count: 1, Bytes: 10, Kept: 1}) {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old object kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh object deleted")
	}
	if _, err := config.LoadFrom([]string{"LHA_OBJECT_RETENTION_DAYS=-1"}, ""); err == nil {
		t.Fatal("negative retention must be rejected")
	}
}

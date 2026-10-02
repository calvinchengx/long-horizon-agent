package persistence

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

// TestConcurrentFirstOpensOfOneFileSucceed: several stores open and write one fresh file at
// once (two processes starting on a shared store); the WAL conversion and schema creation race
// is retried instead of failing with SQLITE_BUSY (python: test_concurrent_first_opens_succeed).
func TestConcurrentFirstOpensOfOneFileSucceed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared", "lha.sqlite3")
	ctx := context.Background()
	const n = 12
	errs := make(chan error, n)
	var start sync.WaitGroup
	start.Add(1)
	for i := 0; i < n; i++ {
		go func(i int) {
			start.Wait()
			store := NewSQLiteStore(path)
			if err := store.Open(ctx); err != nil {
				errs <- err
				return
			}
			defer store.Close()
			errs <- store.UpsertMission(ctx, MissionUpsert{MissionID: "m" + string(rune('a'+i)), Title: "t", Status: "RUNNING"})
		}(i)
	}
	start.Done()
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	store, err := OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows, err := store.ListMissions(ctx, 50)
	if err != nil || len(rows) != n {
		t.Fatalf("%d rows, %v", len(rows), err)
	}
	if mode, _ := store.JournalMode(ctx); mode != "wal" {
		t.Fatal(mode)
	}
}

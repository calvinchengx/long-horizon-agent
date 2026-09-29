package memory

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence/pgtest"
)

// TestPgvectorMemoryRecallSkillsAndMissionScoping is python's integration test of the same name:
// vectors go through pgvector stamped with the embedder model+version, recall is scoped to the
// mission, and skills are shared across missions of one repo. Needs LHA_IT_POSTGRES_DSN.
func TestPgvectorMemoryRecallSkillsAndMissionScoping(t *testing.T) {
	dsn := pgtest.FreshDB(t)
	if _, err := persistence.ApplyMigrations(bg, dsn, pgtest.MigrationsDir()); err != nil {
		t.Fatal(err)
	}
	settings := settingsFrom(t, "LHA_POSTGRES_DSN="+dsn, "LHA_SQLITE_PATH="+filepath.Join(t.TempDir(), "fallback.sqlite3"))
	store, err := persistence.OpenStore(bg, settings, "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if store.Backend() != persistence.BackendPostgres {
		t.Fatal(store.DegradedReason())
	}
	ws := repo(t, map[string]string{"config.py": "PORT = 8080\n"})
	mem := OpenMissionMemory(bg, settings, store, ws, "m1", OpenOptions{})
	defer mem.Close()
	if mem.Mode().Label() != "hybrid" || mem.Embedder().Dim() != 1024 {
		t.Fatalf("%+v", mem.Mode())
	}
	for _, c := range []struct {
		mission  string
		verified bool
	}{{"m1", false}, {"m2", true}} {
		o := CycleObservation{MissionID: c.mission, CycleID: "c1", ItemID: "01", ItemDescription: "make the port configurable",
			Verdict: "failed", Verified: c.verified, Status: "in_progress", Attempts: 1, Failure: "KeyError: PORT",
			DoneSummary: "read PORT from the environment", Tools: []string{"write_file"}}
		if c.verified {
			o.Verdict, o.Status, o.Failure = "passed", "done", ""
		}
		mem.ObserveCycle(bg, o)
	}
	if mem.Errors() != 0 || mem.Mode().Label() != "hybrid" {
		t.Fatalf("errors %d, %+v", mem.Errors(), mem.Mode())
	}
	var count int
	pool := store.(*persistence.PostgresStore).Pool
	if err := pool.QueryRow(bg, "SELECT count(*) FROM semantic_memory WHERE embedding IS NOT NULL AND embedding_model = $1",
		mem.Embedder().Name()).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	block := mem.Recall(bg, "m1", "c2", contracts.NewChecklistItem("02", "make the port configurable via env"), contracts.SituationSnapshot{})
	for _, want := range []string{"c1 [01] failed", "read PORT from the environment", "KeyError: PORT"} {
		if !strings.Contains(block, want) {
			t.Fatalf("%q missing from\n%s", want, block)
		}
	}
	if strings.Contains(block, "c1 [01] passed") {
		t.Fatal("m2's episodes leaked into m1's recall")
	}
	// The dense channel answers on its own too.
	index, err := mem.denseIndex("m1")
	if err != nil {
		t.Fatal(err)
	}
	hits, err := index.Query(bg, "make the port configurable", 5)
	if err != nil || len(hits) != 1 || hits[0].Record.Kind != "progress" || hits[0].Record.EmbeddingModel != "hash" {
		t.Fatalf("%+v %v", hits, err)
	}
}

// TestPgvectorReembedAfterAnEmbedderChange is python's integration test of the same name: rows
// stored without a vector, then rows stamped by an older model version, are re-embedded through
// pgvector and found by the dense channel again. Needs LHA_IT_POSTGRES_DSN.
func TestPgvectorReembedAfterAnEmbedderChange(t *testing.T) {
	dsn := pgtest.FreshDB(t)
	if _, err := persistence.ApplyMigrations(bg, dsn, pgtest.MigrationsDir()); err != nil {
		t.Fatal(err)
	}
	settings := settingsFrom(t, "LHA_POSTGRES_DSN="+dsn, "LHA_SQLITE_PATH="+filepath.Join(t.TempDir(), "fallback.sqlite3"))
	store, err := persistence.OpenStore(bg, settings, "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mem := OpenMissionMemory(bg, settings, store, t.TempDir(), "m1", OpenOptions{})
	defer mem.Close()
	if err := store.PutMemory(bg, "m1", facts("fact a", "fact b"), nil); err != nil {
		t.Fatal(err)
	}
	if done, err := mem.Reembed(bg, "m1", 0, ""); err != nil || !reflect.DeepEqual(done, map[string]int{"m1": 2}) {
		t.Fatal(done, err)
	}
	mem.SetEmbedder(versioned{NewHashEmbedder(PGEmbeddingDim), "2"})
	if counts, err := store.CountStaleMemory(bg, "m1", "hash", "2"); err != nil || !reflect.DeepEqual(counts, map[string]int{"m1": 2}) {
		t.Fatal(counts, err)
	}
	if done, err := mem.Reembed(bg, "m1", 0, ""); err != nil || !reflect.DeepEqual(done, map[string]int{"m1": 2}) {
		t.Fatal(done, err)
	}
	var count int
	if err := store.(*persistence.PostgresStore).Pool.QueryRow(bg, "SELECT count(*) FROM semantic_memory "+
		"WHERE mission_id = 'm1' AND embedding IS NOT NULL AND embedding_version = '2'").Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	index, err := mem.denseIndex("m1")
	if err != nil {
		t.Fatal(err)
	}
	if hits, err := index.Query(bg, "fact a", 1); err != nil || len(hits) != 1 || hits[0].Record.ID != "fact a" {
		t.Fatalf("%+v %v", hits, err)
	}
}

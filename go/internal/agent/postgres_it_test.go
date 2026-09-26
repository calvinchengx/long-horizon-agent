package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence/pgtest"
)

// TestRunLocalOnPostgresPersistsMissionAndLedger is python's integration test of the same name:
// with LHA_POSTGRES_DSN a local run writes its mission row, ledger, episodes and skills to
// Postgres (pgvector memory) and nothing falls back to SQLite. Needs LHA_IT_POSTGRES_DSN.
func TestRunLocalOnPostgresPersistsMissionAndLedger(t *testing.T) {
	dsn := pgtest.FreshDB(t)
	ctx := context.Background()
	if _, err := persistence.ApplyMigrations(ctx, dsn, pgtest.MigrationsDir()); err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	fallback := filepath.Join(tmp, "fallback.sqlite3")
	settings := runnerSettings(t, "LHA_POSTGRES_DSN="+dsn, "LHA_SQLITE_PATH="+fallback)
	ws := filepath.Join(tmp, "ws")
	o := runOpts(t, ws, settings, model.NewStub([]contracts.TurnResult{text(`{"done": true, "summary": "did it"}`)}), passCheck)
	o.Title = "pg run"
	s, err := RunMissionLocal(ctx, o)
	if err != nil || !s.Completed {
		t.Fatal(s, err)
	}
	store, err := persistence.OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	row, _ := store.GetMission(ctx, s.MissionID)
	sum, _ := store.CostSummary(ctx, s.MissionID)
	episodes, _ := store.ListEvents(ctx, s.MissionID, persistence.EventQuery{Kinds: []string{"cycle_outcome"}})
	skills, _ := store.ListSkills(ctx, config.ResolvePath(ws), 0)
	if row == nil || row.Status != "DONE" || sum.Calls != 1 || len(episodes) == 0 || len(skills) == 0 {
		t.Fatalf("%+v %+v %d %d", row, sum, len(episodes), len(skills))
	}
	if _, err := os.Stat(fallback); !os.IsNotExist(err) {
		t.Fatal("the run fell back to SQLite")
	}
}

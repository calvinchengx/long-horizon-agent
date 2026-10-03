package persistence_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence/pgtest"
)

// Postgres integration tests (python/tests/integration/test_postgres.py and
// test_postgres_store.py), run only with LHA_IT_POSTGRES_DSN (see package pgtest).

func migrate(t *testing.T, dsn string) []string {
	t.Helper()
	applied, err := persistence.ApplyMigrations(context.Background(), dsn, pgtest.MigrationsDir())
	if err != nil {
		t.Fatal(err)
	}
	return applied
}

func openPG(t *testing.T, dsn string) *persistence.PostgresStore {
	t.Helper()
	store, err := persistence.OpenPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestPGMigrationsApplyInOrderThenAreANoop(t *testing.T) {
	dsn := pgtest.FreshDB(t)
	if got := migrate(t, dsn); !reflect.DeepEqual(got, persistence.RequiredPGMigrations) {
		t.Fatal(got)
	}
	if got := migrate(t, dsn); len(got) != 0 {
		t.Fatal(got)
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var hasVector, kind, metadata bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector'), "+
		"EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'semantic_memory' AND column_name = 'kind'), "+
		"EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'semantic_memory' AND column_name = 'metadata')").
		Scan(&hasVector, &kind, &metadata); err != nil {
		t.Fatal(err)
	}
	if !hasVector || !kind || !metadata {
		t.Fatal(hasVector, kind, metadata)
	}
}

func TestPGStoreRequiresEveryMigration(t *testing.T) {
	dsn := pgtest.FreshDB(t)
	_, err := persistence.OpenPostgres(context.Background(), dsn)
	var unavailable *persistence.StoreUnavailableError
	if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "run `lha db migrate`") ||
		!strings.Contains(err.Error(), "['0001_init'") {
		t.Fatal(err)
	}
}

func TestPGGatesAndMonotonicMissionStatus(t *testing.T) {
	dsn := pgtest.FreshDB(t)
	migrate(t, dsn)
	store := openPG(t, dsn)
	ctx := context.Background()
	checkGateLifecyclePG(t, store)
	for _, terminal := range []string{"DONE", "IMPOSSIBLE", "ABORTED"} {
		id := "m-" + terminal
		mustOK(t, store.UpsertMission(ctx, persistence.MissionUpsert{MissionID: id, Title: "T", Status: "RUNNING"}))
		mustOK(t, store.UpsertMission(ctx, persistence.MissionUpsert{MissionID: id, Status: terminal}))
		mustOK(t, store.UpsertMission(ctx, persistence.MissionUpsert{MissionID: id, Status: "RUNNING", HeadSHA: "late"}))
		row, err := store.GetMission(ctx, id)
		if err != nil || row.Status != terminal || row.HeadSHA != "late" || row.Title != "T" {
			t.Fatalf("%+v %v", row, err)
		}
		mustOK(t, store.UpsertMission(ctx, persistence.MissionUpsert{MissionID: id, Status: "RUNNING", Reopen: true}))
		if row, _ := store.GetMission(ctx, id); row.Status != "RUNNING" {
			t.Fatal(row.Status)
		}
	}
}

func mustOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func checkGateLifecyclePG(t *testing.T, store persistence.Store) {
	ctx := context.Background()
	g := func(event, at string, step int, decision, by string) persistence.GateEvent {
		return persistence.GateEvent{MissionID: "m1", GateID: "deadlock-3", Kind: "deadlock", Event: event, At: at,
			Question: "q", Options: []string{"retry", "abort"}, DefaultAction: "abort",
			Deadline: "2026-01-01T02:00:00+00:00", Decision: decision, ResolvedBy: by, Step: step}
	}
	one := func() persistence.GateRow {
		rows, err := store.ListGates(ctx, "m1", 0)
		if err != nil || len(rows) != 1 {
			t.Fatalf("%+v %v", rows, err)
		}
		return rows[0]
	}
	opened := g("opened", "2026-01-01T01:00:00+00:00", 0, "", "")
	mustOK(t, store.RecordGateEvent(ctx, opened))
	mustOK(t, store.RecordGateEvent(ctx, opened))
	if r := one(); r.Status != "OPEN" || r.Risk != "deadlock" || r.OpenedAt != "2026-01-01T01:00:00+00:00" ||
		r.Deadline != "2026-01-01T02:00:00+00:00" || !reflect.DeepEqual(r.Options, []string{"retry", "abort"}) {
		t.Fatalf("%+v", r)
	}
	for _, step := range []int{1, 2, 1} {
		mustOK(t, store.RecordGateEvent(ctx, g("reminder", "2026-01-01T01:10:00+00:00", step, "", "")))
	}
	if r := one(); r.Status != "ESCALATED" || r.Reminders != 2 {
		t.Fatalf("%+v", r)
	}
	mustOK(t, store.RecordGateEvent(ctx, g("resolved", "2026-01-01T01:30:00+00:00", 0, "retry", "human")))
	mustOK(t, store.RecordGateEvent(ctx, g("defaulted", "2026-01-01T02:00:00+00:00", 0, "abort", "")))
	if r := one(); r.Status != "RESOLVED" || *r.Decision != "retry" || *r.ResolvedBy != "human" || r.ResolvedAt != "2026-01-01T01:30:00+00:00" {
		t.Fatalf("%+v", r)
	}
	mustOK(t, store.RecordGateEvent(ctx, g("opened", "2026-01-02T00:00:00+00:00", 0, "", "")))
	if r := one(); r.Status != "OPEN" || r.Decision != nil || r.Reminders != 0 {
		t.Fatalf("%+v", r)
	}
	request := map[string]string{"tool": "run_command", "argv": `["make", "release"]`}
	mustOK(t, store.RecordGateEvent(ctx, persistence.GateEvent{MissionID: "m2", GateID: "a", Kind: "tool_call",
		Event: "defaulted", At: "2026-01-01T00:00:00+00:00", Decision: "reject", Request: request, Options: []string{"approve", "reject"}}))
	rows, err := store.ListGates(ctx, "m2", 0)
	if err != nil || len(rows) != 1 || !reflect.DeepEqual(rows[0].Request, request) || rows[0].Risk != "tool_call" {
		t.Fatalf("%+v %v", rows, err)
	}
}

func TestPGStoreRoundTripsEverything(t *testing.T) {
	dsn := pgtest.FreshDB(t)
	migrate(t, dsn)
	store := openPG(t, dsn)
	ctx := context.Background()
	for i, known := range []bool{true, false} {
		e := governor.CostEntry{CycleID: "c1", Model: "m", InputTokens: 10, OutputTokens: 5, USD: 0.25, CostKnown: known, Role: "lead"}
		key := []string{"k#0", "k#1"}[i]
		if w, err := store.RecordCost(ctx, "m1", e, key); err != nil || !w {
			t.Fatal(w, err)
		}
		if w, _ := store.RecordCost(ctx, "m1", e, key); w {
			t.Fatal("not idempotent")
		}
	}
	sum, err := store.CostSummary(ctx, "m1")
	if err != nil || sum.Calls != 2 || sum.KnownUSD != 0.25 || sum.UnknownCostCalls != 1 || sum.InputTokens != 20 {
		t.Fatalf("%+v %v", sum, err)
	}
	costs, _ := store.ListCosts(ctx, "m1", 0)
	if len(costs) != 2 || costs[1].USD != nil || costs[1].CostKnown || costs[0].TS == "" {
		t.Fatalf("%+v", costs)
	}
	for i := 0; i < 4; i++ {
		kind := []string{"a", "b"}[i%2]
		if _, err := store.AppendEvent(ctx, "m1", "c1", kind, map[string]any{"i": i, "s": "é"}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := store.ListEvents(ctx, "m1", persistence.EventQuery{Kinds: []string{"b"}, Limit: 1})
	if err != nil || len(events) != 1 || events[0].Payload["i"].(interface{ String() string }).String() != "3" || events[0].Payload["s"] != "é" {
		t.Fatalf("%+v %v", events, err)
	}
	records := []contracts.MemoryRecord{
		contracts.NewMemoryRecord("fact:1", "fact", "alpha", map[string]string{"item_id": "01"}),
		contracts.NewMemoryRecord("progress:1", "progress", "beta", nil),
	}
	mustOK(t, store.PutMemory(ctx, "m1", records, nil))
	if err := store.PutMemory(ctx, "m1", records, &persistence.Embedding{}); err == nil {
		t.Fatal("vectors accepted")
	}
	listed, _ := store.ListMemory(ctx, "m1", 0)
	if len(listed) != 2 || listed[0].ID != "fact:1" || listed[0].Metadata["item_id"] != "01" {
		t.Fatalf("%+v", listed)
	}
	if n, _ := store.InvalidateMemory(ctx, []string{"progress:1", "zzz"}); n != 1 {
		t.Fatal(n)
	}
	skill := contracts.Skill{ID: "s1", Name: "n", Description: "d", Code: "c", Preconditions: []string{"p"}, Namespace: "/r",
		Provenance: "m1:c1:x", ExpiresAt: "2099-01-01", Verified: true, Uses: 1}
	mustOK(t, store.PutSkill(ctx, skill))
	skills, _ := store.ListSkills(ctx, "/r", 0)
	if len(skills) != 1 || !reflect.DeepEqual(skills[0], skill) {
		t.Fatalf("%+v", skills)
	}
	skill.Verified = false
	var notVerified *contracts.SkillNotVerifiedError
	if err := store.PutSkill(ctx, skill); !errors.As(err, &notVerified) {
		t.Fatal(err)
	}
	mustOK(t, store.UpsertMission(ctx, persistence.MissionUpsert{MissionID: "m1", Title: "T", Status: "RUNNING", WorkflowID: "w"}))
	missions, _ := store.ListMissions(ctx, 0)
	if len(missions) != 1 || missions[0].WorkflowID != "w" || missions[0].CreatedAt == "" {
		t.Fatalf("%+v", missions)
	}
}

func TestOpenStoreUsesPostgresAndFallsBackWhenUnmigrated(t *testing.T) {
	dsn := pgtest.FreshDB(t)
	tmp := t.TempDir()
	settings, err := config.LoadFrom([]string{"LHA_POSTGRES_DSN=" + dsn, "LHA_SQLITE_PATH=" + filepath.Join(tmp, "fallback.sqlite3")}, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	unmigrated, err := persistence.OpenStore(ctx, settings, "")
	if err != nil {
		t.Fatal(err)
	}
	if unmigrated.Backend() != persistence.BackendSQLite || !strings.Contains(unmigrated.DegradedReason(), "lha db migrate") {
		t.Fatal(unmigrated.Backend(), unmigrated.DegradedReason())
	}
	unmigrated.Close()
	migrate(t, dsn)
	store, err := persistence.OpenStore(ctx, settings, "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if store.Backend() != persistence.BackendPostgres || store.DegradedReason() != "" {
		t.Fatal(store.Backend(), store.DegradedReason())
	}
}

func TestPGMissionEventsAppendInOrderAndReadForward(t *testing.T) {
	dsn := pgtest.FreshDB(t)
	migrate(t, dsn)
	store := openPG(t, dsn)
	ctx := context.Background()
	if err := store.AppendMissionEvents(ctx, []persistence.MissionEvent{
		{MissionID: "e1", CycleID: "c1", Kind: "cycle_started", Payload: map[string]any{"item_id": "01"}},
		{MissionID: "e2", CycleID: "c1", Kind: "tool_call", Payload: map[string]any{"tool": "grep"}, TS: "2026-10-03T00:00:00Z"},
		{MissionID: "e1", CycleID: "c1", Kind: "tool_call", Payload: map[string]any{"tool": "edit_file", "ok": true}},
	}); err != nil {
		t.Fatal(err)
	}
	every, err := store.ReadMissionEvents(ctx, "", 0, 0)
	if err != nil || len(every) != 3 || every[1].MissionID != "e2" || !strings.HasPrefix(every[1].TS, "2026-10-03T00:00:00") || every[2].Payload["tool"] != "edit_file" {
		t.Fatalf("%v %+v", err, every)
	}
	one, err := store.ReadMissionEvents(ctx, "e1", 0, 1)
	if err != nil || len(one) != 1 || one[0].Kind != "cycle_started" {
		t.Fatalf("%v %+v", err, one)
	}
}

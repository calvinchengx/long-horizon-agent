package persistence

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
)

// Ported from python/tests/unit/test_persistence_store.py.

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}

func openTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "db", "lha.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func entry(cycle string, usd float64, known bool, role string) governor.CostEntry {
	return governor.CostEntry{CycleID: cycle, Model: "m", InputTokens: 10, OutputTokens: 5, USD: usd, CostKnown: known, Role: role}
}

// must returns v, panicking (failing the test) on err.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func settingsFrom(t *testing.T, env ...string) *config.Settings {
	t.Helper()
	return must(config.LoadFrom(env, ""))
}

// --- schema / file ---------------------------------------------------------------------------------

func TestSQLiteStoreIsWALAndSchemaIsVersioned(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	var _ Store = store
	if store.Backend() != BackendSQLite {
		t.Fatal(store.Backend())
	}
	if mode := must(store.JournalMode(ctx)); strings.ToLower(mode) != "wal" {
		t.Fatalf("journal mode %q", mode)
	}
	db := must(sql.Open("sqlite", store.Path))
	defer db.Close()
	rows := must(db.Query("SELECT version FROM schema_migrations ORDER BY version"))
	var versions []string
	for rows.Next() {
		var v string
		ok(t, rows.Scan(&v))
		versions = append(versions, v)
	}
	rows.Close()
	if !reflect.DeepEqual(versions, []string{"sqlite_0001_init", "sqlite_0002_hitl_gates", "sqlite_0003_mission_events"}) {
		t.Fatal(versions)
	}
	for _, table := range []string{"missions", "hitl_gates", "cost_ledger", "episodic_events", "mission_events", "semantic_memory", "skills"} {
		var name string
		ok(t, db.QueryRow("SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&name))
	}
	// Re-opening an existing file applies nothing twice and keeps the data.
	ok(t, store.UpsertMission(ctx, MissionUpsert{MissionID: "m1", Title: "t", Status: "RUNNING"}))
	ok(t, store.Close())
	again := must(OpenSQLite(ctx, store.Path))
	defer again.Close()
	if m := must(again.GetMission(ctx, "m1")); m == nil {
		t.Fatal("mission lost")
	}
}

// --- missions --------------------------------------------------------------------------------------

func TestMissionUpsertTracksStatusAndKeepsHead(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if m := must(store.GetMission(ctx, "nope")); m != nil {
		t.Fatal(m)
	}
	ok(t, store.UpsertMission(ctx, MissionUpsert{MissionID: "m1", Title: "T", Description: "D", Status: "RUNNING"}))
	ok(t, store.UpsertMission(ctx, MissionUpsert{MissionID: "m1", Status: "RUNNING", HeadSHA: "abc"}))
	ok(t, store.UpsertMission(ctx, MissionUpsert{MissionID: "m1", Status: "DONE"}))
	row := must(store.GetMission(ctx, "m1"))
	if row.Title != "T" || row.Description != "D" || row.Status != "DONE" || row.HeadSHA != "abc" {
		t.Fatalf("%+v", row)
	}
	if row.CreatedAt == "" || row.UpdatedAt < row.CreatedAt {
		t.Fatalf("%+v", row)
	}
	ok(t, store.UpsertMission(ctx, MissionUpsert{MissionID: "m2", Title: "second", Status: "RUNNING"}))
	ids := func(limit int) []string {
		out := []string{}
		for _, m := range must(store.ListMissions(ctx, limit)) {
			out = append(out, m.MissionID)
		}
		return out
	}
	if !reflect.DeepEqual(ids(20), []string{"m2", "m1"}) || !reflect.DeepEqual(ids(1), []string{"m2"}) {
		t.Fatal(ids(20))
	}
}

func TestATerminalStatusIsNeverOverwrittenByANonTerminalOne(t *testing.T) {
	ctx := context.Background()
	for _, terminal := range []string{"DONE", "IMPOSSIBLE", "ABORTED"} {
		store := openTestStore(t)
		ok(t, store.UpsertMission(ctx, MissionUpsert{MissionID: "m1", Title: "T", Status: "RUNNING"}))
		ok(t, store.UpsertMission(ctx, MissionUpsert{MissionID: "m1", Status: terminal}))
		for _, late := range []string{"RUNNING", "WAITING_ON_HUMAN", "SLEEPING", "DEGRADED_PARK"} {
			ok(t, store.UpsertMission(ctx, MissionUpsert{MissionID: "m1", Status: late, HeadSHA: "late"}))
		}
		row := must(store.GetMission(ctx, "m1"))
		if row.Status != terminal || row.HeadSHA != "late" {
			t.Fatalf("%s: %+v", terminal, row)
		}
		ok(t, store.UpsertMission(ctx, MissionUpsert{MissionID: "m1", Status: "ABORTED"}))
		if row := must(store.GetMission(ctx, "m1")); row.Status != "ABORTED" {
			t.Fatal(row.Status)
		}
		ok(t, store.UpsertMission(ctx, MissionUpsert{MissionID: "m1", Status: "RUNNING", Reopen: true}))
		if row := must(store.GetMission(ctx, "m1")); row.Status != "RUNNING" {
			t.Fatal(row.Status)
		}
	}
}

// --- human gates -----------------------------------------------------------------------------------

func gate(event, at string, step int, decision, by string) GateEvent {
	return GateEvent{
		MissionID: "m1", GateID: "deadlock-3", Kind: "deadlock", Event: event, At: at,
		Question:      "Mission m1 is deadlocked. Retry, abort or impossible?",
		Options:       []string{"retry", "abort", "impossible"},
		DefaultAction: "abort", Deadline: "2026-01-01T02:00:00+00:00", Decision: decision, ResolvedBy: by, Step: step,
	}
}

func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func oneGate(t *testing.T, store Store, missionID string) GateRow {
	t.Helper()
	rows := must(store.ListGates(context.Background(), missionID, 0))
	if len(rows) != 1 {
		t.Fatalf("%d gates", len(rows))
	}
	return rows[0]
}

// GateLifecycle is shared with the Postgres integration test.
func checkGateLifecycle(t *testing.T, store Store) {
	ctx := context.Background()
	opened := gate("opened", "2026-01-01T01:00:00+00:00", 0, "", "")
	ok(t, store.RecordGateEvent(ctx, opened))
	ok(t, store.RecordGateEvent(ctx, opened)) // a retried write
	row := oneGate(t, store, "m1")
	if row.Status != "OPEN" || row.Kind != "deadlock" || row.Reminders != 0 || row.Decision != nil {
		t.Fatalf("%+v", row)
	}
	if !reflect.DeepEqual(row.Options, []string{"retry", "abort", "impossible"}) || row.DefaultAction != "abort" || row.Risk != "deadlock" {
		t.Fatalf("%+v", row)
	}
	if row.OpenedAt != "2026-01-01T01:00:00+00:00" {
		t.Fatal(row.OpenedAt)
	}
	for _, step := range []int{1, 2, 1} { // the last is an out-of-order retry
		ok(t, store.RecordGateEvent(ctx, gate("reminder", "2026-01-01T01:10:00+00:00", step, "", "")))
	}
	if row := oneGate(t, store, "m1"); row.Status != "ESCALATED" || row.Reminders != 2 {
		t.Fatalf("%+v", row)
	}
	resolved := gate("resolved", "2026-01-01T01:30:00+00:00", 0, "retry", "human")
	ok(t, store.RecordGateEvent(ctx, resolved))
	ok(t, store.RecordGateEvent(ctx, resolved))
	ok(t, store.RecordGateEvent(ctx, gate("reminder", "2026-01-01T01:40:00+00:00", 3, "", "")))
	ok(t, store.RecordGateEvent(ctx, gate("defaulted", "2026-01-01T02:00:00+00:00", 0, "abort", "")))
	row = oneGate(t, store, "")
	if row.Status != "RESOLVED" || str(row.Decision) != "retry" || str(row.ResolvedBy) != "human" ||
		row.Reminders != 2 || row.ResolvedAt != "2026-01-01T01:30:00+00:00" {
		t.Fatalf("%+v", row)
	}
	ok(t, store.RecordGateEvent(ctx, gate("opened", "2026-01-02T00:00:00+00:00", 0, "", "")))
	if row := oneGate(t, store, "m1"); row.Status != "OPEN" || row.Decision != nil || row.Reminders != 0 {
		t.Fatalf("%+v", row)
	}
}

func TestGateLifecycleIsRecordedIdempotently(t *testing.T) { checkGateLifecycle(t, openTestStore(t)) }

func checkCloseWithoutOpening(t *testing.T, store Store) {
	ctx := context.Background()
	request := map[string]string{"fingerprint": strings.Repeat("f", 32), "tool": "run_command", "argv": `["make", "release"]`}
	ok(t, store.RecordGateEvent(ctx, GateEvent{
		MissionID: "m2", GateID: "approval-ffff", Kind: "tool_call", Event: "defaulted",
		At: "2026-01-01T00:00:00+00:00", Options: []string{"approve", "reject"}, DefaultAction: "reject",
		Decision: "reject", ResolvedBy: "default (timeout)", Risk: "irreversible", Request: request,
	}))
	ok(t, store.RecordGateEvent(ctx, gate("opened", "2026-01-03T00:00:00+00:00", 0, "", "")))
	rows := must(store.ListGates(ctx, "", 0))
	if len(rows) != 2 || rows[0].MissionID != "m1" || rows[1].MissionID != "m2" {
		t.Fatalf("%+v", rows)
	}
	if rows := must(store.ListGates(ctx, "", 1)); len(rows) != 1 || rows[0].MissionID != "m1" {
		t.Fatalf("%+v", rows)
	}
	m2 := oneGate(t, store, "m2")
	if m2.Status != "DEFAULTED" || str(m2.Decision) != "reject" || !reflect.DeepEqual(m2.Request, request) || m2.Risk != "irreversible" {
		t.Fatalf("%+v", m2)
	}
	if rows := must(store.ListGates(ctx, "nope", 0)); len(rows) != 0 {
		t.Fatal(rows)
	}
	if err := store.RecordGateEvent(ctx, gate("exploded", "2026-01-01T00:00:00+00:00", 0, "", "")); err == nil ||
		!strings.Contains(err.Error(), "unknown gate event") {
		t.Fatal(err)
	}
}

func TestACloseWithoutAnOpeningStillRecordsTheOutcome(t *testing.T) {
	checkCloseWithoutOpening(t, openTestStore(t))
}

// --- cost ledger -----------------------------------------------------------------------------------

func checkCostLedger(t *testing.T, store Store) {
	ctx := context.Background()
	rec := func(mission string, e governor.CostEntry, key string) bool {
		return must(store.RecordCost(ctx, mission, e, key))
	}
	if !rec("m1", entry("c1", 0.25, true, "lead"), "a#0") || rec("m1", entry("c1", 0.25, true, "lead"), "a#0") {
		t.Fatal("not idempotent")
	}
	if !rec("m1", entry("c1", 9.0, false, "lead"), "a#1") || !rec("m1", entry("c2", 0.5, true, "lead"), "a#0") ||
		!rec("m2", entry("c1", 7.0, true, "lead"), "a#0") {
		t.Fatal("distinct keys refused")
	}
	sum := must(store.CostSummary(ctx, "m1"))
	if sum.Calls != 3 || math.Abs(sum.KnownUSD-0.75) > 1e-9 || sum.UnknownCostCalls != 1 || sum.InputTokens != 30 || sum.OutputTokens != 15 {
		t.Fatalf("%+v", sum)
	}
	rows := must(store.ListCosts(ctx, "m1", 0))
	if len(rows) != 3 || rows[0].CycleID != "c1" || rows[1].CycleID != "c1" || rows[2].CycleID != "c2" {
		t.Fatalf("%+v", rows)
	}
	if rows[1].USD != nil || rows[1].CostKnown || rows[0].USD == nil || math.Abs(*rows[0].USD-0.25) > 1e-9 || rows[0].Role != "lead" {
		t.Fatalf("%+v", rows)
	}
	if last := must(store.ListCosts(ctx, "m1", 1)); len(last) != 1 || last[0].CycleID != "c2" {
		t.Fatalf("%+v", last)
	}
	if s := must(store.CostSummary(ctx, "none")); s.Calls != 0 {
		t.Fatal(s)
	}
}

func TestCostLedgerIsIdempotentAndUnknownCostIsNull(t *testing.T) {
	checkCostLedger(t, openTestStore(t))
}

// --- events ----------------------------------------------------------------------------------------

func checkEvents(t *testing.T, store Store) {
	ctx := context.Background()
	var ids []int64
	for i := 0; i < 6; i++ {
		kind := "j"
		if i%2 == 1 {
			kind = "k"
		}
		ids = append(ids, must(store.AppendEvent(ctx, "m1", "c"+string(rune('0'+i)), kind, map[string]any{"i": i})))
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] <= ids[i-1] {
			t.Fatal(ids)
		}
	}
	is := func(q EventQuery) []string {
		out := []string{}
		for _, e := range must(store.ListEvents(ctx, "m1", q)) {
			out = append(out, string(e.Payload["i"].(interface{ String() string }).String()))
		}
		return out
	}
	for _, c := range []struct {
		q    EventQuery
		want []string
	}{
		{EventQuery{}, []string{"0", "1", "2", "3", "4", "5"}},
		{EventQuery{Kinds: []string{"k"}}, []string{"1", "3", "5"}},
		{EventQuery{Limit: 2}, []string{"4", "5"}},
		{EventQuery{AfterID: ids[3]}, []string{"4", "5"}},
	} {
		if got := is(c.q); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%+v: %v", c.q, got)
		}
	}
	if rows := must(store.ListEvents(ctx, "other", EventQuery{})); len(rows) != 0 {
		t.Fatal(rows)
	}
}

func TestEventsAreOrderedFilterableAndBounded(t *testing.T) { checkEvents(t, openTestStore(t)) }

// --- semantic memory -------------------------------------------------------------------------------

func TestStaleMemoryIsUnvectoredOrAnotherEmbeddersRows(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	rec := func(id string) []contracts.MemoryRecord {
		return []contracts.MemoryRecord{contracts.NewMemoryRecord(id, "fact", id, nil)}
	}
	vec := func(model, version string) *Embedding {
		return &Embedding{Vectors: [][]float64{{1}}, Model: model, Version: version}
	}
	ok(t, store.PutMemory(ctx, "m1", rec("cur"), vec("e", "2")))
	ok(t, store.PutMemory(ctx, "m1", rec("old"), vec("e", "1")))
	ok(t, store.PutMemory(ctx, "m1", rec("lex"), nil)) // stored while the dense channel was down
	ok(t, store.PutMemory(ctx, "m2", rec("other"), vec("f", "2")))
	ok(t, store.PutMemory(ctx, "m1", rec("gone"), nil))
	must(store.InvalidateMemory(ctx, []string{"gone"})) // soft-forgotten rows are never re-embedded
	stale := must(store.StaleMemory(ctx, "m1", "e", "2", 0))
	got := []string{}
	for _, sr := range stale {
		got = append(got, sr.MissionID+"/"+sr.Record.ID)
	}
	if !reflect.DeepEqual(got, []string{"m1/old", "m1/lex"}) {
		t.Fatal(got)
	}
	if every := must(store.StaleMemory(ctx, "", "e", "2", 2)); len(every) != 2 || every[0].Record.ID != "old" || every[1].Record.ID != "lex" {
		t.Fatalf("%+v", every)
	}
	if counts := must(store.CountStaleMemory(ctx, "", "e", "2")); !reflect.DeepEqual(counts, map[string]int{"m1": 2, "m2": 1}) {
		t.Fatal(counts)
	}
	if counts := must(store.CountStaleMemory(ctx, "m2", "f", "2")); len(counts) != 0 {
		t.Fatal(counts)
	}
}

func TestMemoryUpsertVectorsGatingAndSoftInvalidation(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	a := contracts.NewMemoryRecord("a", "fact", "alpha", map[string]string{"item_id": "01"})
	b := contracts.NewMemoryRecord("b", "progress", "beta", nil)
	ok(t, store.PutMemory(ctx, "m1", []contracts.MemoryRecord{a}, &Embedding{Vectors: [][]float64{{1, 0}}, Model: "e", Version: "1"}))
	ok(t, store.PutMemory(ctx, "m1", []contracts.MemoryRecord{b}, nil))
	a2 := a
	a2.Text = "alpha v2"
	ok(t, store.PutMemory(ctx, "m1", []contracts.MemoryRecord{a2}, &Embedding{Vectors: [][]float64{{0, 1}}, Model: "e", Version: "1"}))
	listed := must(store.ListMemory(ctx, "m1", 0))
	if len(listed) != 2 || listed[0].ID != "a" || listed[0].Text != "alpha v2" || listed[1].ID != "b" {
		t.Fatalf("%+v", listed)
	}
	if !reflect.DeepEqual(listed[0].Metadata, map[string]string{"item_id": "01"}) || listed[0].Kind != "fact" {
		t.Fatalf("%+v", listed[0])
	}
	vecs := must(store.MemoryVectors(ctx, "m1", "e", "1", 0))
	if len(vecs) != 1 || vecs[0].Record.ID != "a" || !reflect.DeepEqual(vecs[0].Vector, []float64{0, 1}) {
		t.Fatalf("%+v", vecs)
	}
	if v := must(store.MemoryVectors(ctx, "m1", "e", "2", 0)); len(v) != 0 {
		t.Fatal(v)
	}
	if n := must(store.InvalidateMemory(ctx, []string{"a", "zzz"})); n != 1 {
		t.Fatal(n)
	}
	if n := must(store.InvalidateMemory(ctx, []string{"a"})); n != 0 {
		t.Fatal(n)
	}
	if l := must(store.ListMemory(ctx, "m1", 0)); len(l) != 1 || l[0].ID != "b" {
		t.Fatal(l)
	}
	if n := must(store.InvalidateMemory(ctx, nil)); n != 0 {
		t.Fatal(n)
	}
	err := store.PutMemory(ctx, "m1", []contracts.MemoryRecord{a, b}, &Embedding{Vectors: [][]float64{{1}}})
	if err == nil || !strings.Contains(err.Error(), "one vector per record") {
		t.Fatal(err)
	}
}

// --- skills ----------------------------------------------------------------------------------------

func checkSkills(t *testing.T, store Store) {
	ctx := context.Background()
	skill := contracts.Skill{ID: "s1", Name: "add endpoint", Description: "add a REST endpoint", Code: "summary: ...",
		Preconditions: []string{"fastapi installed"}, Namespace: "/repo/a", Provenance: "m1:c3:abc",
		ExpiresAt: "2099-01-01", Verified: true}
	ok(t, store.PutSkill(ctx, skill))
	ok(t, store.PutSkill(ctx, contracts.Skill{ID: "g", Name: "g", Description: "global", Namespace: "global", Verified: true}))
	ok(t, store.PutSkill(ctx, contracts.Skill{ID: "o", Name: "o", Description: "other repo", Namespace: "/repo/b", Verified: true}))
	var notVerified *contracts.SkillNotVerifiedError
	if err := store.PutSkill(ctx, contracts.Skill{ID: "u", Name: "u", Description: "unverified"}); !errors.As(err, &notVerified) {
		t.Fatal(err)
	}
	got := must(store.ListSkills(ctx, "/repo/a", 0))
	byID := map[string]contracts.Skill{}
	for _, s := range got {
		byID[s.ID] = s
	}
	if len(got) != 2 || byID["g"].ID == "" {
		t.Fatalf("%+v", got)
	}
	if !reflect.DeepEqual(byID["s1"], skill) {
		t.Fatalf("%+v != %+v", byID["s1"], skill)
	}
}

func TestSkillsRoundTripNamespacedAndVerifiedOnly(t *testing.T) { checkSkills(t, openTestStore(t)) }

// --- factory / fallback ----------------------------------------------------------------------------

func TestSQLitePathIsRelocatedOutOfTheMissionCheckout(t *testing.T) {
	tmp := t.TempDir()
	ws := filepath.Join(tmp, "ws")
	inside := settingsFrom(t, "LHA_SQLITE_PATH="+filepath.Join(ws, ".lha", "lha.sqlite3"))
	root := config.ResolvePath(ws)
	if got := ResolveSQLitePath(inside, ws); got != filepath.Join(root, ".git", "lha", "lha.sqlite3") {
		t.Fatal(got)
	}
	outside := settingsFrom(t, "LHA_SQLITE_PATH="+filepath.Join(tmp, "x.sqlite3"))
	if got := ResolveSQLitePath(outside, ws); got != config.ResolvePath(filepath.Join(tmp, "x.sqlite3")) {
		t.Fatal(got)
	}
}

func TestUnsetSQLitePathUsesTheDefaultWhereverTheProcessRuns(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "xdg"))
	settings := settingsFrom(t)
	want := filepath.Join(tmp, "xdg", "lha", "lha.sqlite3")
	for _, cwd := range []string{tmp, filepath.Join(tmp, "elsewhere")} {
		ok(t, os.MkdirAll(cwd, 0o755))
		t.Chdir(cwd)
		if got := ResolveSQLitePath(settings, ""); got != want {
			t.Fatal(got)
		}
	}
	if got := DescribeStore(settings); got != "sqlite "+want {
		t.Fatal(got)
	}
}

func TestRelativeSQLitePathResolvesAgainstCwdAndWarnsOnce(t *testing.T) {
	var buf bytes.Buffer
	saved := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(saved)
	warnedMu.Lock()
	warnedRelative = map[string]bool{}
	warnedMu.Unlock()
	tmp := t.TempDir()
	t.Chdir(tmp)
	settings := settingsFrom(t, "LHA_SQLITE_PATH=rel/x.sqlite3")
	want := config.ResolvePath(filepath.Join(tmp, "rel", "x.sqlite3"))
	for i := 0; i < 2; i++ {
		if got := ResolveSQLitePath(settings, ""); got != want {
			t.Fatal(got)
		}
	}
	if n := strings.Count(buf.String(), "sqlite_path_relative"); n != 1 {
		t.Fatalf("%d warnings: %s", n, buf.String())
	}
	ws := filepath.Join(tmp, "rel")
	if got := ResolveSQLitePath(settings, ws); got != filepath.Join(config.ResolvePath(ws), ".git", "lha", "x.sqlite3") {
		t.Fatal(got)
	}
}

func TestDescribeStoreNamesPostgresAndItsFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.sqlite3")
	on := settingsFrom(t, "LHA_POSTGRES_DSN=postgresql://u:p@h/db", "LHA_SQLITE_PATH="+path)
	if got := DescribeStore(on); got != "postgres (LHA_POSTGRES_DSN) (falls back to SQLite at "+config.ResolvePath(path)+")" {
		t.Fatal(got)
	}
	off := settingsFrom(t, "LHA_POSTGRES_DSN=postgresql://u:p@h/db", "LHA_SQLITE_PATH="+path, "LHA_POSTGRES_FALLBACK_TO_SQLITE=false")
	if got := DescribeStore(off); got != "postgres (LHA_POSTGRES_DSN)" || strings.Contains(DescribeStore(on), "u:p") {
		t.Fatal(got)
	}
}

func TestOpenStoreDefaultsToSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sqlite3")
	store := must(OpenStore(context.Background(), settingsFrom(t, "LHA_SQLITE_PATH="+path), ""))
	defer store.Close()
	if store.Backend() != BackendSQLite || store.DegradedReason() != "" {
		t.Fatal(store.Backend(), store.DegradedReason())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestUnusablePostgresFallsBackToSQLiteOrFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.sqlite3")
	dsn := "LHA_POSTGRES_DSN=postgresql://u:p@127.0.0.1:1/none?connect_timeout=1"
	store := must(OpenStore(context.Background(), settingsFrom(t, dsn, "LHA_SQLITE_PATH="+path), ""))
	defer store.Close()
	if store.Backend() != BackendSQLite || !strings.HasPrefix(store.DegradedReason(), "postgres unavailable") {
		t.Fatal(store.Backend(), store.DegradedReason())
	}
	_, err := OpenStore(context.Background(), settingsFrom(t, dsn, "LHA_SQLITE_PATH="+path, "LHA_POSTGRES_FALLBACK_TO_SQLITE=false"), "")
	var unavailable *StoreUnavailableError
	if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "postgres unavailable") {
		t.Fatal(err)
	}
}

// --- ledger sink / mission tracker -----------------------------------------------------------------

type unpriced struct{ *model.StubModel }

func (unpriced) EstimateCostUSD(contracts.Usage) (float64, error) {
	return 0, contracts.ErrUnknownPrice
}

func testMeter() *governor.CostMeter {
	return governor.NewCostMeter(governor.NewCostLedger(), governor.NewBudgetGovernor(10, 100, true))
}

var hi = []contracts.ModelMessage{{Role: "user", Content: "hi"}}

func TestEveryMeteredCallReachesThePersistentLedger(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	meter := testMeter()
	must(meter.Wrap(model.NewStub(nil), "planner").Complete(ctx, hi, nil, 0)) // before the sink exists
	sink := NewLedgerSink(store, "m1", "run")
	sink.Backfill(ctx, meter.Ledger.Entries())
	sink.Attach(meter)
	meter.SetCycleID("c1")
	must(meter.Wrap(model.NewStub(nil), "lead").Complete(ctx, hi, nil, 0))
	must(meter.Wrap(unpriced{model.NewStub([]contracts.TurnResult{{Text: "x"}})}, "researcher").Complete(ctx, hi, nil, 0))
	rows := must(store.ListCosts(ctx, "m1", 0))
	got := [][2]string{}
	for _, r := range rows {
		got = append(got, [2]string{r.CycleID, r.Role})
	}
	if !reflect.DeepEqual(got, [][2]string{{"c0", "planner"}, {"c1", "lead"}, {"c1", "researcher"}}) {
		t.Fatal(got)
	}
	if rows[2].USD != nil || rows[2].CostKnown || len(rows) != len(meter.Ledger.Entries()) {
		t.Fatalf("%+v", rows)
	}
	if w, _ := sink.Counts(); w != 3 {
		t.Fatal(w)
	}
	replay := NewLedgerSink(store, "m1", "run")
	replay.Backfill(ctx, meter.Ledger.Entries())
	if w, _ := replay.Counts(); w != 0 || len(must(store.ListCosts(ctx, "m1", 0))) != 3 {
		t.Fatal("replay wrote rows")
	}
}

type brokenStore struct{ *SQLiteStore }

func (brokenStore) RecordCost(context.Context, string, governor.CostEntry, string) (bool, error) {
	return false, errors.New("disk I/O error")
}
func (brokenStore) UpsertMission(context.Context, MissionUpsert) error {
	return errors.New("disk I/O error")
}

func TestLedgerAndTrackerFailuresNeverFailTheRun(t *testing.T) {
	ctx := context.Background()
	broken := brokenStore{NewSQLiteStore(":memory:")}
	meter := testMeter()
	sink := NewLedgerSink(broken, "m1", "")
	sink.Attach(meter)
	result := must(meter.Wrap(model.NewStub(nil), "lead").Complete(ctx, hi, nil, 0))
	if _, failures := sink.Counts(); result.Text == "" || failures != 1 || len(meter.Ledger.Entries()) != 1 {
		t.Fatal(failures)
	}
	tracker := NewMissionTracker(broken, "m1", "", "", "")
	tracker.Running(ctx, "")
	if tracker.Failures != 1 {
		t.Fatal(tracker.Failures)
	}
}

type boomHook struct{}

func (boomHook) RecordCost(context.Context, governor.CostEntry) error {
	return errors.New("hook exploded")
}

func TestMeterSurvivesAHookThatFails(t *testing.T) {
	meter := testMeter()
	meter.SetHook(boomHook{})
	result := must(meter.Wrap(model.NewStub(nil), "lead").Complete(context.Background(), hi, nil, 0))
	if result.Text == "" || len(meter.Ledger.Entries()) != 1 {
		t.Fatal(result)
	}
}

func TestStatusForStopMirrorsTheWorkflow(t *testing.T) {
	for stopped, want := range map[string]string{
		"complete": "DONE", "deadlocked: blocked: 01": "IMPOSSIBLE", "governor: budget": "ABORTED",
		"loop on item 01": "ABORTED", "max_cycles": "ABORTED", "error: RuntimeError": "ABORTED",
	} {
		if got := StatusForStop(stopped); got != want {
			t.Errorf("%s: %s", stopped, got)
		}
	}
}

func TestTrackerRecordsTransitions(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	tracker := NewMissionTracker(store, "m1", "T", "D", "mission:m1")
	tracker.Running(ctx, "")
	if row := must(store.GetMission(ctx, "m1")); row.Status != "RUNNING" {
		t.Fatal(row.Status)
	}
	if got := tracker.Finish(ctx, "complete", "f00"); got != "DONE" {
		t.Fatal(got)
	}
	row := must(store.GetMission(ctx, "m1"))
	if row.Status != "DONE" || row.HeadSHA != "f00" || row.WorkflowID != "mission:m1" || row.Title != "T" {
		t.Fatalf("%+v", row)
	}
}

// --- python compatibility helpers ------------------------------------------------------------------

func TestPyDumpsMatchesPythonJSON(t *testing.T) {
	for _, c := range []struct {
		v    any
		want string
	}{
		{map[string]string{"b": "\u00e9", "a": "x\"y"}, "{\"a\": \"x\\\"y\", \"b\": \"\\u00e9\"}"},
		{[]string{"approve", "reject"}, `["approve", "reject"]`},
		{[]float64{1, 0.5, 1e-7, 1e16, 100000}, `[1.0, 0.5, 1e-07, 1e+16, 100000.0]`},
		{map[string]any{"n": 3, "ok": true, "none": nil, "list": []string{}}, `{"list": [], "n": 3, "none": null, "ok": true}`},
		{"\U0001F600", "\"\\ud83d\\ude00\""},
	} {
		if got := PyDumps(c.v); got != c.want {
			t.Errorf("PyDumps(%v) = %s, want %s", c.v, got, c.want)
		}
	}
	if got := CostIdempotencyKey("m1", "c1", "k#0"); len(got) != 32 {
		t.Fatal(got)
	}
}

func TestTerminalGuardSQL(t *testing.T) {
	want := "CASE WHEN a IN ('DONE', 'IMPOSSIBLE', 'ABORTED') AND b NOT IN ('DONE', 'IMPOSSIBLE', 'ABORTED') AND NOT r THEN a ELSE b END"
	if got := TerminalGuardSQL("a", "b", "r"); got != want {
		t.Fatal(got)
	}
}

func TestDiscoverMigrationsOrdersByNameAndVersionsAreStems(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"0002_b.sql", "0001_a.sql", "notes.txt"} {
		ok(t, os.WriteFile(filepath.Join(dir, name), []byte("SELECT 1;"), 0o644))
	}
	got := must(DiscoverMigrations(dir))
	if len(got) != 2 || got[0].Version != "0001_a" || got[1].Version != "0002_b" {
		t.Fatalf("%+v", got)
	}
	if _, err := DiscoverMigrations(filepath.Join(dir, "missing")); err == nil || !strings.Contains(err.Error(), "migrations directory not found") {
		t.Fatal(err)
	}
	// The repository's migrations are exactly the ones the Postgres backend requires.
	repo := must(DiscoverMigrations(filepath.Join("..", "..", "..", "db", "migrations")))
	versions := []string{}
	for _, m := range repo {
		versions = append(versions, m.Version)
		sqlText := must(m.SQL())
		if !strings.Contains(sqlText, "INSERT INTO schema_migrations (version) VALUES ('"+m.Version+"')") {
			t.Errorf("%s does not record itself", m.Version)
		}
	}
	if !reflect.DeepEqual(versions, RequiredPGMigrations) {
		t.Fatal(versions)
	}
}

func TestMissionEventsAreAppendedInOrderAndReadForward(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	ok(t, store.AppendMissionEvents(ctx, []MissionEvent{
		{MissionID: "m1", CycleID: "c1", Kind: "cycle_started", Payload: map[string]any{"item_id": "01"}},
		{MissionID: "m2", CycleID: "c1", Kind: "cycle_started", Payload: map[string]any{"item_id": "07"}, TS: "2026-10-03T00:00:00Z"},
		{MissionID: "m1", CycleID: "c1", Kind: "tool_call", Payload: map[string]any{"tool": "grep", "ok": true}},
	}))
	ok(t, store.AppendMissionEvents(ctx, nil)) // nothing to write is not an error
	every := must(store.ReadMissionEvents(ctx, "", 0, 0))
	if len(every) != 3 || every[0].Kind != "cycle_started" || every[1].MissionID != "m2" || every[2].Kind != "tool_call" {
		t.Fatalf("%+v", every)
	}
	if !(every[0].ID < every[1].ID && every[1].ID < every[2].ID) || every[0].TS == "" || every[1].TS != "2026-10-03T00:00:00Z" {
		t.Fatalf("%+v", every)
	}
	if every[2].Payload["tool"] != "grep" || every[2].Payload["ok"] != true {
		t.Fatal(every[2].Payload)
	}
	if m1 := must(store.ReadMissionEvents(ctx, "m1", 0, 0)); len(m1) != 2 || m1[1].Kind != "tool_call" {
		t.Fatalf("%+v", m1)
	}
	// Paging forward: the oldest limit after the last id seen, never the newest.
	first := must(store.ReadMissionEvents(ctx, "", 0, 1))
	rest := must(store.ReadMissionEvents(ctx, "", first[0].ID, 5))
	if len(first) != 1 || len(rest) != 2 || first[0].ID != every[0].ID || rest[1].ID != every[2].ID {
		t.Fatalf("%+v %+v", first, rest)
	}
	if after := must(store.ReadMissionEvents(ctx, "", every[2].ID, 0)); len(after) != 0 {
		t.Fatal(after)
	}
}

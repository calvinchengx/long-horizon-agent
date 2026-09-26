package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
)

// The mission-store commands (python/tests/unit/test_persistence_wiring.py and test_wiring_cli.py):
// missions / costs / gates read the store a run writes; db migrate applies db/migrations.

func seedStore(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	store, err := persistence.OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	check(store.UpsertMission(ctx, persistence.MissionUpsert{MissionID: "m1", Title: "First", Status: "DONE", HeadSHA: "abc123"}))
	entry := governor.CostEntry{CycleID: "c1", Model: "claude-x", InputTokens: 100, OutputTokens: 20, USD: 0.12, CostKnown: true, Role: "lead"}
	_, err = store.RecordCost(ctx, "m1", entry, "k#0")
	check(err)
	entry.USD, entry.CostKnown, entry.Role = 0, false, "researcher"
	_, err = store.RecordCost(ctx, "m1", entry, "k#1")
	check(err)
	check(store.RecordGateEvent(ctx, persistence.GateEvent{MissionID: "m1", GateID: "deadlock-3", Kind: "deadlock",
		Event: "opened", At: "2026-01-01T01:00:00+00:00", Question: "Retry, abort or impossible?",
		Options: []string{"retry", "abort", "impossible"}, DefaultAction: "abort", Deadline: "2026-01-01T02:00:00+00:00"}))
	check(store.RecordGateEvent(ctx, persistence.GateEvent{MissionID: "m1", GateID: "m1:tool:ff", Kind: "tool_call",
		Event: "defaulted", At: "2026-01-01T00:00:00+00:00", Question: "push?", Options: []string{"approve", "reject"},
		DefaultAction: "reject", Decision: "reject", ResolvedBy: "timeout", Risk: "irreversible",
		Request: map[string]string{"tool": "run_command", "argv": `["git", "push"]`}}))
}

func TestMissionsCostsAndGatesReadTheStore(t *testing.T) {
	dir := cleanEnv(t)
	path := filepath.Join(dir, "store.sqlite3")
	t.Setenv("LHA_SQLITE_PATH", path)
	seedStore(t, path)

	listed := runCLI(t, nil, "missions")
	if listed.code != 0 || !strings.HasPrefix(listed.stdout, "m1  DONE             $0.1200 (+1 unknown-cost)  calls 2  head abc123  updated ") ||
		!strings.HasSuffix(listed.stdout, "  First\n") {
		t.Fatalf("%+v", listed)
	}
	costs := runCLI(t, nil, "costs", "m1")
	lines := strings.Split(strings.TrimSuffix(costs.stdout, "\n"), "\n")
	if costs.code != 0 || len(lines) != 3 ||
		!strings.HasSuffix(lines[0], "  c1           lead        claude-x                 in     100  out     20  $0.1200") ||
		!strings.HasSuffix(lines[1], "  c1           researcher  claude-x                 in     100  out     20  unknown") ||
		lines[2] != "total: 2 calls  known $0.1200  unknown-cost calls 1  tokens in 200 out 40" {
		t.Fatalf("%q", lines)
	}
	if summary := runCLI(t, nil, "costs", "m1", "--limit", "0"); !strings.HasPrefix(summary.stdout, "total:") {
		t.Fatalf("%+v", summary)
	}
	if missing := runCLI(t, nil, "costs", "--limit", "5", "nope"); missing.code != 1 || missing.stderr != "error: no cost ledger rows for mission nope\n" {
		t.Fatalf("%+v", missing)
	}
	gates := runCLI(t, nil, "gates")
	want := "2026-01-01T01:00:00  m1  deadlock-3  deadlock  OPEN      reminders 0  open, default abort at 2026-01-01T02:00:00\n" +
		"  question: Retry, abort or impossible?\n" +
		"  options: retry | abort | impossible\n" +
		"2026-01-01T00:00:00  m1  m1:tool:ff  tool_call DEFAULTED reminders 0  reject by timeout at 2026-01-01T00:00:00\n" +
		"  question: push?\n" +
		"  options: approve | reject\n" +
		`  request: run_command ["git", "push"]` + "\n"
	if gates.code != 0 || gates.stdout != want {
		t.Fatalf("%q\n%q", gates.stdout, want)
	}
	if none := runCLI(t, nil, "gates", "m9"); none.stdout != "no gates recorded for mission m9\n" {
		t.Fatalf("%+v", none)
	}
	if one := runCLI(t, nil, "gates", "--limit", "1"); strings.Count(one.stdout, "question:") != 1 {
		t.Fatalf("%+v", one)
	}
}

func TestStoreCommandsOnAnEmptyStoreAndUsageErrors(t *testing.T) {
	cleanEnv(t)
	if r := runCLI(t, nil, "missions"); r.code != 0 || r.stdout != "no missions recorded\n" {
		t.Fatalf("%+v", r)
	}
	if r := runCLI(t, nil, "gates"); r.code != 0 || r.stdout != "no gates recorded\n" {
		t.Fatalf("%+v", r)
	}
	if r := runCLI(t, nil, "missions", "--limit", "0"); r.code != 2 || !strings.Contains(r.stderr, "Invalid value for '--limit': 0 is not in the range x>=1.") {
		t.Fatalf("%+v", r)
	}
	if r := runCLI(t, nil, "costs"); r.code != 2 || !strings.Contains(r.stderr, "Missing argument 'MISSION_ID'") {
		t.Fatalf("%+v", r)
	}
	if r := runCLI(t, nil, "costs", "a", "b"); r.code != 2 || !strings.Contains(r.stderr, "unexpected extra argument (b)") {
		t.Fatalf("%+v", r)
	}
}

func TestAnUnusableStoreIsACleanError(t *testing.T) {
	cleanEnv(t, "LHA_POSTGRES_DSN=postgresql://u:p@127.0.0.1:1/none?connect_timeout=1")
	r := runCLI(t, nil, "missions")
	if r.code != 0 || !strings.HasPrefix(r.stderr, "warning: postgres unavailable") || !strings.Contains(r.stderr, "reading SQLite instead") {
		t.Fatalf("%+v", r)
	}
	t.Setenv("LHA_POSTGRES_FALLBACK_TO_SQLITE", "false")
	if r := runCLI(t, nil, "missions"); r.code != 2 || !strings.HasPrefix(r.stderr, "error: postgres unavailable") {
		t.Fatalf("%+v", r)
	}
}

func TestDBMigrate(t *testing.T) {
	dir := cleanEnv(t)
	if r := runCLI(t, nil, "db", "migrate"); r.code != 2 || r.stderr != "error: cannot find db/migrations here or in the parent dir; pass --migrations-dir\n" {
		t.Fatalf("%+v", r)
	}
	if err := os.MkdirAll(filepath.Join(dir, "db", "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "python")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub) // like running from python/: the default finds ../db/migrations
	if r := runCLI(t, nil, "db", "migrate"); r.code != 2 || !strings.Contains(r.stderr, "LHA_POSTGRES_DSN is not set") {
		t.Fatalf("%+v", r)
	}
	t.Setenv("LHA_POSTGRES_DSN", "postgresql://u:p@h/db")
	var calls [][2]string
	saved := applyMigrations
	t.Cleanup(func() { applyMigrations = saved })
	applyMigrations = func(_ *cli, dsn, dir string) ([]string, error) {
		calls = append(calls, [2]string{dsn, dir})
		return []string{"0001_init", "0002_idempotent_ledger"}, nil
	}
	r := runCLI(t, nil, "db", "migrate")
	if r.code != 0 || r.stdout != "migrations applied: ['0001_init', '0002_idempotent_ledger']\n" || strings.Contains(r.stdout, "postgresql://") {
		t.Fatalf("%+v", r)
	}
	if len(calls) != 1 || calls[0] != [2]string{"postgresql://u:p@h/db", "../db/migrations"} {
		t.Fatal(calls)
	}
	applyMigrations = func(*cli, string, string) ([]string, error) { return nil, nil }
	if r := runCLI(t, nil, "db", "migrate", "--migrations-dir", "x"); r.stdout != "migrations applied: []\n" {
		t.Fatalf("%+v", r)
	}
	if r := runCLI(t, nil, "db"); r.code != 2 || !strings.Contains(r.stdout, "migrate") {
		t.Fatalf("%+v", r)
	}
	if r := runCLI(t, nil, "db", "nope"); r.code != 2 || !strings.Contains(r.stderr, "No such command 'nope'") {
		t.Fatalf("%+v", r)
	}
}

func TestConfigShowsTheResolvedStore(t *testing.T) {
	dir := cleanEnv(t)
	path := filepath.Join(dir, "s.sqlite3")
	t.Setenv("LHA_SQLITE_PATH", path)
	r := runCLI(t, nil, "config")
	if r.code != 0 || !strings.HasSuffix(r.stdout, "mission store = sqlite "+config.ResolvePath(path)+"\n") {
		t.Fatalf("%q", r.stdout)
	}
}

// TestStoreCommandsMatchPython runs `lha missions`, `lha costs` and `lha gates` of both
// implementations on the same store: the output is identical (skipped without uv).
func TestStoreCommandsMatchPython(t *testing.T) {
	if _, err := exec.LookPath("uv"); err != nil || testing.Short() {
		t.Skip("uv not on PATH (or -short); skipping the cross-implementation check")
	}
	dir := cleanEnv(t)
	path := filepath.Join(dir, "store.sqlite3")
	t.Setenv("LHA_SQLITE_PATH", path)
	seedStore(t, path)
	_, file, _, _ := runtime.Caller(0)
	project := filepath.Join(filepath.Dir(file), "..", "..", "..", "python")
	for _, args := range [][]string{{"missions"}, {"costs", "m1"}, {"costs", "m1", "--limit", "1"}, {"gates"}, {"gates", "m1", "--limit", "1"}, {"costs", "nope"}} {
		cmd := exec.Command("uv", append([]string{"run", "--quiet", "--project", project, "lha"}, args...)...)
		cmd.Dir = dir
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		code := 0
		if exitErr, isExit := err.(*exec.ExitError); isExit {
			code = exitErr.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		got := runCLI(t, nil, args...)
		if got.stdout != stdout.String() || got.code != code || got.stderr != stderr.String() {
			t.Errorf("%v:\n go (%d) %q %q\n py (%d) %q %q", args, got.code, got.stdout, got.stderr, code, stdout.String(), stderr.String())
		}
	}
}

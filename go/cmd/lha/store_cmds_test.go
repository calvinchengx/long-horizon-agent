package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
	"github.com/calvinchengx/long-horizon-agent/go/internal/spec"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
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

func seedStaleMemory(t *testing.T, path string) {
	t.Helper()
	store, err := persistence.OpenSQLite(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	records := []contracts.MemoryRecord{
		contracts.NewMemoryRecord("a", "fact", "fact a", nil),
		contracts.NewMemoryRecord("b", "fact", "fact b", nil),
	}
	if err := store.PutMemory(context.Background(), "m1", records, nil); err != nil { // stored while the dense channel was down
		t.Fatal(err)
	}
}

// TestMemoryReembed is python's test_cli_memory_reembed, and the two implementations agree on a
// shared store: Python re-embeds what Go counted, and Go then finds nothing stale.
func TestMemoryReembed(t *testing.T) {
	dir := cleanEnv(t)
	path := filepath.Join(dir, "lha.sqlite3")
	t.Setenv("LHA_SQLITE_PATH", path)
	seedStaleMemory(t, path)

	dry := runCLI(t, nil, "memory", "reembed", "--dry-run")
	if dry.code != 0 || dry.stdout != "m1  2 rows\n2 rows to re-embed with hash (1)\n" {
		t.Fatalf("%+v", dry)
	}
	if pythonAvailable(t) {
		py := runPythonLHA(t, dir, nil, "memory", "reembed", "--dry-run")
		if py.code != 0 || !strings.HasSuffix(py.stdout, dry.stdout) {
			t.Fatalf("python: %+v", py)
		}
		py = runPythonLHA(t, dir, nil, "memory", "reembed", "m1")
		if py.code != 0 || !strings.HasSuffix(py.stdout, "m1  2 rows\n2 rows re-embedded with hash (1)\n") {
			t.Fatalf("python: %+v", py)
		}
	} else if done := runCLI(t, nil, "memory", "reembed", "m1"); done.stdout != "m1  2 rows\n2 rows re-embedded with hash (1)\n" {
		t.Fatalf("%+v", done)
	}
	if again := runCLI(t, nil, "memory", "reembed"); again.code != 0 || again.stdout != "nothing to re-embed for hash (1)\n" {
		t.Fatalf("%+v", again)
	}
}

func TestMemoryReembedNeedsMemoryAndAnEmbedder(t *testing.T) {
	dir := cleanEnv(t)
	t.Setenv("LHA_SQLITE_PATH", filepath.Join(dir, "lha.sqlite3"))
	if r := runCLI(t, nil, "memory", "reembed"); r.code != 0 || r.stdout != "nothing to re-embed for hash (1)\n" {
		t.Fatalf("%+v", r)
	}
	t.Setenv("LHA_MEMORY_EMBEDDER", "none")
	if r := runCLI(t, nil, "memory", "reembed"); r.code != 2 || !strings.HasPrefix(r.stderr, "error: no embedder to re-embed with: ") {
		t.Fatalf("%+v", r)
	}
	t.Setenv("LHA_MEMORY_ENABLED", "false")
	if r := runCLI(t, nil, "memory", "reembed"); r.code != 2 || r.stderr != "error: memory is disabled (LHA_MEMORY_ENABLED=false)\n" {
		t.Fatalf("%+v", r)
	}
	if r := runCLI(t, nil, "memory"); r.code != 2 || !strings.Contains(r.stdout, "reembed") {
		t.Fatalf("%+v", r)
	}
	if r := runCLI(t, nil, "memory", "nope"); r.code != 2 || !strings.Contains(r.stderr, "No such command 'nope'") {
		t.Fatalf("%+v", r)
	}
}

// TestObjectsPrune is python's test_cli_objects_prune, with Python's output on the same store.
func TestObjectsPrune(t *testing.T) {
	dir := cleanEnv(t)
	root := filepath.Join(dir, "objects")
	t.Setenv("LHA_OBJECT_STORE_ROOT", root)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(root, strings.Repeat("c", 64))
	if err := os.WriteFile(old, make([]byte, 2<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, strings.Repeat("d", 64)), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dry := runCLI(t, nil, "objects", "prune", "--older-than-days", "7", "--dry-run")
	if dry.code != 0 || dry.stdout != "1 objects to delete (2.0 MiB), 1 kept\n" {
		t.Fatalf("%+v", dry)
	}
	if pythonAvailable(t) {
		py := runPythonLHA(t, dir, []string{"LHA_OBJECT_STORE_ROOT=" + root}, "objects", "prune", "--older-than-days", "7", "--dry-run")
		if py.code != 0 || !strings.HasSuffix(py.stdout, dry.stdout) {
			t.Fatalf("python: %+v", py)
		}
	}
	if r := runCLI(t, nil, "objects", "prune", "--older-than-days", "7"); r.stdout != "1 objects deleted (2.0 MiB), 1 kept\n" {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old object kept")
	}
	if r := runCLI(t, nil, "objects", "prune"); r.code != 2 || !strings.Contains(r.stderr, "Missing option '--older-than-days'") {
		t.Fatalf("%+v", r)
	}
	if r := runCLI(t, nil, "objects", "prune", "--older-than-days", "0"); r.code != 2 || !strings.Contains(r.stderr, "not in the range x>=1") {
		t.Fatalf("%+v", r)
	}
}

// TestLabelsExport is python's test_cli_exports_the_anchor_and_the_missions_gates, with Python's
// output on the same anchor and store.
func TestLabelsExport(t *testing.T) {
	dir := cleanEnv(t)
	sqlitePath := filepath.Join(dir, "lha.sqlite3") // where pythonEnv points the Python CLI too
	t.Setenv("LHA_SQLITE_PATH", sqlitePath)
	ws := filepath.Join(dir, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	anchor := state.NewGitMissionAnchor(ws)
	checklist := contracts.Checklist{SchemaVersion: 1, Items: []contracts.ChecklistItem{contracts.NewChecklistItem("01", "do it")}}
	if _, err := anchor.Initialize(ctx, "T", "D", checklist); err != nil {
		t.Fatal(err)
	}
	approval := func(fp, decision string, defaulted bool) contracts.EventRecord {
		by := "terminal:calvin"
		if defaulted {
			by = "timeout"
		}
		return contracts.EventRecord{Kind: "tool_approval", CycleID: "c1", Payload: contracts.Payload(
			"tool", "run_command", "arguments", `{"argv": ["git", "push"], "token": "sk-ant-abcdefghijklmnop1234"}`,
			"reason", "pushes to a remote", "fingerprint", fp, "decision", decision, "approved", decision == "approve",
			"resolved_by", by, "defaulted", defaulted,
		)}
	}
	if _, err := anchor.CommitCheckpoint(ctx, contracts.Checkpoint{
		CycleID: "c1", ProgressSummary: "- c1", Checklist: checklist, Decisions: []contracts.DecisionRecord{},
		Events: []contracts.EventRecord{
			{Kind: "orchestrate", Payload: contracts.Payload("mission_id", "m1", "run", 1)},
			approval("fp1", "approve", false),
			approval("fp9", "reject", true),
			{Kind: "cycle", CycleID: "c2", Payload: contracts.Payload(
				"item_id", "01", "verified", true, "verdict", "passed", "status", "done", "tool_calls", 4,
				"split_into", []any{}, "rolled_back", []any{},
				"checks", []any{contracts.Payload("name", "pytest", "passed", true, "gating", true, "exit_code", 0, "duration_s", 1.234)},
			)},
			{Kind: "review", CycleID: "c3", Payload: contracts.Payload(
				"item_id", "01", "verdict", "approve", "blocking", false, "blocking_issues", []any{}, "advisory", []any{"rename x"},
				"reopened", false, "blocked", false, "base", "aaa111", "head", "bbb222",
			)},
		},
	}); err != nil {
		t.Fatal(err)
	}
	store, err := persistence.OpenSQLite(ctx, sqlitePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []struct{ event, at, decision, by string }{
		{"opened", "2026-01-01T01:00:00+00:00", "", ""},
		{"resolved", "2026-01-01T01:05:00+00:00", "approve", "terminal:calvin"},
	} {
		if err := store.RecordGateEvent(ctx, persistence.GateEvent{
			MissionID: "m1", GateID: "m1:tool:fp1", Kind: "tool_call", Event: e.event, At: e.at,
			Question: "Allow `git push`?", Options: []string{"approve", "reject"}, DefaultAction: "reject",
			Decision: e.decision, ResolvedBy: e.by, Risk: "high", Request: map[string]string{"fingerprint": "fp1", "tool": "run_command"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	store.Close()

	want := `{"at":"","by":"default","cycle_id":"c1","input":{"arguments":"{\"argv\": [\"git\", \"push\"], \"token\": \"***\"}","fingerprint":"fp9","reason":"pushes to a remote","tool":"run_command"},"item_id":"","label":"reject","mission_id":"m1","schema":1,"source":"tool_approval"}
{"at":"","by":"verifier","cycle_id":"c2","input":{"checks":[{"exit_code":0,"gating":true,"name":"pytest","passed":true}],"rolled_back":[],"split_into":[],"status":"done","tool_calls":4,"verified":true},"item_id":"01","label":"passed","mission_id":"m1","schema":1,"source":"verifier"}
{"at":"","by":"reviewer","cycle_id":"c3","input":{"advisory":["rename x"],"base":"aaa111","blocking":false,"blocking_issues":[],"head":"bbb222"},"item_id":"01","label":"approve","mission_id":"m1","schema":1,"source":"review"}
{"at":"2026-01-01T01:05:00+00:00","by":"terminal:calvin","cycle_id":"","input":{"kind":"tool_call","options":["approve","reject"],"question":"Allow ` + "`git push`" + `?","request":{"fingerprint":"fp1","tool":"run_command"},"risk":"high"},"item_id":"","label":"approve","mission_id":"m1","schema":1,"source":"gate"}
`
	r := runCLI(t, nil, "labels", "export", "--workdir", ws)
	if r.code != 0 || r.stdout != want || r.stderr != "4 labels (1 gate, 1 tool_approval, 1 verifier, 1 review)\n" {
		t.Fatalf("%+v", r)
	}
	if pythonAvailable(t) {
		// The Python anchor reads events with git, so the process needs PATH.
		py := runPythonLHA(t, dir, []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}, "labels", "export", "--workdir", ws)
		if py.code != 0 || py.stdout != want {
			t.Fatalf("python: %+v", py)
		}
	}
	out := filepath.Join(dir, "labels.jsonl")
	if r := runCLI(t, nil, "labels", "export", "--workdir", ws, "--out", out, "--diffs"); r.code != 0 || r.stdout != "" {
		t.Fatalf("%+v", r)
	}
	data, err := os.ReadFile(out)
	if err != nil || !strings.Contains(string(data), `"diff":"`) {
		t.Fatalf("%v %s", err, data)
	}
	if r := runCLI(t, nil, "labels", "export", "m1", "--workdir", filepath.Join(dir, "none")); r.code != 0 || strings.Count(r.stdout, "\n") != 1 || !strings.Contains(r.stdout, `"source":"gate"`) {
		t.Fatalf("%+v", r)
	}
	if r := runCLI(t, nil, "labels", "export", "--workdir", filepath.Join(dir, "none")); r.code != 2 || !strings.HasPrefix(r.stderr, "error: no mission anchor at ") {
		t.Fatalf("%+v", r)
	}
	if r := runCLI(t, nil, "labels"); r.code != 2 || !strings.Contains(r.stdout, "export") {
		t.Fatalf("%+v", r)
	}
	if r := runCLI(t, nil, "labels", "nope"); r.code != 2 || !strings.Contains(r.stderr, "No such command 'nope'") {
		t.Fatalf("%+v", r)
	}
}

// TestMissionReport is python's test_cli_renders_the_anchor_and_the_store, with Python's page on
// the same anchor and store.
func TestMissionReport(t *testing.T) {
	dir := cleanEnv(t)
	sqlitePath := filepath.Join(dir, "lha.sqlite3") // where pythonEnv points the Python CLI too
	t.Setenv("LHA_SQLITE_PATH", sqlitePath)
	ws := filepath.Join(dir, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	anchor := state.NewGitMissionAnchor(ws)
	items := contracts.Checklist{SchemaVersion: 1, Items: []contracts.ChecklistItem{contracts.NewChecklistItem("01", "do it"), contracts.NewChecklistItem("02", "then this")}}
	if _, err := anchor.Initialize(ctx, "Report test", "two items", items); err != nil {
		t.Fatal(err)
	}
	items.Items[0].Status, items.Items[0].Attempts = contracts.StatusDone, 1
	if _, err := anchor.CommitCheckpoint(ctx, contracts.Checkpoint{CycleID: "c1", ProgressSummary: "- c1", Checklist: items, Decisions: []contracts.DecisionRecord{},
		Events: []contracts.EventRecord{
			{Kind: "orchestrate", Payload: contracts.Payload("mission_id", "m1", "run", 1)},
			{Kind: "cycle", CycleID: "c1", Payload: contracts.Payload("item_id", "01", "verdict", "passed")},
			{Kind: "review", CycleID: "c1", Payload: contracts.Payload("item_id", "01", "verdict", "approve")},
		}}); err != nil {
		t.Fatal(err)
	}
	store, err := persistence.OpenSQLite(ctx, sqlitePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertMission(ctx, persistence.MissionUpsert{MissionID: "m1", Title: "Report test", Status: "RUNNING", HeadSHA: "abc"}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordGateEvent(ctx, persistence.GateEvent{MissionID: "m1", GateID: "m1:tool:fp1", Kind: "tool_call", Event: "opened",
		At: "2026-01-01T01:00:00+00:00", Question: "Allow `git push`?", Options: []string{"approve", "reject"}, DefaultAction: "reject", Risk: "high",
		Request: map[string]string{"fingerprint": "fp1", "tool": "run_command", "argv": `["git", "push"]`}}); err != nil {
		t.Fatal(err)
	}
	store.Close()

	r := runCLI(t, nil, "mission-report", "--workdir", ws)
	if r.code != 0 || !strings.HasPrefix(r.stdout, "# Mission: Report test\ntwo items\nmission m1  status RUNNING  head ") ||
		!strings.Contains(r.stdout, "  commits 2\n") ||
		!strings.Contains(r.stdout, "## Items (1/2 done)\n01  done        attempts  1  do it\n02  todo        attempts  0  then this\n") ||
		!strings.Contains(r.stdout, "## Cycles (1)\nverdicts: passed 1, failed 0, other 0\nreviews: approve 1, block 0, unparsed 0\n") ||
		!strings.Contains(r.stdout, "## Gates (1)\n2026-01-01T01:00:00  m1  m1:tool:fp1  tool_call OPEN      reminders 0  open, default reject at -\n") ||
		!strings.HasSuffix(r.stdout, "## Cost\nno cost ledger rows\n") {
		t.Fatalf("%+v", r)
	}
	if pythonAvailable(t) {
		py := runPythonLHA(t, dir, []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}, "mission-report", "--workdir", ws)
		if py.code != 0 || py.stdout != r.stdout {
			t.Fatalf("python: %+v\ngo: %q", py, r.stdout)
		}
	}
	if r := runCLI(t, nil, "mission-report", "m1", "--workdir", filepath.Join(dir, "none")); r.code != 0 ||
		!strings.HasPrefix(r.stdout, "# Mission: Report test\nmission m1  status RUNNING  head -  commits 0\n\n## Items (0/0 done)\n(no items)\n") {
		t.Fatalf("%+v", r)
	}
	if r := runCLI(t, nil, "mission-report", "--workdir", filepath.Join(dir, "none")); r.code != 2 || !strings.HasPrefix(r.stderr, "error: no mission anchor at ") {
		t.Fatalf("%+v", r)
	}
}

// lha eval check / run on the committed gold sets, with Python's output (python: test_gold.py).
func TestEvalCheckAndRun(t *testing.T) {
	dir := cleanEnv(t, "LHA_SYSTEM_ONE_BACKEND=stub")
	sets := []string{filepath.Join(spec.Dir(), "..", "eval", "gold", "review-and-verifier-2026-10-02.jsonl")}
	all, err := filepath.Glob(filepath.Join(spec.Dir(), "..", "eval", "gold", "*.jsonl"))
	if err != nil || len(all) < 2 {
		t.Fatalf("gold sets: %v %v", all, err)
	}
	if every := runCLI(t, nil, append([]string{"eval", "check"}, all...)...); every.code != 0 || !strings.Contains(every.stdout, "100 gold rows (0 gate, 0 tool_approval, 70 verifier, 30 review) in 2 files") {
		t.Fatalf("%+v", every)
	}
	checked := runCLI(t, nil, append([]string{"eval", "check"}, sets...)...)
	if checked.code != 0 || !strings.HasPrefix(checked.stdout, "73 gold rows (0 gate, 0 tool_approval, 52 verifier, 21 review) in 1 file") {
		t.Fatalf("%+v", checked)
	}
	ran := runCLI(t, nil, append([]string{"eval", "run", "--judge", "screen"}, sets...)...)
	if ran.code != 0 || !strings.HasPrefix(ran.stdout, "judge: screen\nverifier: 52 rows, 0 judged (the judge abstains)\n") || !strings.Contains(ran.stdout, "review: 21 rows, 21 judged") {
		t.Fatalf("%+v", ran)
	}
	so := runCLI(t, nil, append([]string{"eval", "run", "--judge", "system_one"}, sets...)...)
	if so.code != 0 || !strings.HasPrefix(so.stdout, "judge: system_one\nverifier: 52 rows, 0 judged (the judge abstains)\n") || !strings.Contains(so.stdout, "review: 21 rows, 21 judged") {
		t.Fatalf("%+v", so)
	}
	t.Setenv("LHA_SYSTEM_ONE_BACKEND", "off")
	if off := runCLI(t, nil, "eval", "run", "--judge", "system_one", sets[0]); off.code != 2 || !strings.Contains(off.stderr, "needs a System One backend") {
		t.Fatalf("%+v", off)
	}
	t.Setenv("LHA_SYSTEM_ONE_BACKEND", "stub")
	if r := runCLI(t, nil, "eval", "run", "--judge", "oracle", sets[0]); r.code != 2 || !strings.Contains(r.stderr, "error: unknown --judge 'oracle'; expected recorded, screen, system_one") {
		t.Fatalf("%+v", r)
	}
	bad := filepath.Join(dir, "bad.jsonl")
	_ = os.WriteFile(bad, []byte(`{"schema": 1, "source": "gate"}`+"\n"), 0o644)
	if r := runCLI(t, nil, "eval", "check", bad); r.code != 2 || !strings.Contains(r.stderr, bad+":1: 'gold' must be an object") {
		t.Fatalf("%+v", r)
	}
	dup := filepath.Join(dir, "dup.jsonl")
	row := `{"schema": 1, "source": "verifier", "label": "passed", "mission_id": "m", "cycle_id": "c1", "item_id": "01", "gold": {"label": "passed", "by": "me"}}` + "\n"
	_ = os.WriteFile(dup, []byte(row+row), 0o644)
	if r := runCLI(t, nil, "eval", "check", dup); r.code != 2 || !strings.Contains(r.stderr, "duplicate of") {
		t.Fatalf("%+v", r)
	}
	if r := runCLI(t, nil, "eval", "check", filepath.Join(dir, "missing.jsonl")); r.code != 2 {
		t.Fatalf("%+v", r)
	}
	if r := runCLI(t, nil, "eval"); r.code != 2 || !strings.Contains(r.stdout, "Commands:") {
		t.Fatalf("%+v", r)
	}
	if r := runCLI(t, nil, "eval", "bogus"); r.code != 2 || !strings.Contains(r.stderr, "No such command 'bogus'") {
		t.Fatalf("%+v", r)
	}
}

// lha workspace init builds a repository of members with Python's output (python: test_multi_repo.py).
func TestWorkspaceInit(t *testing.T) {
	dir := cleanEnv(t)
	ctx := context.Background()
	mk := func(name string) string {
		path := filepath.Join(dir, "upstream", name)
		if err := state.InitRepo(ctx, path); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(path, "README.md"), []byte("# "+name+"\n"), 0o644)
		if _, err := state.CommitAll(ctx, path, "init"); err != nil {
			t.Fatal(err)
		}
		return path
	}
	svc, lib := mk("svc.git"), mk("lib")
	ws := filepath.Join(dir, "ws")
	r := runCLI(t, nil, "workspace", "init", ws, "--repo", svc, "--repo", "lib="+lib+"@main")
	lines := strings.Split(strings.TrimRight(r.stdout, "\n"), "\n")
	if r.code != 0 || len(lines) != 3 || !strings.HasPrefix(lines[0], "svc <- "+svc+" (") ||
		!strings.HasPrefix(lines[1], "lib <- "+lib+"@main (") || lines[2] != "workspace "+ws+": 2 members" {
		t.Fatalf("%+v", r)
	}
	members, _ := state.MemberPaths(ctx, ws)
	if strings.Join(members, ",") != "lib,svc" {
		t.Fatalf("members %v", members)
	}
	log, _ := state.LogOneline(ctx, ws, 1)
	if !strings.HasSuffix(log[0], "lha: workspace members svc, lib") {
		t.Fatalf("log %v", log)
	}
	again := runCLI(t, nil, "workspace", "init", ws, "--repo", mk("docs"), "--repo", filepath.Join(ws, "svc"))
	if again.code != 0 || !strings.Contains(again.stderr, "svc: already a member, kept") || !strings.Contains(again.stdout, "3 members") {
		t.Fatalf("%+v", again)
	}
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"workspace", "init", filepath.Join(dir, "nope"), "--repo", "a=", "--repo", "x"}, 2, "missing a URL"},
		{[]string{"workspace", "init", filepath.Join(dir, "nope"), "--repo", "a=x", "--repo", "a=y"}, 2, "error: --repo names repeat: a"},
		{[]string{"workspace", "init", filepath.Join(dir, "nope")}, 2, "Missing option '--repo'"},
		{[]string{"workspace", "init", "--repo", "x"}, 2, "Missing argument 'DIRECTORY'"},
		{[]string{"workspace", "init", filepath.Join(dir, "ws2"), "--repo", filepath.Join(dir, "absent")}, 1, "cannot add member 'absent'"},
		{[]string{"workspace", "bogus"}, 2, "No such command 'bogus'"},
	} {
		r := runCLI(t, nil, tc.args...)
		if r.code != tc.code || !strings.Contains(r.stderr, tc.want) {
			t.Fatalf("%v: %+v", tc.args, r)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "nope")); err == nil {
		t.Fatal("a refused init created the directory")
	}
	refused := runCLI(t, nil, "mission-start", "--task", "x", "--max-parallel", "2", "--workdir", ws)
	if refused.code != 2 || !strings.Contains(refused.stderr, "is a multi-repo workspace (members: docs, lib, svc)") {
		t.Fatalf("%+v", refused)
	}
}

func TestParseMemberSpec(t *testing.T) {
	for _, tc := range []struct{ spec, name, url, ref string }{
		{"git@host:org/svc.git", "svc", "git@host:org/svc.git", ""},
		{"https://h/x/lib.git@v2", "lib", "https://h/x/lib.git", "v2"},
		{"api=https://h/x/y@main", "api", "https://h/x/y", "main"},
		{"/tmp/repos/thing/", "thing", "/tmp/repos/thing/", ""},
	} {
		name, url, ref, err := parseMemberSpec(tc.spec)
		if err != nil || name != tc.name || url != tc.url || ref != tc.ref {
			t.Fatalf("%q -> %q %q %q %v", tc.spec, name, url, ref, err)
		}
	}
	if _, _, _, err := parseMemberSpec("..=/x"); err == nil {
		t.Fatal("'..' accepted as a name")
	}
}

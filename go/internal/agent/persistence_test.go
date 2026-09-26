package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// The local run path persists the mission row and every metered call, and gives the lead tiered
// memory (python/tests/unit/test_persistence_wiring.py and the run-path half of
// test_memory_service.py).

func storeSettings(t *testing.T, env ...string) (*config.Settings, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lha.sqlite3")
	return runnerSettings(t, append([]string{"LHA_SQLITE_PATH=" + path, "LHA_MAX_TURNS_PER_CYCLE=1"}, env...)...), path
}

func openRunStore(t *testing.T, path string) *persistence.SQLiteStore {
	t.Helper()
	s, err := persistence.OpenSQLite(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func items(n int) contracts.Checklist {
	c := contracts.Checklist{SchemaVersion: 1}
	for i := 1; i <= n; i++ {
		c.Items = append(c.Items, item(fmt.Sprintf("%02d", i), fmt.Sprintf("thing %d", i)))
	}
	return c
}

func TestRunLocalPersistsMissionAndEveryModelCall(t *testing.T) {
	settings, path := storeSettings(t, "LHA_MAX_TURNS_PER_CYCLE=2")
	meter := BuildMeter(settings)
	o := runOpts(t, t.TempDir(), settings, model.NewStub([]contracts.TurnResult{done}), passCheck)
	o.Title, o.Description, o.Checklist, o.Meter = "Persisted", "desc", items(2), meter
	s, err := RunMissionLocal(context.Background(), o)
	if err != nil || !s.Completed {
		t.Fatal(s, err)
	}
	store := openRunStore(t, path)
	row, err := store.GetMission(context.Background(), s.MissionID)
	if err != nil || row.Status != "DONE" || row.Title != "Persisted" || row.Description != "desc" || row.HeadSHA != s.HeadSHA {
		t.Fatalf("%+v %v", row, err)
	}
	rows, _ := store.ListCosts(context.Background(), s.MissionID, 0)
	if len(rows) != 2 || len(meter.Ledger.Entries()) != 2 || rows[0].CycleID != "c1" || rows[1].CycleID != "c2" {
		t.Fatalf("%+v", rows)
	}
	for _, r := range rows {
		if r.Role != "lead" || r.USD == nil || *r.USD != 0 || !r.CostKnown {
			t.Fatalf("%+v", r) // a stub is genuinely $0
		}
	}
	if meter.Hook() != nil {
		t.Fatal("the ledger sink stayed attached after the run")
	}
}

type crashModel struct{ *model.StubModel }

func (crashModel) Complete(context.Context, []contracts.ModelMessage, []map[string]any, int) (contracts.TurnResult, error) {
	return contracts.TurnResult{}, errors.New("provider exploded")
}

func TestDeadlockAndCrashRecordTerminalStatuses(t *testing.T) {
	settings, path := storeSettings(t)
	o := runOpts(t, t.TempDir(), settings, model.NewStub([]contracts.TurnResult{done}), redCheck)
	deadlocked, err := RunMissionLocal(context.Background(), o)
	if err != nil || !strings.HasPrefix(deadlocked.StoppedReason, "deadlocked") {
		t.Fatal(deadlocked, err)
	}
	o = runOpts(t, t.TempDir(), settings, crashModel{model.NewStub(nil)}, passCheck)
	o.Title = "crash"
	if _, err := RunMissionLocal(context.Background(), o); err == nil || !strings.Contains(err.Error(), "provider exploded") {
		t.Fatal(err)
	}
	store := openRunStore(t, path)
	byTitle := map[string]persistence.MissionRow{}
	for _, m := range must(store.ListMissions(context.Background(), 0)) {
		byTitle[m.Title] = m
	}
	if byTitle["t"].MissionID != deadlocked.MissionID || byTitle["t"].Status != "IMPOSSIBLE" || byTitle["crash"].Status != "ABORTED" {
		t.Fatalf("%+v", byTitle)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestPlannerSpendIsBackfilledIntoTheMissionLedger(t *testing.T) {
	settings, path := storeSettings(t)
	plan := text(`[{"id": "01", "description": "only item"}]`)
	stub := model.NewStub([]contracts.TurnResult{plan, done})
	o := PlanOptions{RunOptions: runOpts(t, t.TempDir(), settings, stub, passCheck), Task: "t", PlannerModel: stub}
	s, err := PlanAndRunLocal(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	rows := must(openRunStore(t, path).ListCosts(context.Background(), s.MissionID, 0))
	if len(rows) < 2 || rows[0].Role != "planner" || rows[0].CycleID != "c0" || rows[1].Role != "lead" {
		t.Fatalf("%+v", rows)
	}
}

func TestAnUnusableStoreWithoutFallbackFailsTheRunUpFront(t *testing.T) {
	settings, _ := storeSettings(t, "LHA_POSTGRES_DSN=postgresql://u:p@127.0.0.1:1/none?connect_timeout=1",
		"LHA_POSTGRES_FALLBACK_TO_SQLITE=false")
	_, err := RunMissionLocal(context.Background(), runOpts(t, t.TempDir(), settings, model.NewStub([]contracts.TurnResult{done}), passCheck))
	var unavailable *persistence.StoreUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatal(err)
	}
}

// --- memory through the real loop -------------------------------------------------------------------

// recording is a scripted stub that remembers every lead cycle prompt it was sent.
type recording struct {
	*model.StubModel
	mu      sync.Mutex
	prompts [][]contracts.ModelMessage
}

func (r *recording) Complete(ctx context.Context, messages []contracts.ModelMessage, tools []map[string]any, maxTokens int) (contracts.TurnResult, error) {
	if len(messages) > 1 && strings.HasPrefix(messages[1].Content, "Active checklist item") {
		r.mu.Lock()
		r.prompts = append(r.prompts, append([]contracts.ModelMessage{}, messages...))
		r.mu.Unlock()
	}
	return r.StubModel.Complete(ctx, messages, tools, maxTokens)
}

func memoryBlock(prompt []contracts.ModelMessage) string {
	task := prompt[1].Content
	start := strings.Index(task, MemoryHeader)
	if start < 0 {
		return ""
	}
	return strings.TrimSpace(task[start:strings.Index(task, "Recent commits:")])
}

func gitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	ws := filepath.Join(t.TempDir(), "ws")
	ctx := context.Background()
	if err := state.InitRepo(ctx, ws); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := state.CommitAll(ctx, ws, "init"); err != nil {
		t.Fatal(err)
	}
	return ws
}

func memoryRun(t *testing.T, ws string, settings *config.Settings, reply string, check contracts.Check) (*recording, MissionSummary) {
	t.Helper()
	m := &recording{StubModel: model.NewStub([]contracts.TurnResult{text(reply)})}
	o := runOpts(t, ws, settings, m, check)
	o.Checklist = contracts.Checklist{SchemaVersion: 1, Items: []contracts.ChecklistItem{item("01", "fix the config parser")}}
	s, err := RunMissionLocal(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return m, s
}

func TestPastFailuresAreRecalledIntoTheNextCyclesPrompt(t *testing.T) {
	ws := gitRepo(t, map[string]string{"config_parser.py": "def parse_config(text):\n    return {}\n"})
	settings, _ := storeSettings(t)
	m, s := memoryRun(t, ws, settings, `{"done": true, "summary": "tried a regex"}`, redCheck)
	if !strings.HasPrefix(s.StoppedReason, "deadlocked") || len(m.prompts) < 3 {
		t.Fatal(s.StoppedReason, len(m.prompts))
	}
	first, second, third := memoryBlock(m.prompts[0]), memoryBlock(m.prompts[1]), memoryBlock(m.prompts[2])
	if strings.Contains(first, "Earlier attempts") || !strings.Contains(first, "config_parser.py") {
		t.Fatal(first)
	}
	for _, want := range []string{"c1 [01] failed", "claimed: tried a regex", "red FAILED"} {
		if !strings.Contains(second, want) {
			t.Fatalf("%q missing:\n%s", want, second)
		}
	}
	if !strings.Contains(third, "c1 [01] failed") || !strings.Contains(third, "c2 [01] failed") {
		t.Fatal(third)
	}
	for _, block := range []string{first, second, third} {
		if len([]rune(block)) > settings.MemoryPromptBudgetChars {
			t.Fatal(len(block))
		}
	}
}

func TestMemoryPromptIsDeterministicAcrossIdenticalRuns(t *testing.T) {
	var blocks [2][]string
	for i := range blocks {
		ws := gitRepo(t, map[string]string{"app.py": "def main():\n    return 1\n"})
		settings, _ := storeSettings(t)
		m, _ := memoryRun(t, ws, settings, `{"done": true, "summary": "s"}`, redCheck)
		for _, p := range m.prompts {
			blocks[i] = append(blocks[i], memoryBlock(p))
		}
	}
	if strings.Join(blocks[0], "\x00") != strings.Join(blocks[1], "\x00") || len(blocks[0]) < 2 || blocks[0][1] == "" {
		t.Fatalf("%q\n%q", blocks[0], blocks[1])
	}
}

func TestMemoryDisabledLeavesThePromptUntouched(t *testing.T) {
	settings, _ := storeSettings(t, "LHA_MEMORY_ENABLED=false")
	m, _ := memoryRun(t, gitRepo(t, map[string]string{"a.py": "x = 1\n"}), settings, `{"done": true}`, redCheck)
	if len(m.prompts) == 0 {
		t.Fatal("no prompts")
	}
	for _, p := range m.prompts {
		if memoryBlock(p) != "" {
			t.Fatal(memoryBlock(p))
		}
	}
}

func TestMemoryBudgetSettingBoundsTheBlock(t *testing.T) {
	var big []string
	for i := 0; i < 200; i++ {
		big = append(big, "def parse_config_"+string(rune('a'+i%26))+"(text):\n    return 1")
	}
	ws := gitRepo(t, map[string]string{"config_parser.py": strings.Join(big, "\n")})
	settings, _ := storeSettings(t, "LHA_MEMORY_PROMPT_BUDGET_CHARS=600")
	m, _ := memoryRun(t, ws, settings, `{"done": true, "summary": "`+strings.Repeat("z", 900)+`"}`, redCheck)
	for _, p := range m.prompts {
		if n := len([]rune(memoryBlock(p))); n == 0 || n > 600 {
			t.Fatal(n)
		}
	}
}

func TestRunPathAdmitsASkillOnlyAfterVerification(t *testing.T) {
	settings, _ := storeSettings(t)
	_, s := memoryRun(t, gitRepo(t, map[string]string{"a.py": "x = 1\n"}), settings, `{"done": true, "summary": "fixed it"}`, passCheck)
	if !s.Completed || !strings.Contains(s.TraceJSONL, "skill_stored") {
		t.Fatal(s.StoppedReason)
	}
}

func TestLexicalOnlySettingStillRunsAMission(t *testing.T) {
	settings, _ := storeSettings(t, "LHA_MEMORY_EMBEDDER=none")
	_, s := memoryRun(t, gitRepo(t, map[string]string{"a.py": "x = 1\n"}), settings, `{"done": true}`, passCheck)
	if !s.Completed || !strings.Contains(s.TraceJSONL, "memory_degraded") || !strings.Contains(s.TraceJSONL, "LHA_MEMORY_EMBEDDER=none") {
		t.Fatal(s.TraceJSONL)
	}
}

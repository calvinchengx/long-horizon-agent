package org

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/memory"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
)

// promptRecorder says done and keeps every task message it was sent.
type promptRecorder struct {
	mu      sync.Mutex
	prompts []string
}

func (*promptRecorder) Name() string { return "fake:recorder" }
func (r *promptRecorder) Complete(_ context.Context, messages []contracts.ModelMessage, _ []map[string]any, _ int) (contracts.TurnResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(messages) > 1 {
		r.prompts = append(r.prompts, messages[1].Content)
	}
	return doneTurn, nil
}
func (*promptRecorder) EstimateCostUSD(contracts.Usage) (float64, error) { return 0, nil }

// The default services persist an orchestrated run like open_run_services does: the mission row,
// every org role's calls in the persistent ledger, and memory the Lead recalls on a later item.
func TestDefaultServicesPersistTheRunAndGiveTheLeadMemory(t *testing.T) {
	t.Parallel()
	dir, storePath := t.TempDir(), filepath.Join(t.TempDir(), "store.sqlite3")
	lead := &promptRecorder{}
	summary := run(t, []string{"LHA_SQLITE_PATH=" + storePath}, OrchestratorOptions{ResearchPerItem: 1, DoReview: true,
		Models: map[string]contracts.ModelProvider{"lead": lead, "researcher": stub(doneTurn), "reviewer": stub(approve)}},
		MissionOptions{Workdir: dir, Title: "Store test", Description: "persist the org", Checklist: checklist2(),
			Checks: []contracts.Check{pass}})
	if !summary.Completed {
		t.Fatalf("%+v", summary)
	}
	ctx := context.Background()
	store, err := persistence.OpenSQLite(ctx, storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	row, err := store.GetMission(ctx, summary.MissionID)
	if err != nil || row == nil || row.Status != "DONE" || row.Title != "Store test" || row.HeadSHA != summary.HeadSHA {
		t.Fatalf("mission row %+v %v", row, err)
	}
	costs, err := store.ListCosts(ctx, summary.MissionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]bool{}
	for _, c := range costs {
		roles[c.Role] = true
	}
	got := make([]string, 0, len(roles))
	for r := range roles {
		got = append(got, r)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "lead,researcher,reviewer" {
		t.Fatalf("ledger roles %v", got)
	}
	lead.mu.Lock()
	defer lead.mu.Unlock()
	if len(lead.prompts) != 2 || strings.Contains(lead.prompts[0], memory.MemoryHeader) || !strings.Contains(lead.prompts[1], memory.MemoryHeader) {
		t.Fatalf("the second item's lead prompt should recall the first cycle:\n%q", lead.prompts)
	}
}

func TestNoopServicesPersistNothing(t *testing.T) {
	var s RunServices = NoopServices{}
	if s.Memory() != nil || s.Running(context.Background()) != nil {
		t.Fatal("noop")
	}
}

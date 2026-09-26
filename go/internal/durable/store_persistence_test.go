package durable

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/coordination"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// The durable core over the Go mission store (python: tests/durability/test_mission_row.py and
// the ledger / hitl_gates assertions of the spine tests, against a real SQLite store).

func openTestStore(t *testing.T, acts *Activities, workdir string) persistence.Store {
	t.Helper()
	store, err := persistence.OpenStore(context.Background(), acts.Settings, workdir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// A mission run with the default store writes its missions row, every metered call to cost_ledger
// and its episodic memory to the configured SQLite store.
func TestMissionPersistsToTheGoMissionStore(t *testing.T) {
	inp := initMission(t, 2)
	acts := newActs(t, workingModel, nil) // DefaultStoreOpener: the Go mission store
	env := newEnv(t, acts, nil)
	res, err := env.run(inp)
	if err != nil || !res.Completed {
		t.Fatalf("result %+v err %v", res, err)
	}
	store := openTestStore(t, acts, inp.Workdir)
	ctx := context.Background()
	row, err := store.GetMission(ctx, inp.MissionID)
	if err != nil || row == nil {
		t.Fatalf("row %+v err %v", row, err)
	}
	if row.Status != StatusDone || row.Title != "Test mission" || row.HeadSHA != res.HeadSHA {
		t.Fatalf("row %+v (result head %s)", row, res.HeadSHA)
	}
	costs, err := store.ListCosts(ctx, inp.MissionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	cycles := map[string]bool{}
	for _, c := range costs {
		cycles[c.CycleID] = true
		if c.Role != "lead" {
			t.Fatalf("cost row %+v", c)
		}
	}
	if !cycles["c1"] || !cycles["c2"] {
		t.Fatalf("cost rows %+v", costs)
	}
	records, err := store.ListMemory(ctx, inp.MissionID, 0)
	if err != nil || len(records) == 0 {
		t.Fatalf("memory records %v err %v", records, err)
	}
}

// notify_gate writes hitl_gates through the default store and reports stored=true.
func TestNotifyGateWritesHitlGates(t *testing.T) {
	inp := initMission(t, 1)
	acts := newActs(t, idleModel, nil)
	res, err := acts.NotifyGate(context.Background(), GateNotice{
		MissionID: inp.MissionID, Workdir: inp.Workdir, GateID: "g1", Kind: "deadlock", Event: "opened",
		Question: "retry?", Options: []string{"retry", "abort"}, DefaultAction: "abort",
		At: "2026-01-01T00:00:00+00:00",
	})
	if err != nil || !res.Stored {
		t.Fatalf("result %+v err %v", res, err)
	}
	gates, err := openTestStore(t, acts, inp.Workdir).ListGates(context.Background(), inp.MissionID, 0)
	if err != nil || len(gates) != 1 || gates[0].GateID != "g1" || gates[0].Status != persistence.GateOpen {
		t.Fatalf("gates %+v err %v", gates, err)
	}
}

// An unusable store with fallback off is ErrStoreUnavailable, keeping the store's message.
func TestPersistenceStoreOpenerUnavailable(t *testing.T) {
	settings := testSettings(t, "LHA_POSTGRES_DSN=postgresql://u:p@127.0.0.1:1/none?connect_timeout=1",
		"LHA_POSTGRES_FALLBACK_TO_SQLITE=false")
	_, err := PersistenceStoreOpener(context.Background(), settings, t.TempDir())
	var unavailable *persistence.StoreUnavailableError
	if !errors.Is(err, ErrStoreUnavailable) || !errors.As(err, &unavailable) ||
		!strings.HasPrefix(err.Error(), "postgres unavailable") || pyTypeName(err) != "StoreUnavailableError" {
		t.Fatalf("err %v", err)
	}
}

// The ledger hook writes each call as it is metered, keyed <prefix>#<n>; a failing write is
// logged, never returned.
func TestLedgerHookKeysEachCall(t *testing.T) {
	store := &recordingStore{}
	handle, _ := store.opener(context.Background(), nil, "")
	hook := newLedgerHook(handle, "m1", "c3@2")
	for range 2 {
		if err := hook.RecordCost(context.Background(), governor.CostEntry{CycleID: "c3"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.costs) != 2 || store.costs[0] != "c3@2#0" || store.costs[1] != "c3@2#1" {
		t.Fatalf("keys %v", store.costs)
	}
	if err := newLedgerHook(failingCosts{handle}, "m1", "p").RecordCost(context.Background(), governor.CostEntry{}); err != nil {
		t.Fatalf("a failing write was returned: %v", err)
	}
}

type failingCosts struct{ Store }

func (failingCosts) RecordCost(context.Context, string, governor.CostEntry, string) (bool, error) {
	return false, errors.New("ledger down")
}

// lead_guard: no ownership map -> the plain dispatcher; with one -> an OwnershipGuard for the lead
// and the active item's writer.
func TestLeadGuard(t *testing.T) {
	inp := initMission(t, 2)
	ctx := context.Background()
	anchor := state.NewGitMissionAnchor(inp.Workdir)
	var inner contracts.ToolDispatcher = nopDispatcher{}
	got, err := LeadGuard(ctx, anchor, inner, "01")
	if err != nil || got != inner {
		t.Fatalf("got %T err %v", got, err)
	}
	owned := coordination.NewFileOwnershipMap()
	if err := owned.Assign("src/b.py", coordination.WriterForItem("02")); err != nil {
		t.Fatal(err)
	}
	if err := coordination.StageOwnership(anchor, owned); err != nil {
		t.Fatal(err)
	}
	checklist, err := anchor.ReadChecklist(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := anchor.CommitCheckpoint(ctx, contracts.Checkpoint{
		CycleID: "c0", ProgressSummary: "- ownership", Checklist: checklist, CommitMessage: "lha: ownership",
	}); err != nil {
		t.Fatal(err)
	}
	got, err = LeadGuard(ctx, anchor, inner, "01")
	guard, ok := got.(*coordination.OwnershipGuard)
	if err != nil || !ok {
		t.Fatalf("got %T err %v", got, err)
	}
	if w := guard.Writers(); len(w) != 2 || w[0] != coordination.Lead || w[1] != coordination.WriterForItem("01") {
		t.Fatalf("writers %v", w)
	}
}

type nopDispatcher struct{}

func (nopDispatcher) Specs() []contracts.ToolSpec { return nil }
func (nopDispatcher) Dispatch(context.Context, contracts.ToolCall, contracts.ToolContext) contracts.ToolResult {
	return contracts.ToolResult{}
}

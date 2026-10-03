package persistence

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
)

// Ported from python/tests/unit/test_mission_event_log.py.

func TestEventsAreWrittenInOrderStampedAndRedacted(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	log := NewMissionEventLog(store)
	recorder := obs.NewTraceRecorder(nil)
	recorder.AddListener(log.Add)
	recorder.Record("cycle_started", "m1", "c1", obs.F("item_id", "01"))
	recorder.Record("tool_call", "m1", "c1", obs.F("tool", "grep"), obs.F("ok", false),
		obs.F("error", "token=ghp_"+strings.Repeat("a", 36)))
	log.Close(ctx)
	rows := must(store.ReadMissionEvents(ctx, "", 0, 0))
	if len(rows) != 2 || rows[0].Kind != "cycle_started" || rows[1].Kind != "tool_call" || rows[0].CycleID != "c1" {
		t.Fatalf("%+v", rows)
	}
	if rows[0].Payload["item_id"] != "01" || rows[0].TS == "" {
		t.Fatalf("%+v", rows[0])
	}
	if strings.Contains(PyDumps(rows[1].Payload), "ghp_"+strings.Repeat("a", 36)) { // redacted before it is persisted
		t.Fatal(rows[1].Payload)
	}
	if log.Written() != 2 || log.Dropped() != 0 {
		t.Fatal(log.Written(), log.Dropped())
	}
}

type failingStore struct {
	Store
	calls int
}

func (f *failingStore) AppendMissionEvents(context.Context, []MissionEvent) error {
	f.calls++
	return errors.New("disk full")
}

func TestAStoreFailureDropsTheBatchAndNeverFails(t *testing.T) {
	store := &failingStore{}
	log := NewMissionEventLog(store)
	log.Add(obs.TraceEvent{Kind: "x", MissionID: "m", CycleID: "c"})
	log.Flush(context.Background())
	if log.Dropped() != 1 || log.Written() != 0 || store.calls != 1 {
		t.Fatal(log.Dropped(), log.Written(), store.calls)
	}
	log.Flush(context.Background()) // nothing pending: no write attempted
	if store.calls != 1 {
		t.Fatal(store.calls)
	}
}

func TestTheBacklogIsBoundedByDroppingTheOldest(t *testing.T) {
	log := NewMissionEventLog(&failingStore{})
	log.maxPending = 3
	for _, k := range []string{"k0", "k1", "k2", "k3", "k4"} {
		log.Add(obs.TraceEvent{Kind: k, MissionID: "m"})
	}
	if len(log.pending) != 3 || log.pending[0].Kind != "k2" || log.pending[2].Kind != "k4" || log.Dropped() != 2 {
		t.Fatalf("%+v %d", log.pending, log.Dropped())
	}
}

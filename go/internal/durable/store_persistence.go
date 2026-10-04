package durable

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
)

// The durable core's Store over the Go mission store (internal/persistence): the missions row
// (cycle tracker + record_mission_status), hitl_gates (notify_gate) and the cost ledger (the
// cycle's per-call ledger hook and mission-start's planner calls). python: open_store(settings,
// workdir=...) as the activities call it.

// storeUnavailable is a *persistence.StoreUnavailableError that also matches
// ErrStoreUnavailable, keeping the store's own message (python: str(StoreUnavailableError)).
type storeUnavailable struct{ err error }

func (e *storeUnavailable) Error() string        { return e.err.Error() }
func (e *storeUnavailable) Unwrap() error        { return e.err }
func (e *storeUnavailable) PyTypeName() string   { return "StoreUnavailableError" }
func (e *storeUnavailable) Is(target error) bool { return target == ErrStoreUnavailable }

// PersistenceStoreOpener opens the configured mission store (SQLite unless LHA_POSTGRES_DSN,
// under the checkout's .lha/ when LHA_SQLITE_PATH is relative). An unusable store with fallback
// off is an error wrapping ErrStoreUnavailable.
func PersistenceStoreOpener(ctx context.Context, settings *config.Settings, workdir string) (Store, error) {
	store, err := persistence.OpenStore(ctx, settings, workdir)
	if err != nil {
		var unavailable *persistence.StoreUnavailableError
		if errors.As(err, &unavailable) {
			return nil, &storeUnavailable{err: err}
		}
		return nil, err
	}
	return &PersistenceStore{store: store}, nil
}

// PersistenceStore adapts a persistence.Store to the durable Store.
type PersistenceStore struct{ store persistence.Store }

// NewPersistenceStore wraps an open persistence.Store.
func NewPersistenceStore(store persistence.Store) *PersistenceStore {
	return &PersistenceStore{store: store}
}

// Persistence is the underlying mission store (the cycle opens the memory plane over it).
func (p *PersistenceStore) Persistence() persistence.Store { return p.store }

// UpsertMission upserts the missions row (a terminal status is never moved back).
func (p *PersistenceStore) UpsertMission(ctx context.Context, row MissionRow) error {
	return p.store.UpsertMission(ctx, persistence.MissionUpsert{
		MissionID: row.MissionID, Title: row.Title, Description: row.Description, Status: row.Status,
		HeadSHA: row.HeadSHA, WorkflowID: row.WorkflowID, Workdir: persistence.AbsWorkdir(row.Workdir),
	})
}

// RecordGateEvent applies one gate event to hitl_gates (idempotent).
func (p *PersistenceStore) RecordGateEvent(ctx context.Context, e GateEvent) error {
	return p.store.RecordGateEvent(ctx, persistence.GateEvent{
		MissionID: e.MissionID, GateID: e.GateID, Kind: e.Kind, Event: e.Event, At: e.At,
		Question: e.Question, Options: e.Options, DefaultAction: e.DefaultAction, Deadline: e.Deadline,
		Decision: e.Decision, ResolvedBy: e.ResolvedBy, Step: e.Step, Risk: e.Risk, Request: e.Request,
	})
}

// RecordCost writes one ledger row under callKey (false when the key was already recorded).
func (p *PersistenceStore) RecordCost(ctx context.Context, missionID string, entry governor.CostEntry, callKey string) (bool, error) {
	return p.store.RecordCost(ctx, missionID, entry, callKey)
}

// Close closes the store.
func (p *PersistenceStore) Close(context.Context) error { return p.store.Close() }

// persistenceBacked is a Store that exposes its persistence.Store (for the memory plane).
type persistenceBacked interface {
	Persistence() persistence.Store
}

// ledgerHook is the cycle meter's CostHook (python: LedgerSink attached with
// key_prefix=f"{cycle_id}@{attempt}"): every metered call is written to the cost ledger as it
// happens, keyed "<prefix>#<n>", so a retried attempt's re-spent calls are new rows while a
// replayed write of the same call is a no-op. A store error is logged, never raised.
type ledgerHook struct {
	store     Store
	missionID string
	prefix    string

	mu  sync.Mutex
	seq int
}

var _ governor.CostHook = (*ledgerHook)(nil)

func newLedgerHook(store Store, missionID, prefix string) *ledgerHook {
	return &ledgerHook{store: store, missionID: missionID, prefix: prefix}
}

func (h *ledgerHook) RecordCost(ctx context.Context, entry governor.CostEntry) error {
	h.mu.Lock()
	key := h.prefix + "#" + strconv.Itoa(h.seq)
	h.seq++
	h.mu.Unlock()
	if _, err := h.store.RecordCost(context.WithoutCancel(ctx), h.missionID, entry, key); err != nil {
		activityLogger().Warn("cost_ledger_write_failed", "mission_id", h.missionID,
			"error", fmt.Sprintf("%s: %v", pyTypeName(err), err))
	}
	return nil
}

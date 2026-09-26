package durable

import (
	"context"
	"errors"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
)

// The persistence the durable core writes (python: lha.persistence.store.MissionStore through
// open_store / open_run_services / MissionTracker / LedgerSink): the missions row, the hitl_gates
// events and the persistent cost ledger. Store is the small slice of the mission store the durable
// core needs; DefaultStoreOpener is PersistenceStoreOpener, the Go mission store
// (internal/persistence, store_persistence.go). NopStoreOpener discards every write (tests,
// embeds), and NoticeResult.stored is then false.

// ErrStoreUnavailable marks a store that cannot be used at all (python: StoreUnavailableError);
// the activities turn it into a non-retryable MissionConfigError. A StoreOpener wraps it.
var ErrStoreUnavailable = errors.New("mission store unavailable")

// MissionRow is one upsert of the missions row (python: MissionStore.upsert_mission): empty
// Title/Description/HeadSHA/WorkflowID keep the stored values, and a terminal status is never
// moved back to a non-terminal one.
type MissionRow struct {
	MissionID   string
	Title       string
	Description string
	Status      string
	HeadSHA     string
	WorkflowID  string
}

// GateEvent is one human-gate event, as written to hitl_gates (python: persistence.store.GateEvent).
// At is when it happened (ISO-8601 UTC), stamped from workflow time on the durable path, so a
// retried write is idempotent.
type GateEvent struct {
	MissionID     string
	GateID        string
	Kind          string // "tool_call" | "deadlock"
	Event         string // "opened" | "reminder" | "resolved" | "defaulted"
	At            string
	Question      string
	Options       []string
	DefaultAction string
	Deadline      string
	Decision      string
	ResolvedBy    string
	Step          int
	Risk          string
	Request       map[string]string // nil = none
}

// Store is the slice of the mission store the durable core writes.
type Store interface {
	UpsertMission(ctx context.Context, row MissionRow) error
	RecordGateEvent(ctx context.Context, event GateEvent) error
	// RecordCost writes one metered call to the persistent cost ledger under callKey
	// (idempotent: a replayed key is a no-op, reported as false).
	RecordCost(ctx context.Context, missionID string, entry governor.CostEntry, callKey string) (bool, error)
	Close(ctx context.Context) error
}

// StoreOpener opens the mission store for a mission checkout (python: open_store(settings,
// workdir=...)). An error wrapping ErrStoreUnavailable is a configuration problem (not retried).
type StoreOpener func(ctx context.Context, settings *config.Settings, workdir string) (Store, error)

// DefaultStoreOpener is used when Activities.OpenStore is nil (and by lha mission-start): the
// configured Go mission store.
var DefaultStoreOpener StoreOpener = PersistenceStoreOpener

// NopStoreOpener opens a store that accepts and discards every write.
func NopStoreOpener(context.Context, *config.Settings, string) (Store, error) { return nopStore{}, nil }

type nopStore struct{}

func (nopStore) UpsertMission(context.Context, MissionRow) error  { return nil }
func (nopStore) RecordGateEvent(context.Context, GateEvent) error { return nil }
func (nopStore) RecordCost(context.Context, string, governor.CostEntry, string) (bool, error) {
	return false, nil
}
func (nopStore) Close(context.Context) error { return nil }

// IsNopStore reports whether s is the no-op store (the gate result then says stored=false).
func IsNopStore(s Store) bool {
	_, ok := s.(nopStore)
	return ok
}

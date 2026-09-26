// Package persistence is the mission store: one interface, two backends (SQLite by default,
// Postgres with a DSN). It mirrors python/src/lha/persistence.
//
// Everything a run persists outside git goes through Store:
//
//   - missions: one row per mission with its status transitions (RUNNING -> DONE / ABORTED / ...);
//   - hitl_gates: one row per human gate: opened, reminders, resolved or defaulted, by whom;
//   - cost_ledger: EVERY metered model call (usd is NULL when the cost is unknown), written
//     idempotently by key so a retried/replayed write never double-counts;
//   - episodic_events: cycle outcomes and memory bookkeeping (the episodic memory tier);
//   - semantic_memory: distilled facts / progress notes, optionally with embeddings;
//   - skills: verified, reusable know-how (the procedural tier).
//
// OpenStore picks the backend: LHA_POSTGRES_DSN set -> PostgresStore (tables from db/migrations;
// run `lha db migrate` first), otherwise SQLiteStore (WAL mode, schema created on open) at
// ResolveSQLitePath. If Postgres is configured but unusable, the run falls back to SQLite
// (postgres_fallback_to_sqlite) and the store says so via DegradedReason.
//
// The tables, columns, migration ids and row encodings are the Python implementation's, so a
// SQLite file (or a Postgres database) written by either implementation is read and written by
// the other.
//
// # API for the durable (Temporal) activities
//
// The durable workflow's activities use the same Store (OpenStore(settings, workdir)):
//
//   - record_mission_status: Store.UpsertMission(ctx, MissionUpsert{...}) — monotonic: a terminal
//     status (TerminalStatuses) is never replaced by a non-terminal one unless Reopen is set;
//     or MissionTracker.SetStatus, which logs instead of failing;
//   - the cost ledger: NewLedgerSink(store, missionID, keyPrefix) installed with
//     LedgerSink.Attach(meter) (every metered call) and LedgerSink.Backfill (calls recorded before
//     the sink existed); the keys are deterministic, so a replayed activity writes nothing twice;
//   - hitl_gates: Store.RecordGateEvent(ctx, GateEvent{...}) — idempotent per (mission, gate, event,
//     At): stamp At from workflow time so a retried write carries the same value.
package persistence

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
)

// Backend names.
const (
	BackendSQLite   = "sqlite"
	BackendPostgres = "postgres"
)

// RequiredPGMigrations are the Postgres migrations (db/migrations) the Postgres backend needs,
// checked when the store opens.
var RequiredPGMigrations = []string{
	"0001_init",
	"0002_idempotent_ledger",
	"0003_cost_unknown_usd_null",
	"0004_memory_skills",
	"0005_hitl_gates",
}

// StoreUnavailableError is the configured store being unusable (unreachable, unmigrated).
type StoreUnavailableError struct{ Message string }

func (e *StoreUnavailableError) Error() string { return e.Message }

// MissionRow is one missions row. HeadSHA and WorkflowID are "" when NULL.
type MissionRow struct {
	MissionID   string
	Title       string
	Status      string
	Description string
	HeadSHA     string
	WorkflowID  string
	CreatedAt   string
	UpdatedAt   string
}

// CostRow is one cost_ledger row. USD is nil when the cost is unknown (never recorded as $0).
type CostRow struct {
	MissionID    string
	CycleID      string
	Model        string
	Role         string
	InputTokens  int
	OutputTokens int
	USD          *float64
	CostKnown    bool
	TS           string
}

// CostSummary totals a mission's ledger: KnownUSD excludes unknown-cost calls.
type CostSummary struct {
	MissionID        string
	Calls            int
	KnownUSD         float64
	UnknownCostCalls int
	InputTokens      int
	OutputTokens     int
}

// EventRow is one episodic_events row. Payload numbers are json.Number.
type EventRow struct {
	ID        int64
	MissionID string
	CycleID   string
	Kind      string
	Payload   map[string]any
	TS        string
}

// TerminalStatuses are the missions.status values a mission ends in; the store never moves a
// row out of one to a non-terminal status (see Store.UpsertMission).
var TerminalStatuses = []string{"DONE", "IMPOSSIBLE", "ABORTED"}

// Mission statuses (python: lha.durable.signals).
const (
	StatusRunning        = "RUNNING"
	StatusSleeping       = "SLEEPING"
	StatusWaitingOnHuman = "WAITING_ON_HUMAN"
	StatusDegradedPark   = "DEGRADED_PARK"
	StatusDone           = "DONE"
	StatusAborted        = "ABORTED"
	StatusImpossible     = "IMPOSSIBLE"
)

// TerminalGuardSQL is the SQL for the status an upsert stores: existing stays when it is
// terminal, incoming is not, and reopen (a boolean SQL expression) is false.
func TerminalGuardSQL(existing, incoming, reopen string) string {
	quoted := make([]string, len(TerminalStatuses))
	for i, s := range TerminalStatuses {
		quoted[i] = "'" + s + "'"
	}
	terminal := strings.Join(quoted, ", ")
	return fmt.Sprintf("CASE WHEN %s IN (%s) AND %s NOT IN (%s) AND NOT %s THEN %s ELSE %s END",
		existing, terminal, incoming, terminal, reopen, existing, incoming)
}

// hitl_gates.status values. GateEscalated = open, at least one reminder sent.
const (
	GateOpen      = "OPEN"
	GateEscalated = "ESCALATED"
	GateResolved  = "RESOLVED"
	GateDefaulted = "DEFAULTED"
)

// GateEventStatus maps a gate event to the status it moves the row to.
var GateEventStatus = map[string]string{
	"opened":    GateOpen,
	"reminder":  GateEscalated,
	"resolved":  GateResolved,
	"defaulted": GateDefaulted,
}

// GateEvent is one human-gate event, as written to hitl_gates (Store.RecordGateEvent). At is
// when it happened (ISO-8601 UTC); the durable workflow stamps it from workflow time, so a
// retried write carries the same value and the write is idempotent. Request nil = NULL.
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
	Request       map[string]string
}

// GateRow is one gate's current state (hitl_gates); OpenedAt is its latest opening. Decision and
// ResolvedBy are nil when NULL.
type GateRow struct {
	MissionID     string
	GateID        string
	Kind          string
	Status        string
	Question      string
	Options       []string
	DefaultAction string
	Risk          string
	Deadline      string
	Decision      *string
	ResolvedBy    *string
	Reminders     int
	Request       map[string]string
	OpenedAt      string
	ResolvedAt    string
	UpdatedAt     string
}

// ValidateGateEvent is the row status event moves to (an error for an unknown event).
func ValidateGateEvent(event GateEvent) (string, error) {
	status, ok := GateEventStatus[event.Event]
	if !ok {
		return "", fmt.Errorf("unknown gate event %s", contracts.PyRepr(event.Event))
	}
	return status, nil
}

// gateUpdateKind is the ON CONFLICT rule an event uses: "closed", "reminder" or "opened".
func gateUpdateKind(status string) (kind string, closing bool) {
	closing = status == GateResolved || status == GateDefaulted
	switch {
	case closing:
		return "closed", true
	case status == GateEscalated:
		return "reminder", false
	}
	return "opened", false
}

// MissionUpsert is Store.UpsertMission's input. Empty Title/Description/HeadSHA/WorkflowID keep
// the stored values (never null them). Reopen lets a caller deliberately resume an ended mission
// under the same id.
type MissionUpsert struct {
	MissionID   string
	Title       string
	Status      string
	Description string
	HeadSHA     string
	WorkflowID  string
	Reopen      bool
}

// EventQuery filters Store.ListEvents: the newest Limit (default 200) matching events with
// id > AfterID, oldest first; Kinds nil = every kind.
type EventQuery struct {
	Kinds   []string
	AfterID int64
	Limit   int
}

// Embedding is the vector data Store.PutMemory stores with records (nil Vectors = lexical-only
// rows, model "none" version "0").
type Embedding struct {
	Vectors [][]float64
	Model   string
	Version string
}

// Store is the backend-neutral persistence for missions, spend and the memory tiers
// (python: MissionStore).
type Store interface {
	Backend() string
	// DegradedReason is non-empty when this store is a fallback for a configured-but-unusable
	// backend.
	DegradedReason() string

	// UpsertMission inserts or updates a mission row. Monotonic: a terminal status is never
	// replaced by a non-terminal one (a late write from a cycle still finishing when the mission
	// ended cannot turn ABORTED back into RUNNING); the other fields are still updated. Reopen
	// is the explicit exception.
	UpsertMission(ctx context.Context, m MissionUpsert) error
	GetMission(ctx context.Context, missionID string) (*MissionRow, error)
	// ListMissions is the most recently updated first (limit <= 0 => 20).
	ListMissions(ctx context.Context, limit int) ([]MissionRow, error)

	// RecordGateEvent applies one gate event to its hitl_gates row (key: mission + gate id).
	// Idempotent: "opened" (re)opens the row unless it repeats the same opening (same At);
	// "reminder" raises the reminder count on an open row; "resolved" / "defaulted" close an open
	// row and are no-ops on a closed one. An event whose row is missing inserts it.
	RecordGateEvent(ctx context.Context, event GateEvent) error
	// ListGates lists gates of missionID ("" = every mission), most recently opened first.
	ListGates(ctx context.Context, missionID string, limit int) ([]GateRow, error)

	// RecordCost inserts one ledger row; false if this logical call (key) was already recorded.
	RecordCost(ctx context.Context, missionID string, entry governor.CostEntry, callKey string) (bool, error)
	// ListCosts is oldest first (the most recent limit rows; limit <= 0 => 200).
	ListCosts(ctx context.Context, missionID string, limit int) ([]CostRow, error)
	CostSummary(ctx context.Context, missionID string) (CostSummary, error)

	AppendEvent(ctx context.Context, missionID, cycleID, kind string, payload map[string]any) (int64, error)
	ListEvents(ctx context.Context, missionID string, q EventQuery) ([]EventRow, error)

	// PutMemory upserts records by id (with their vectors when emb is non-nil).
	PutMemory(ctx context.Context, missionID string, records []contracts.MemoryRecord, emb *Embedding) error
	// ListMemory is the valid records of the mission, newest limit (<= 0 => 500), oldest first.
	ListMemory(ctx context.Context, missionID string, limit int) ([]contracts.MemoryRecord, error)
	// InvalidateMemory soft-forgets (valid = false); never deletes. Returns rows changed.
	InvalidateMemory(ctx context.Context, ids []string) (int, error)

	// PutSkill upserts a VERIFIED skill (an unverified one is a *contracts.SkillNotVerifiedError).
	PutSkill(ctx context.Context, skill contracts.Skill) error
	// ListSkills is the skills in namespace plus "global" ones, newest first (limit <= 0 => 200).
	ListSkills(ctx context.Context, namespace string, limit int) ([]contracts.Skill, error)

	Close() error
}

// --- paths ---------------------------------------------------------------------------------------

var (
	warnedMu       sync.Mutex
	warnedRelative = map[string]bool{}
)

func logger() *slog.Logger { return slog.Default().With("logger", "lha.persistence") }

// ConfiguredSQLitePath is settings' sqlite_path made absolute, or the per-user default when it
// is empty (python: configured_sqlite_path). A relative LHA_SQLITE_PATH still resolves against
// this process's working directory, so processes started from different directories would use
// different stores: a warning says so (once per path and process).
func ConfiguredSQLitePath(settings *config.Settings) string {
	configured := strings.TrimSpace(settings.SQLitePathSetting)
	path := settings.SQLitePath()
	if configured == "" {
		return path
	}
	raw := configured
	if strings.HasPrefix(raw, "~") {
		raw = "/" // expanduser makes it absolute
	}
	if !filepath.IsAbs(raw) {
		warnedMu.Lock()
		first := !warnedRelative[configured]
		warnedRelative[configured] = true
		warnedMu.Unlock()
		if first {
			logger().Warn("sqlite_path_relative", "configured", configured, "path", path,
				"reason", "LHA_SQLITE_PATH is relative: it resolves against the working directory, so "+
					"processes started elsewhere use another store; set an absolute path")
		}
	}
	return path
}

func isWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// ResolveSQLitePath is the SQLite store's absolute path (ConfiguredSQLitePath), relocated under
// <workdir>/.git/lha/ if it would land inside workdir ("" = no workdir): a mission checkout is
// reset/cleaned and committed, so the database never lives there.
func ResolveSQLitePath(settings *config.Settings, workdir string) string {
	path := ConfiguredSQLitePath(settings)
	if workdir == "" {
		return path
	}
	root := config.ResolvePath(config.ExpandUser(workdir))
	if isWithin(path, root) {
		relocated := filepath.Join(root, ".git", "lha", filepath.Base(path))
		logger().Warn("sqlite_path_relocated", "configured", path, "path", relocated,
			"reason", "inside the mission checkout")
		return relocated
	}
	return path
}

// DescribeStore says where OpenStore(settings, "") reads and writes, for humans.
func DescribeStore(settings *config.Settings) string {
	if settings.PostgresDSN.Value() != "" {
		fallback := ""
		if settings.PostgresFallbackToSQLite {
			fallback = " (falls back to SQLite at " + ResolveSQLitePath(settings, "") + ")"
		}
		return "postgres (LHA_POSTGRES_DSN)" + fallback
	}
	return "sqlite " + ResolveSQLitePath(settings, "")
}

// OpenStore opens the configured store: Postgres when postgres_dsn is set, else SQLite. An
// unusable Postgres falls back to SQLite (DegradedReason says why) unless
// postgres_fallback_to_sqlite is false, which returns a *StoreUnavailableError.
func OpenStore(ctx context.Context, settings *config.Settings, workdir string) (Store, error) {
	degraded := ""
	if dsn := settings.PostgresDSN.Value(); dsn != "" {
		pg, err := OpenPostgres(ctx, dsn)
		if err == nil {
			return pg, nil
		}
		degraded = fmt.Sprintf("postgres unavailable (%s: %s)", errorTypeName(err), err)
		if !settings.PostgresFallbackToSQLite {
			return nil, &StoreUnavailableError{Message: degraded}
		}
		logger().Warn("store_fallback_sqlite", "reason", degraded)
	}
	store, err := OpenSQLite(ctx, ResolveSQLitePath(settings, workdir))
	if err != nil {
		return nil, err
	}
	store.degraded = degraded
	return store, nil
}

// errorTypeName is a short Python-like class name for err (the message's "TypeName: message").
func errorTypeName(err error) string {
	var unavailable *StoreUnavailableError
	if errors.As(err, &unavailable) {
		return "StoreUnavailableError"
	}
	if isConnectError(err) {
		return "OperationalError"
	}
	return "Error"
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"

	_ "modernc.org/sqlite" // pure-Go SQLite driver ("sqlite")
)

// SQLite backend of Store (python: lha.persistence.sqlite; the zero-infra default).
//
// The tables mirror db/migrations (0001-0005) with SQLite types: timestamps are ISO-8601 UTC
// text, jsonb is JSON text, vectors are JSON arrays of floats. The schema is created/upgraded on
// open and versioned in schema_migrations (versions prefixed "sqlite_") — the same versions and
// scripts as Python, so either implementation opens a file the other created.
//
// The database runs in WAL mode with a busy timeout, so a CLI reader (`lha missions`) and a
// running mission (or several processes) can use the same file. One connection per store is
// shared under a lock.

// BusyTimeoutMS is the SQLite busy timeout.
const BusyTimeoutMS = 10_000

// SQLiteMigrations are (version, script) in order. Append new versions; never edit an applied
// one. They are Python's SQLITE_MIGRATIONS verbatim.
var SQLiteMigrations = [][2]string{
	{"sqlite_0001_init", `
        CREATE TABLE IF NOT EXISTS missions (
            mission_id         TEXT PRIMARY KEY,
            title              TEXT NOT NULL,
            description        TEXT NOT NULL DEFAULT '',
            acceptance         TEXT NOT NULL DEFAULT '',
            status             TEXT NOT NULL DEFAULT 'RUNNING',
            workflow_id        TEXT,
            run_id             TEXT,
            latest_snapshot_id TEXT,
            latest_session_id  TEXT,
            head_sha           TEXT,
            schema_version     INTEGER NOT NULL DEFAULT 1,
            created_at         TEXT NOT NULL,
            updated_at         TEXT NOT NULL
        );
        CREATE TABLE IF NOT EXISTS cost_ledger (
            id              INTEGER PRIMARY KEY AUTOINCREMENT,
            mission_id      TEXT NOT NULL,
            cycle_id        TEXT NOT NULL DEFAULT '',
            ts              TEXT NOT NULL,
            model           TEXT NOT NULL,
            input_tokens    INTEGER NOT NULL DEFAULT 0,
            output_tokens   INTEGER NOT NULL DEFAULT 0,
            usd             REAL,
            idempotency_key TEXT UNIQUE,
            role            TEXT NOT NULL DEFAULT '',
            cost_known      INTEGER NOT NULL DEFAULT 1
        );
        CREATE INDEX IF NOT EXISTS cost_ledger_mission_ts ON cost_ledger (mission_id, ts);
        CREATE TABLE IF NOT EXISTS episodic_events (
            id              INTEGER PRIMARY KEY AUTOINCREMENT,
            mission_id      TEXT NOT NULL,
            cycle_id        TEXT NOT NULL DEFAULT '',
            ts              TEXT NOT NULL,
            kind            TEXT NOT NULL,
            payload         TEXT NOT NULL DEFAULT '{}',
            payload_ref     TEXT,
            schema_version  INTEGER NOT NULL DEFAULT 1
        );
        CREATE INDEX IF NOT EXISTS episodic_events_mission ON episodic_events (mission_id, id);
        CREATE TABLE IF NOT EXISTS semantic_memory (
            id                TEXT PRIMARY KEY,
            mission_id        TEXT,
            kind              TEXT NOT NULL DEFAULT 'semantic',
            text              TEXT NOT NULL,
            metadata          TEXT NOT NULL DEFAULT '{}',
            embedding         TEXT,
            embedding_model   TEXT NOT NULL,
            embedding_version TEXT NOT NULL,
            valid             INTEGER NOT NULL DEFAULT 1,
            source_event_id   INTEGER,
            created_at        TEXT NOT NULL,
            schema_version    INTEGER NOT NULL DEFAULT 1
        );
        CREATE INDEX IF NOT EXISTS semantic_memory_mission ON semantic_memory (mission_id);
        CREATE INDEX IF NOT EXISTS semantic_memory_modelver
            ON semantic_memory (embedding_model, embedding_version);
        CREATE TABLE IF NOT EXISTS skills (
            id             TEXT PRIMARY KEY,
            namespace      TEXT NOT NULL DEFAULT 'global',
            name           TEXT NOT NULL,
            description    TEXT NOT NULL,
            code           TEXT NOT NULL DEFAULT '',
            preconditions  TEXT NOT NULL DEFAULT '[]',
            provenance     TEXT NOT NULL DEFAULT '',
            expires_at     TEXT,
            verified       INTEGER NOT NULL DEFAULT 0,
            uses           INTEGER NOT NULL DEFAULT 0,
            created_at     TEXT NOT NULL,
            updated_at     TEXT NOT NULL,
            schema_version INTEGER NOT NULL DEFAULT 1
        );
        CREATE INDEX IF NOT EXISTS skills_namespace ON skills (namespace);
        `},
	// The Postgres hitl_gates table (0001 + 0005): one row per (mission, gate).
	{"sqlite_0002_hitl_gates", `
        CREATE TABLE IF NOT EXISTS hitl_gates (
            mission_id     TEXT NOT NULL,
            gate_id        TEXT NOT NULL,
            kind           TEXT NOT NULL DEFAULT '',
            question       TEXT NOT NULL DEFAULT '',
            risk           TEXT NOT NULL DEFAULT '',
            default_action TEXT NOT NULL DEFAULT '',
            options        TEXT NOT NULL DEFAULT '[]',
            request        TEXT,
            status         TEXT NOT NULL DEFAULT 'OPEN',
            deadline       TEXT,
            decision       TEXT,
            resolved_by    TEXT,
            reminders      INTEGER NOT NULL DEFAULT 0,
            created_at     TEXT NOT NULL,
            resolved_at    TEXT,
            updated_at     TEXT NOT NULL,
            PRIMARY KEY (mission_id, gate_id)
        );
        CREATE INDEX IF NOT EXISTS hitl_gates_opened ON hitl_gates (created_at);
        `},
	// The Postgres mission_events table (0006): the shared event record a reader follows.
	{"sqlite_0003_mission_events", `
        CREATE TABLE IF NOT EXISTS mission_events (
            id              INTEGER PRIMARY KEY AUTOINCREMENT,
            mission_id      TEXT NOT NULL,
            cycle_id        TEXT NOT NULL DEFAULT '',
            ts              TEXT NOT NULL,
            kind            TEXT NOT NULL,
            payload         TEXT NOT NULL DEFAULT '{}',
            schema_version  INTEGER NOT NULL DEFAULT 1
        );
        CREATE INDEX IF NOT EXISTS mission_events_mission ON mission_events (mission_id, id);
        `},
}

// Per gate event: the ON CONFLICT update (excluded = the incoming event's row). "opened" reopens
// the row unless it is a retry of the same opening; the others touch only an open row.
var sqliteGateUpdate = map[string]string{
	"opened": "kind = excluded.kind, question = excluded.question, risk = excluded.risk, " +
		"default_action = excluded.default_action, options = excluded.options, " +
		"request = excluded.request, status = excluded.status, deadline = excluded.deadline, " +
		"decision = NULL, resolved_by = NULL, resolved_at = NULL, reminders = 0, " +
		"created_at = excluded.created_at, updated_at = excluded.updated_at " +
		"WHERE hitl_gates.created_at != excluded.created_at",
	"reminder": "status = excluded.status, reminders = MAX(hitl_gates.reminders, excluded.reminders), " +
		"updated_at = excluded.updated_at WHERE hitl_gates.status IN ('OPEN', 'ESCALATED')",
	"closed": "status = excluded.status, decision = excluded.decision, " +
		"resolved_by = excluded.resolved_by, resolved_at = excluded.resolved_at, " +
		"updated_at = excluded.updated_at WHERE hitl_gates.status IN ('OPEN', 'ESCALATED')",
}

// SQLiteStore is Store on a local SQLite file.
type SQLiteStore struct {
	Path     string
	degraded string

	mu   sync.Mutex
	db   *sql.DB
	conn *sql.Conn
}

var _ Store = (*SQLiteStore)(nil)

// NewSQLiteStore is a store on path (opened lazily, or by Open).
func NewSQLiteStore(path string) *SQLiteStore {
	p := path
	if p != ":memory:" {
		p = config.ResolvePath(config.ExpandUser(path))
	}
	return &SQLiteStore{Path: p}
}

// OpenSQLite opens (creating and migrating) the SQLite store at path.
func OpenSQLite(ctx context.Context, path string) (*SQLiteStore, error) {
	s := NewSQLiteStore(path)
	if err := s.Open(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Backend is "sqlite".
func (s *SQLiteStore) Backend() string { return BackendSQLite }

// DegradedReason is non-empty when this store stands in for an unusable Postgres.
func (s *SQLiteStore) DegradedReason() string { return s.degraded }

// SetDegradedReason marks the store as a fallback (tests and OpenStore).
func (s *SQLiteStore) SetDegradedReason(reason string) { s.degraded = reason }

// Open creates the file (and its directory), sets WAL mode and applies pending migrations.
func (s *SQLiteStore) Open(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.openLocked(ctx)
}

// openLocked opens the connection, converting a fresh file to WAL and applying migrations.
// SQLite answers a concurrent first open of one file (two processes converting it to WAL, or
// creating the schema, at once) with SQLITE_BUSY without consulting the busy handler, so a busy
// open is retried for up to BusyTimeoutMS.
func (s *SQLiteStore) openLocked(ctx context.Context) error {
	if s.conn != nil {
		return nil
	}
	deadline := time.Now().Add(time.Duration(BusyTimeoutMS) * time.Millisecond)
	for wait := 20 * time.Millisecond; ; wait = min(wait*2, 500*time.Millisecond) {
		err := s.openOnce(ctx)
		if err == nil || !isBusy(err) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// isBusy reports whether err is SQLite's "database is locked" (SQLITE_BUSY) in any wrapping.
func isBusy(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") || strings.Contains(msg, "database is locked")
}

func (s *SQLiteStore) openOnce(ctx context.Context) error {
	if s.Path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
			return err
		}
	}
	dsn := s.Path +
		fmt.Sprintf("?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)", BusyTimeoutMS)
	if s.Path == ":memory:" {
		dsn = ":memory:"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return err
	}
	if err := migrateSQLite(ctx, conn); err != nil {
		conn.Close()
		db.Close()
		return err
	}
	s.db, s.conn = db, conn
	return nil
}

func migrateSQLite(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations "+
		"(version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)"); err != nil {
		return err
	}
	applied := map[string]bool{}
	rows, err := conn.QueryContext(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return err
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()
	for _, m := range SQLiteMigrations {
		version, script := m[0], m[1]
		if applied[version] {
			continue
		}
		if err := applySQLiteMigration(ctx, conn, version, script); err != nil {
			return err
		}
	}
	return nil
}

func applySQLiteMigration(ctx context.Context, conn *sql.Conn, version, script string) (err error) {
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()
	// Re-check under the write lock: another process may have just applied it.
	var one int
	switch err := conn.QueryRowContext(ctx, "SELECT 1 FROM schema_migrations WHERE version = ?", version).Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		for _, statement := range strings.Split(script, ";") {
			if strings.TrimSpace(statement) == "" {
				continue
			}
			if _, err := conn.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)",
			version, NowISO()); err != nil {
			return err
		}
	case err != nil:
		return err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}

// run executes fn on the (lazily opened) connection under the store's lock.
func (s *SQLiteStore) run(ctx context.Context, fn func(*sql.Conn) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.openLocked(ctx); err != nil {
		return err
	}
	return fn(s.conn)
}

// JournalMode is the database's journal mode ("wal").
func (s *SQLiteStore) JournalMode(ctx context.Context) (string, error) {
	var mode string
	err := s.run(ctx, func(c *sql.Conn) error {
		return c.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode)
	})
	return mode, err
}

// Close closes the connection (the store reopens on next use).
func (s *SQLiteStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return nil
	}
	err := errors.Join(s.conn.Close(), s.db.Close())
	s.conn, s.db = nil, nil
	return err
}

// --- missions ----------------------------------------------------------------------------------

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// UpsertMission inserts or updates (see Store).
func (s *SQLiteStore) UpsertMission(ctx context.Context, m MissionUpsert) error {
	now := NowISO()
	statusSQL := TerminalGuardSQL("missions.status", "excluded.status", "?")
	return s.run(ctx, func(c *sql.Conn) error {
		_, err := c.ExecContext(ctx,
			"INSERT INTO missions (mission_id, title, description, status, head_sha, "+
				"workflow_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?) "+
				"ON CONFLICT (mission_id) DO UPDATE SET "+
				"title = CASE WHEN excluded.title != '' THEN excluded.title ELSE title END, "+
				"description = CASE WHEN excluded.description != '' "+
				"THEN excluded.description ELSE description END, "+
				"status = "+statusSQL+", "+
				"head_sha = COALESCE(NULLIF(excluded.head_sha, ''), head_sha), "+
				"workflow_id = COALESCE(excluded.workflow_id, workflow_id), "+
				"updated_at = excluded.updated_at",
			m.MissionID, m.Title, m.Description, m.Status, nullable(m.HeadSHA), nullable(m.WorkflowID),
			now, now, boolInt(m.Reopen))
		return err
	})
}

const sqliteMissionCols = "mission_id, title, status, description, head_sha, workflow_id, created_at, updated_at"

func scanMission(sc interface{ Scan(...any) error }) (MissionRow, error) {
	var r MissionRow
	var head, wf sql.NullString
	err := sc.Scan(&r.MissionID, &r.Title, &r.Status, &r.Description, &head, &wf, &r.CreatedAt, &r.UpdatedAt)
	r.HeadSHA, r.WorkflowID = head.String, wf.String
	return r, err
}

// GetMission is the row, or nil.
func (s *SQLiteStore) GetMission(ctx context.Context, missionID string) (*MissionRow, error) {
	var out *MissionRow
	err := s.run(ctx, func(c *sql.Conn) error {
		row, err := scanMission(c.QueryRowContext(ctx, "SELECT "+sqliteMissionCols+" FROM missions WHERE mission_id = ?", missionID))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err == nil {
			out = &row
		}
		return err
	})
	return out, err
}

// ListMissions is the most recently updated first.
func (s *SQLiteStore) ListMissions(ctx context.Context, limit int) ([]MissionRow, error) {
	if limit <= 0 {
		limit = 20
	}
	out := []MissionRow{}
	err := s.run(ctx, func(c *sql.Conn) error {
		rows, err := c.QueryContext(ctx, "SELECT "+sqliteMissionCols+" FROM missions ORDER BY updated_at DESC, mission_id LIMIT ?", limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanMission(rows)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// --- human gates -------------------------------------------------------------------------------

func gateRequestJSON(request map[string]string) any {
	if request == nil {
		return nil
	}
	return PyDumps(request)
}

func optionsList(options []string) []string {
	if options == nil {
		return []string{}
	}
	return options
}

// RecordGateEvent applies one gate event (see Store).
func (s *SQLiteStore) RecordGateEvent(ctx context.Context, event GateEvent) error {
	status, err := ValidateGateEvent(event)
	if err != nil {
		return err
	}
	kind, closing := gateUpdateKind(status)
	risk := event.Risk
	if risk == "" {
		risk = event.Kind
	}
	var decision, resolvedBy, resolvedAt any
	if closing {
		decision, resolvedBy, resolvedAt = nullable(event.Decision), nullable(event.ResolvedBy), event.At
	}
	reminders := 0
	if status == GateEscalated {
		reminders = event.Step
	}
	return s.run(ctx, func(c *sql.Conn) error {
		_, err := c.ExecContext(ctx,
			"INSERT INTO hitl_gates (mission_id, gate_id, kind, question, risk, "+
				"default_action, options, request, status, deadline, decision, resolved_by, "+
				"reminders, created_at, resolved_at, updated_at) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "+
				"ON CONFLICT (mission_id, gate_id) DO UPDATE SET "+sqliteGateUpdate[kind],
			event.MissionID, event.GateID, event.Kind, event.Question, risk, event.DefaultAction,
			PyDumps(optionsList(event.Options)), gateRequestJSON(event.Request), status,
			nullable(event.Deadline), decision, resolvedBy, reminders, event.At, resolvedAt, NowISO())
		return err
	})
}

// ListGates lists gates, most recently opened first.
func (s *SQLiteStore) ListGates(ctx context.Context, missionID string, limit int) ([]GateRow, error) {
	if limit <= 0 {
		limit = 50
	}
	where, params := "", []any{}
	if missionID != "" {
		where, params = "WHERE mission_id = ? ", []any{missionID}
	}
	out := []GateRow{}
	err := s.run(ctx, func(c *sql.Conn) error {
		rows, err := c.QueryContext(ctx,
			"SELECT mission_id, gate_id, kind, status, question, options, default_action, risk, "+
				"deadline, decision, resolved_by, reminders, request, created_at, resolved_at, "+
				"updated_at FROM hitl_gates "+where+
				"ORDER BY created_at DESC, mission_id, gate_id LIMIT ?", append(params, limit)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r GateRow
			var options string
			var deadline, decision, resolvedBy, request, resolvedAt sql.NullString
			if err := rows.Scan(&r.MissionID, &r.GateID, &r.Kind, &r.Status, &r.Question, &options,
				&r.DefaultAction, &r.Risk, &deadline, &decision, &resolvedBy, &r.Reminders, &request,
				&r.OpenedAt, &resolvedAt, &r.UpdatedAt); err != nil {
				return err
			}
			if r.Options, err = decodeStringList(options); err != nil {
				return err
			}
			r.Deadline, r.ResolvedAt = deadline.String, resolvedAt.String
			r.Decision, r.ResolvedBy = nullStr(decision), nullStr(resolvedBy)
			if request.String != "" {
				if r.Request, err = DecodeStringMap(request.String); err != nil {
					return err
				}
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

func nullStr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	v := n.String
	return &v
}

// --- cost ledger -------------------------------------------------------------------------------

// RecordCost inserts one ledger row (idempotent by key; usd NULL when unknown).
func (s *SQLiteStore) RecordCost(ctx context.Context, missionID string, entry governor.CostEntry, callKey string) (bool, error) {
	key := CostIdempotencyKey(missionID, entry.CycleID, callKey)
	var usd any
	if entry.CostKnown {
		usd = entry.USD
	}
	inserted := false
	err := s.run(ctx, func(c *sql.Conn) error {
		res, err := c.ExecContext(ctx,
			"INSERT OR IGNORE INTO cost_ledger (mission_id, cycle_id, ts, model, "+
				"input_tokens, output_tokens, usd, idempotency_key, role, cost_known) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			missionID, entry.CycleID, NowISO(), entry.Model, entry.InputTokens, entry.OutputTokens,
			usd, key, entry.Role, boolInt(entry.CostKnown))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		inserted = n > 0
		return err
	})
	return inserted, err
}

// ListCosts is oldest first (the most recent limit rows).
func (s *SQLiteStore) ListCosts(ctx context.Context, missionID string, limit int) ([]CostRow, error) {
	if limit <= 0 {
		limit = 200
	}
	out := []CostRow{}
	err := s.run(ctx, func(c *sql.Conn) error {
		rows, err := c.QueryContext(ctx,
			"SELECT mission_id, cycle_id, model, role, input_tokens, output_tokens, usd, cost_known, ts "+
				"FROM (SELECT * FROM cost_ledger WHERE mission_id = ? ORDER BY id DESC LIMIT ?) ORDER BY id",
			missionID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r CostRow
			var usd sql.NullFloat64
			var known int
			if err := rows.Scan(&r.MissionID, &r.CycleID, &r.Model, &r.Role, &r.InputTokens,
				&r.OutputTokens, &usd, &known, &r.TS); err != nil {
				return err
			}
			if usd.Valid {
				v := usd.Float64
				r.USD = &v
			}
			r.CostKnown = known != 0
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// CostSummary totals the mission's ledger.
func (s *SQLiteStore) CostSummary(ctx context.Context, missionID string) (CostSummary, error) {
	sum := CostSummary{MissionID: missionID}
	err := s.run(ctx, func(c *sql.Conn) error {
		return c.QueryRowContext(ctx,
			"SELECT COUNT(*), COALESCE(SUM(usd), 0), "+
				"COALESCE(SUM(CASE WHEN cost_known = 0 THEN 1 ELSE 0 END), 0), "+
				"COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0) "+
				"FROM cost_ledger WHERE mission_id = ?", missionID,
		).Scan(&sum.Calls, &sum.KnownUSD, &sum.UnknownCostCalls, &sum.InputTokens, &sum.OutputTokens)
	})
	return sum, err
}

// --- episodic events ---------------------------------------------------------------------------

// AppendEvent appends one event; returns its id.
func (s *SQLiteStore) AppendEvent(ctx context.Context, missionID, cycleID, kind string, payload map[string]any) (int64, error) {
	body := PyDumps(payloadOrEmpty(payload))
	var id int64
	err := s.run(ctx, func(c *sql.Conn) error {
		res, err := c.ExecContext(ctx,
			"INSERT INTO episodic_events (mission_id, cycle_id, ts, kind, payload) VALUES (?, ?, ?, ?, ?)",
			missionID, cycleID, NowISO(), kind, body)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

func payloadOrEmpty(p map[string]any) map[string]any {
	if p == nil {
		return map[string]any{}
	}
	return p
}

// ListEvents is the newest q.Limit matching events with id > q.AfterID, oldest first.
func (s *SQLiteStore) ListEvents(ctx context.Context, missionID string, q EventQuery) ([]EventRow, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}
	query := "SELECT * FROM episodic_events WHERE mission_id = ? AND id > ?"
	params := []any{missionID, q.AfterID}
	if len(q.Kinds) > 0 {
		query += " AND kind IN (" + strings.TrimSuffix(strings.Repeat("?, ", len(q.Kinds)), ", ") + ")"
		for _, k := range q.Kinds {
			params = append(params, k)
		}
	}
	query = "SELECT id, mission_id, cycle_id, kind, payload, ts FROM (" + query + " ORDER BY id DESC LIMIT ?) ORDER BY id"
	params = append(params, limit)
	out := []EventRow{}
	err := s.run(ctx, func(c *sql.Conn) error {
		rows, err := c.QueryContext(ctx, query, params...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r EventRow
			var payload sql.NullString
			if err := rows.Scan(&r.ID, &r.MissionID, &r.CycleID, &r.Kind, &payload, &r.TS); err != nil {
				return err
			}
			if r.Payload, err = decodeObject(payload.String); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// AppendMissionEvents appends to the shared event record, in order, in one transaction.
func (s *SQLiteStore) AppendMissionEvents(ctx context.Context, events []MissionEvent) error {
	if len(events) == 0 {
		return nil
	}
	now := NowISO()
	return s.run(ctx, func(c *sql.Conn) error {
		tx, err := c.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, e := range events {
			ts := e.TS
			if ts == "" {
				ts = now
			}
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO mission_events (mission_id, cycle_id, ts, kind, payload) VALUES (?, ?, ?, ?, ?)",
				e.MissionID, e.CycleID, ts, e.Kind, PyDumps(payloadOrEmpty(e.Payload))); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		return tx.Commit()
	})
}

// ReadMissionEvents is the oldest limit events with id > afterID (one mission, or all), oldest first.
func (s *SQLiteStore) ReadMissionEvents(ctx context.Context, missionID string, afterID int64, limit int) ([]EventRow, error) {
	if limit <= 0 {
		limit = 500
	}
	query := "SELECT id, mission_id, cycle_id, kind, payload, ts FROM mission_events WHERE id > ?"
	params := []any{afterID}
	if missionID != "" {
		query += " AND mission_id = ?"
		params = append(params, missionID)
	}
	query += " ORDER BY id LIMIT ?"
	params = append(params, limit)
	out := []EventRow{}
	err := s.run(ctx, func(c *sql.Conn) error {
		rows, err := c.QueryContext(ctx, query, params...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r EventRow
			var payload sql.NullString
			if err := rows.Scan(&r.ID, &r.MissionID, &r.CycleID, &r.Kind, &payload, &r.TS); err != nil {
				return err
			}
			if r.Payload, err = decodeObject(payload.String); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// --- semantic memory ---------------------------------------------------------------------------

func metadataOrEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// PutMemory upserts records by id (with vectors when emb is non-nil).
func (s *SQLiteStore) PutMemory(ctx context.Context, missionID string, records []contracts.MemoryRecord, emb *Embedding) error {
	if len(records) == 0 {
		return nil
	}
	model, version := "none", "0"
	if emb != nil {
		if len(emb.Vectors) != len(records) {
			return errors.New("put_memory: one vector per record is required")
		}
		model, version = emb.Model, emb.Version
	}
	now := NowISO()
	return s.run(ctx, func(c *sql.Conn) error {
		for i, record := range records {
			var vector any
			if emb != nil {
				vector = PyDumps(emb.Vectors[i])
			}
			if _, err := c.ExecContext(ctx,
				"INSERT INTO semantic_memory (id, mission_id, kind, text, metadata, embedding, "+
					"embedding_model, embedding_version, valid, created_at) "+
					"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "+
					"ON CONFLICT (id) DO UPDATE SET mission_id = excluded.mission_id, "+
					"kind = excluded.kind, text = excluded.text, metadata = excluded.metadata, "+
					"embedding = excluded.embedding, embedding_model = excluded.embedding_model, "+
					"embedding_version = excluded.embedding_version, valid = excluded.valid",
				record.ID, missionID, record.Kind, record.Text, PyDumps(metadataOrEmpty(record.Metadata)),
				vector, model, version, boolInt(record.Valid), now); err != nil {
				return err
			}
		}
		return nil
	})
}

const sqliteMemoryCols = "id, kind, text, metadata, embedding_model, embedding_version, valid"

func scanRecord(sc interface{ Scan(...any) error }, extra ...any) (contracts.MemoryRecord, error) {
	var r contracts.MemoryRecord
	var metadata sql.NullString
	var valid int
	dest := append([]any{&r.ID, &r.Kind, &r.Text, &metadata, &r.EmbeddingModel, &r.EmbeddingVersion, &valid}, extra...)
	if err := sc.Scan(dest...); err != nil {
		return r, err
	}
	r.Valid = valid != 0
	md, err := DecodeStringMap(metadata.String)
	r.Metadata = md
	return r, err
}

// ListMemory is the mission's valid records, newest limit, oldest first.
func (s *SQLiteStore) ListMemory(ctx context.Context, missionID string, limit int) ([]contracts.MemoryRecord, error) {
	if limit <= 0 {
		limit = 500
	}
	out := []contracts.MemoryRecord{}
	err := s.run(ctx, func(c *sql.Conn) error {
		rows, err := c.QueryContext(ctx,
			"SELECT "+sqliteMemoryCols+" FROM (SELECT rowid AS rid, * FROM semantic_memory "+
				"WHERE mission_id = ? AND valid = 1 ORDER BY rowid DESC LIMIT ?) ORDER BY rid",
			missionID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRecord(rows)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// StoredVector is a record with its stored vector.
type StoredVector struct {
	Record contracts.MemoryRecord
	Vector []float64
}

// MemoryVectors is the valid records with a vector from exactly this embedder model+version
// (the SQLite dense channel), newest limit (<= 0 => 2000), oldest first.
func (s *SQLiteStore) MemoryVectors(ctx context.Context, missionID, model, version string, limit int) ([]StoredVector, error) {
	if limit <= 0 {
		limit = 2000
	}
	out := []StoredVector{}
	err := s.run(ctx, func(c *sql.Conn) error {
		rows, err := c.QueryContext(ctx,
			"SELECT "+sqliteMemoryCols+", embedding FROM (SELECT rowid AS rid, * FROM semantic_memory "+
				"WHERE mission_id = ? AND valid = 1 AND embedding IS NOT NULL "+
				"AND embedding_model = ? AND embedding_version = ? "+
				"ORDER BY rowid DESC LIMIT ?) ORDER BY rid",
			missionID, model, version, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var embedding string
			r, err := scanRecord(rows, &embedding)
			if err != nil {
				return err
			}
			var vector []float64
			if err := json.Unmarshal([]byte(embedding), &vector); err != nil {
				return err
			}
			out = append(out, StoredVector{Record: r, Vector: vector})
		}
		return rows.Err()
	})
	return out, err
}

func sqlitePH(int) string { return "?" }

// StaleMemory is the valid rows with no vector or another embedder's, oldest first.
func (s *SQLiteStore) StaleMemory(ctx context.Context, missionID, model, version string, limit int) ([]StaleRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	where, _ := staleWhere(missionID, sqlitePH, " = 1")
	out := []StaleRecord{}
	err := s.run(ctx, func(c *sql.Conn) error {
		rows, err := c.QueryContext(ctx,
			"SELECT "+sqliteMemoryCols+", mission_id FROM semantic_memory "+where+" ORDER BY rowid LIMIT ?",
			append(staleArgs(missionID, model, version), limit)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var mission string
			r, err := scanRecord(rows, &mission)
			if err != nil {
				return err
			}
			out = append(out, StaleRecord{MissionID: mission, Record: r})
		}
		return rows.Err()
	})
	return out, err
}

// CountStaleMemory is how many StaleMemory rows each mission has.
func (s *SQLiteStore) CountStaleMemory(ctx context.Context, missionID, model, version string) (map[string]int, error) {
	where, _ := staleWhere(missionID, sqlitePH, " = 1")
	out := map[string]int{}
	err := s.run(ctx, func(c *sql.Conn) error {
		rows, err := c.QueryContext(ctx,
			"SELECT mission_id, COUNT(*) FROM semantic_memory "+where+" GROUP BY mission_id",
			staleArgs(missionID, model, version)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var mission string
			var n int
			if err := rows.Scan(&mission, &n); err != nil {
				return err
			}
			out[mission] = n
		}
		return rows.Err()
	})
	return out, err
}

// InvalidateMemory soft-forgets ids; returns rows changed.
func (s *SQLiteStore) InvalidateMemory(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	params := make([]any, len(ids))
	for i, id := range ids {
		params[i] = id
	}
	n := 0
	err := s.run(ctx, func(c *sql.Conn) error {
		res, err := c.ExecContext(ctx, "UPDATE semantic_memory SET valid = 0 WHERE valid = 1 AND id IN ("+
			strings.TrimSuffix(strings.Repeat("?, ", len(ids)), ", ")+")", params...)
		if err != nil {
			return err
		}
		changed, err := res.RowsAffected()
		n = int(changed)
		return err
	})
	return n, err
}

// --- skills ------------------------------------------------------------------------------------

func stringsOrEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// PutSkill upserts a VERIFIED skill.
func (s *SQLiteStore) PutSkill(ctx context.Context, skill contracts.Skill) error {
	if !skill.Verified {
		return contracts.UnverifiedSkill("store", skill.ID)
	}
	now := NowISO()
	return s.run(ctx, func(c *sql.Conn) error {
		_, err := c.ExecContext(ctx,
			"INSERT INTO skills (id, namespace, name, description, code, preconditions, "+
				"provenance, expires_at, verified, uses, created_at, updated_at) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "+
				"ON CONFLICT (id) DO UPDATE SET namespace = excluded.namespace, "+
				"name = excluded.name, description = excluded.description, "+
				"code = excluded.code, preconditions = excluded.preconditions, "+
				"provenance = excluded.provenance, expires_at = excluded.expires_at, "+
				"verified = excluded.verified, uses = excluded.uses, "+
				"updated_at = excluded.updated_at",
			skill.ID, skill.Namespace, skill.Name, skill.Description, skill.Code,
			PyDumps(stringsOrEmpty(skill.Preconditions)), skill.Provenance, nullable(skill.ExpiresAt),
			1, skill.Uses, now, now)
		return err
	})
}

// ListSkills is the verified skills in namespace plus global ones, newest first.
func (s *SQLiteStore) ListSkills(ctx context.Context, namespace string, limit int) ([]contracts.Skill, error) {
	if limit <= 0 {
		limit = 200
	}
	out := []contracts.Skill{}
	err := s.run(ctx, func(c *sql.Conn) error {
		rows, err := c.QueryContext(ctx,
			"SELECT id, name, description, code, preconditions, namespace, provenance, expires_at, "+
				"verified, uses FROM skills WHERE verified = 1 AND namespace IN (?, 'global') "+
				"ORDER BY updated_at DESC, id LIMIT ?", namespace, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sk contracts.Skill
			var pre string
			var expires sql.NullString
			var verified int
			if err := rows.Scan(&sk.ID, &sk.Name, &sk.Description, &sk.Code, &pre, &sk.Namespace,
				&sk.Provenance, &expires, &verified, &sk.Uses); err != nil {
				return err
			}
			if sk.Preconditions, err = decodeStringList(pre); err != nil {
				return err
			}
			sk.ExpiresAt, sk.Verified = expires.String, verified != 0
			out = append(out, sk)
		}
		return rows.Err()
	})
	return out, err
}

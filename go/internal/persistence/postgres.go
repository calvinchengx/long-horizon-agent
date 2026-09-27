package persistence

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// Postgres backend of Store (python: lha.persistence.postgres; needs `lha db migrate`).
//
// Uses the tables from db/migrations (0001-0005). The cost-ledger insert and its idempotency key
// are the SQLite backend's and Python's, so rows written by any of them are interchangeable.
// Opening verifies that the required migrations are applied.
//
// Semantic-memory VECTORS are written/queried by memory.PgSemanticIndex (pgvector) over this
// store's pool; this store writes rows without an embedding (lexical-only mode) and lists row
// text for BM25.

// ConnectTimeout bounds opening a Postgres connection.
const ConnectTimeout = 5 * time.Second

// InsertCostSQL is the ledger insert (Postgres placeholders).
const InsertCostSQL = "INSERT INTO cost_ledger " +
	"(mission_id, cycle_id, model, input_tokens, output_tokens, usd, role, cost_known, idempotency_key) " +
	"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT (idempotency_key) DO NOTHING"

var pgGateUpdate = map[string]string{
	"opened": "kind = EXCLUDED.kind, question = EXCLUDED.question, risk = EXCLUDED.risk, " +
		"default_action = EXCLUDED.default_action, options = EXCLUDED.options, " +
		"request = EXCLUDED.request, status = EXCLUDED.status, deadline = EXCLUDED.deadline, " +
		"decision = NULL, resolved_by = NULL, resolved_at = NULL, reminders = 0, " +
		"created_at = EXCLUDED.created_at, updated_at = now() " +
		"WHERE hitl_gates.created_at IS DISTINCT FROM EXCLUDED.created_at",
	"reminder": "status = EXCLUDED.status, " +
		"reminders = GREATEST(hitl_gates.reminders, EXCLUDED.reminders), updated_at = now() " +
		"WHERE hitl_gates.status IN ('OPEN', 'ESCALATED')",
	"closed": "status = EXCLUDED.status, decision = EXCLUDED.decision, " +
		"resolved_by = EXCLUDED.resolved_by, resolved_at = EXCLUDED.resolved_at, " +
		"updated_at = now() WHERE hitl_gates.status IN ('OPEN', 'ESCALATED')",
}

// PostgresStore is Store on Postgres (+ pgvector for dense memory via memory.PgSemanticIndex).
type PostgresStore struct {
	DSN  string
	Pool *pgxpool.Pool
}

var _ Store = (*PostgresStore)(nil)

// OpenPostgres connects and verifies the required migrations (a *StoreUnavailableError names
// the missing ones).
func OpenPostgres(ctx context.Context, dsn string) (*PostgresStore, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if cfg.ConnConfig.ConnectTimeout == 0 {
		cfg.ConnConfig.ConnectTimeout = ConnectTimeout
	}
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := checkMigrations(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &PostgresStore{DSN: dsn, Pool: pool}, nil
}

func checkMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('schema_migrations') IS NOT NULL").Scan(&exists); err != nil {
		return err
	}
	applied := map[string]bool{}
	if exists {
		rows, err := pool.Query(ctx, "SELECT version FROM schema_migrations")
		if err != nil {
			return err
		}
		versions, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, v := range versions {
			applied[v] = true
		}
	}
	missing := []string{}
	for _, v := range RequiredPGMigrations {
		if !applied[v] {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		return &StoreUnavailableError{Message: fmt.Sprintf(
			"postgres schema is missing migrations %s; run `lha db migrate`", pyList(missing))}
	}
	return nil
}

// pyList is Python's repr of a list of strings.
func pyList(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = contracts.PyRepr(s)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func isConnectError(err error) bool {
	var pgErr *pgconn.PgError
	var netErr net.Error
	var connErr *pgconn.ConnectError
	return errors.As(err, &connErr) || errors.As(err, &netErr) || (errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "08"))
}

// psycopgSQLStateNames are the psycopg.errors classes of the SQLSTATEs a store operation meets.
var psycopgSQLStateNames = map[string]string{
	"23505": "UniqueViolation",
	"23503": "ForeignKeyViolation",
	"23502": "NotNullViolation",
	"42P01": "UndefinedTable",
	"42703": "UndefinedColumn",
	"42883": "UndefinedFunction",
	"42501": "InsufficientPrivilege",
	"3D000": "InvalidCatalogName",
	"28P01": "InvalidPassword",
	"28000": "InvalidAuthorizationSpecification",
	"40001": "SerializationFailure",
	"40P01": "DeadlockDetected",
	"57014": "QueryCanceled",
}

// Postgres errors are named after psycopg's classes wherever an error message embeds a type name
// (memory_error, cost_hook_failed, ...), not only in the store's own messages.
func init() { pyfmt.RegisterExcNamer(psycopgErrorName) }

// psycopgErrorName is the psycopg exception class of a Postgres error ("" when err is not one):
// OperationalError for a failed connection, the SQLSTATE's class when listed, else the SQLSTATE
// class family's base.
func psycopgErrorName(err error) string {
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) {
		return "OperationalError"
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return ""
	}
	if name, ok := psycopgSQLStateNames[pgErr.Code]; ok {
		return name
	}
	switch {
	case strings.HasPrefix(pgErr.Code, "23"):
		return "IntegrityError"
	case strings.HasPrefix(pgErr.Code, "42"):
		return "ProgrammingError"
	case strings.HasPrefix(pgErr.Code, "22"):
		return "DataError"
	case strings.HasPrefix(pgErr.Code, "08"), strings.HasPrefix(pgErr.Code, "57"), strings.HasPrefix(pgErr.Code, "53"):
		return "OperationalError"
	}
	return "DatabaseError"
}

// Backend is "postgres".
func (s *PostgresStore) Backend() string { return BackendPostgres }

// DegradedReason is always "" (Postgres is never a fallback).
func (s *PostgresStore) DegradedReason() string { return "" }

// Close closes the pool.
func (s *PostgresStore) Close() error {
	if s.Pool != nil {
		s.Pool.Close()
	}
	return nil
}

// pyTS renders a timestamptz like Python's datetime.isoformat() (in UTC).
func pyTS(t *time.Time) string {
	if t == nil {
		return ""
	}
	return ISOAuto(*t)
}

// --- missions ----------------------------------------------------------------------------------

// UpsertMission inserts or updates (see Store).
func (s *PostgresStore) UpsertMission(ctx context.Context, m MissionUpsert) error {
	statusSQL := TerminalGuardSQL("missions.status", "EXCLUDED.status", "$7")
	_, err := s.Pool.Exec(ctx,
		"INSERT INTO missions (mission_id, title, description, status, head_sha, workflow_id) "+
			"VALUES ($1, $2, $3, $4, $5, $6) "+
			"ON CONFLICT (mission_id) DO UPDATE SET "+
			"title = COALESCE(NULLIF(EXCLUDED.title, ''), missions.title), "+
			"description = COALESCE(NULLIF(EXCLUDED.description, ''), missions.description), "+
			"status = "+statusSQL+", "+
			"head_sha = COALESCE(NULLIF(EXCLUDED.head_sha, ''), missions.head_sha), "+
			"workflow_id = COALESCE(EXCLUDED.workflow_id, missions.workflow_id), "+
			"updated_at = now()",
		m.MissionID, m.Title, m.Description, m.Status, nullable(m.HeadSHA), nullable(m.WorkflowID), m.Reopen)
	return err
}

const pgMissionCols = "mission_id, title, status, description, head_sha, workflow_id, created_at, updated_at"

func scanPGMission(row pgx.Row) (MissionRow, error) {
	var r MissionRow
	var desc, head, wf *string
	var created, updated *time.Time
	if err := row.Scan(&r.MissionID, &r.Title, &r.Status, &desc, &head, &wf, &created, &updated); err != nil {
		return r, err
	}
	r.Description, r.HeadSHA, r.WorkflowID = deref(desc), deref(head), deref(wf)
	r.CreatedAt, r.UpdatedAt = pyTS(created), pyTS(updated)
	return r, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// GetMission is the row, or nil.
func (s *PostgresStore) GetMission(ctx context.Context, missionID string) (*MissionRow, error) {
	r, err := scanPGMission(s.Pool.QueryRow(ctx, "SELECT "+pgMissionCols+" FROM missions WHERE mission_id = $1", missionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ListMissions is the most recently updated first.
func (s *PostgresStore) ListMissions(ctx context.Context, limit int) ([]MissionRow, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.Pool.Query(ctx, "SELECT "+pgMissionCols+" FROM missions ORDER BY updated_at DESC, mission_id LIMIT $1", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MissionRow{}
	for rows.Next() {
		r, err := scanPGMission(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- human gates (migration 0005) --------------------------------------------------------------

// RecordGateEvent applies one gate event (see Store).
func (s *PostgresStore) RecordGateEvent(ctx context.Context, event GateEvent) error {
	status, err := ValidateGateEvent(event)
	if err != nil {
		return err
	}
	kind, closing := gateUpdateKind(status)
	at, err := ParseISO(event.At)
	if err != nil {
		return err
	}
	var deadline any
	if event.Deadline != "" {
		d, err := ParseISO(event.Deadline)
		if err != nil {
			return err
		}
		deadline = d
	}
	risk := event.Risk
	if risk == "" {
		risk = event.Kind
	}
	var decision, resolvedBy, resolvedAt any
	if closing {
		decision, resolvedBy, resolvedAt = nullable(event.Decision), nullable(event.ResolvedBy), at
	}
	reminders := 0
	if status == GateEscalated {
		reminders = event.Step
	}
	_, err = s.Pool.Exec(ctx,
		"INSERT INTO hitl_gates (mission_id, gate_id, kind, question, risk, default_action, "+
			"options, request, status, deadline, decision, resolved_by, reminders, created_at, "+
			"resolved_at, updated_at) "+
			"VALUES ($1, $2, $3, $4, $5, $6, $7::text::jsonb, $8::text::jsonb, $9, $10::timestamptz, $11, $12, $13, "+
			"$14::timestamptz, $15::timestamptz, now()) ON CONFLICT (mission_id, gate_id) DO UPDATE SET "+pgGateUpdate[kind],
		event.MissionID, event.GateID, event.Kind, event.Question, risk, event.DefaultAction,
		PyDumps(optionsList(event.Options)), gateRequestJSON(event.Request), status, deadline,
		decision, resolvedBy, reminders, at, resolvedAt)
	return err
}

// ListGates lists gates, most recently opened first.
func (s *PostgresStore) ListGates(ctx context.Context, missionID string, limit int) ([]GateRow, error) {
	if limit <= 0 {
		limit = 50
	}
	where, params := "", []any{}
	if missionID != "" {
		where, params = "WHERE mission_id = $1 ", []any{missionID}
	}
	params = append(params, limit)
	rows, err := s.Pool.Query(ctx,
		"SELECT mission_id, gate_id, kind, status, question, options::text, default_action, risk, "+
			"deadline, decision, resolved_by, reminders, request::text, created_at, resolved_at, "+
			"updated_at FROM hitl_gates "+where+
			fmt.Sprintf("ORDER BY created_at DESC, mission_id, gate_id LIMIT $%d", len(params)), params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GateRow{}
	for rows.Next() {
		var r GateRow
		var options string
		var request *string
		var deadline, opened, resolved, updated *time.Time
		if err := rows.Scan(&r.MissionID, &r.GateID, &r.Kind, &r.Status, &r.Question, &options,
			&r.DefaultAction, &r.Risk, &deadline, &r.Decision, &r.ResolvedBy, &r.Reminders, &request,
			&opened, &resolved, &updated); err != nil {
			return nil, err
		}
		if r.Options, err = decodeStringList(options); err != nil {
			return nil, err
		}
		if request != nil {
			if r.Request, err = DecodeStringMap(*request); err != nil {
				return nil, err
			}
			if len(r.Request) == 0 {
				r.Request = nil // python: {..} if r[12] else None
			}
		}
		r.Deadline, r.OpenedAt, r.ResolvedAt, r.UpdatedAt = pyTS(deadline), pyTS(opened), pyTS(resolved), pyTS(updated)
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- cost ledger -------------------------------------------------------------------------------

// RecordCost inserts one ledger row (idempotent by key; usd NULL when unknown).
func (s *PostgresStore) RecordCost(ctx context.Context, missionID string, entry governor.CostEntry, callKey string) (bool, error) {
	var usd any
	if entry.CostKnown {
		usd = entry.USD
	}
	tag, err := s.Pool.Exec(ctx, InsertCostSQL, missionID, entry.CycleID, entry.Model, entry.InputTokens,
		entry.OutputTokens, usd, entry.Role, entry.CostKnown, CostIdempotencyKey(missionID, entry.CycleID, callKey))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ListCosts is oldest first (the most recent limit rows).
func (s *PostgresStore) ListCosts(ctx context.Context, missionID string, limit int) ([]CostRow, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.Pool.Query(ctx,
		"SELECT * FROM (SELECT id, mission_id, cycle_id, model, role, input_tokens, "+
			"output_tokens, usd::float8, cost_known, ts FROM cost_ledger WHERE mission_id = $1 "+
			"ORDER BY id DESC LIMIT $2) t ORDER BY id", missionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CostRow{}
	for rows.Next() {
		var r CostRow
		var id int64
		var ts *time.Time
		if err := rows.Scan(&id, &r.MissionID, &r.CycleID, &r.Model, &r.Role, &r.InputTokens,
			&r.OutputTokens, &r.USD, &r.CostKnown, &ts); err != nil {
			return nil, err
		}
		r.TS = pyTS(ts)
		out = append(out, r)
	}
	return out, rows.Err()
}

// CostSummary totals the mission's ledger.
func (s *PostgresStore) CostSummary(ctx context.Context, missionID string) (CostSummary, error) {
	sum := CostSummary{MissionID: missionID}
	var calls, unknown, in, out int64
	err := s.Pool.QueryRow(ctx,
		"SELECT COUNT(*), COALESCE(SUM(usd), 0)::float8, COUNT(*) FILTER (WHERE NOT cost_known), "+
			"COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0) "+
			"FROM cost_ledger WHERE mission_id = $1", missionID,
	).Scan(&calls, &sum.KnownUSD, &unknown, &in, &out)
	sum.Calls, sum.UnknownCostCalls, sum.InputTokens, sum.OutputTokens = int(calls), int(unknown), int(in), int(out)
	return sum, err
}

// --- episodic events ---------------------------------------------------------------------------

// AppendEvent appends one event; returns its id.
func (s *PostgresStore) AppendEvent(ctx context.Context, missionID, cycleID, kind string, payload map[string]any) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx,
		"INSERT INTO episodic_events (mission_id, cycle_id, kind, payload) "+
			"VALUES ($1, $2, $3, $4::text::jsonb) RETURNING id",
		missionID, cycleID, kind, PyDumps(payloadOrEmpty(payload))).Scan(&id)
	return id, err
}

// ListEvents is the newest q.Limit matching events with id > q.AfterID, oldest first.
func (s *PostgresStore) ListEvents(ctx context.Context, missionID string, q EventQuery) ([]EventRow, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}
	query := "SELECT id, mission_id, cycle_id, kind, payload::text, ts FROM episodic_events " +
		"WHERE mission_id = $1 AND id > $2"
	params := []any{missionID, q.AfterID}
	if len(q.Kinds) > 0 {
		params = append(params, q.Kinds)
		query += fmt.Sprintf(" AND kind = ANY($%d)", len(params))
	}
	params = append(params, limit)
	query = fmt.Sprintf("SELECT * FROM (%s ORDER BY id DESC LIMIT $%d) t ORDER BY id", query, len(params))
	rows, err := s.Pool.Query(ctx, query, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EventRow{}
	for rows.Next() {
		var r EventRow
		var payload string
		var ts *time.Time
		if err := rows.Scan(&r.ID, &r.MissionID, &r.CycleID, &r.Kind, &payload, &ts); err != nil {
			return nil, err
		}
		if r.Payload, err = decodeObject(payload); err != nil {
			return nil, err
		}
		r.TS = pyTS(ts)
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- semantic memory ---------------------------------------------------------------------------

// PutMemory upserts rows WITHOUT vectors (vectors go through memory.PgSemanticIndex).
func (s *PostgresStore) PutMemory(ctx context.Context, missionID string, records []contracts.MemoryRecord, emb *Embedding) error {
	if emb != nil {
		return errors.New("PostgresStore.put_memory stores text only; use PgSemanticIndex")
	}
	for _, record := range records {
		if _, err := s.Pool.Exec(ctx,
			"INSERT INTO semantic_memory (id, mission_id, kind, text, metadata, "+
				"embedding_model, embedding_version, valid) "+
				"VALUES ($1, $2, $3, $4, $5::text::jsonb, $6, $7, $8) "+
				"ON CONFLICT (id) DO UPDATE SET mission_id = EXCLUDED.mission_id, "+
				"kind = EXCLUDED.kind, text = EXCLUDED.text, metadata = EXCLUDED.metadata, "+
				"valid = EXCLUDED.valid",
			record.ID, missionID, record.Kind, record.Text, PyDumps(metadataOrEmpty(record.Metadata)),
			"none", "0", record.Valid); err != nil {
			return err
		}
	}
	return nil
}

// ListMemory is the mission's valid records, newest limit, oldest first.
func (s *PostgresStore) ListMemory(ctx context.Context, missionID string, limit int) ([]contracts.MemoryRecord, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.Pool.Query(ctx,
		"SELECT id, kind, text, metadata, embedding_model, embedding_version, valid FROM ("+
			"SELECT id, kind, text, metadata::text AS metadata, embedding_model, "+
			"embedding_version, valid, created_at FROM semantic_memory "+
			"WHERE mission_id = $1 AND valid ORDER BY created_at DESC, id LIMIT $2) t "+
			"ORDER BY created_at, id", missionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []contracts.MemoryRecord{}
	for rows.Next() {
		var r contracts.MemoryRecord
		var metadata string
		if err := rows.Scan(&r.ID, &r.Kind, &r.Text, &metadata, &r.EmbeddingModel, &r.EmbeddingVersion, &r.Valid); err != nil {
			return nil, err
		}
		if r.Metadata, err = DecodeStringMap(metadata); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InvalidateMemory soft-forgets ids; returns rows changed.
func (s *PostgresStore) InvalidateMemory(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tag, err := s.Pool.Exec(ctx, "UPDATE semantic_memory SET valid = false WHERE valid AND id = ANY($1)", ids)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// --- skills ------------------------------------------------------------------------------------

// PutSkill upserts a VERIFIED skill.
func (s *PostgresStore) PutSkill(ctx context.Context, skill contracts.Skill) error {
	if !skill.Verified {
		return contracts.UnverifiedSkill("store", skill.ID)
	}
	_, err := s.Pool.Exec(ctx,
		"INSERT INTO skills (id, namespace, name, description, code, preconditions, "+
			"provenance, expires_at, verified, uses) "+
			"VALUES ($1, $2, $3, $4, $5, $6::text::jsonb, $7, $8, true, $9) "+
			"ON CONFLICT (id) DO UPDATE SET namespace = EXCLUDED.namespace, "+
			"name = EXCLUDED.name, description = EXCLUDED.description, code = EXCLUDED.code, "+
			"preconditions = EXCLUDED.preconditions, provenance = EXCLUDED.provenance, "+
			"expires_at = EXCLUDED.expires_at, verified = EXCLUDED.verified, "+
			"uses = EXCLUDED.uses, updated_at = now()",
		skill.ID, skill.Namespace, skill.Name, skill.Description, skill.Code,
		PyDumps(stringsOrEmpty(skill.Preconditions)), skill.Provenance, nullable(skill.ExpiresAt), skill.Uses)
	return err
}

// ListSkills is the verified skills in namespace plus global ones, newest first.
func (s *PostgresStore) ListSkills(ctx context.Context, namespace string, limit int) ([]contracts.Skill, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.Pool.Query(ctx,
		"SELECT id, name, description, code, preconditions::text, namespace, provenance, "+
			"expires_at, verified, uses FROM skills "+
			"WHERE verified AND namespace IN ($1, 'global') "+
			"ORDER BY updated_at DESC, id LIMIT $2", namespace, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []contracts.Skill{}
	for rows.Next() {
		var sk contracts.Skill
		var pre string
		var expires *string
		if err := rows.Scan(&sk.ID, &sk.Name, &sk.Description, &sk.Code, &pre, &sk.Namespace,
			&sk.Provenance, &expires, &sk.Verified, &sk.Uses); err != nil {
			return nil, err
		}
		if sk.Preconditions, err = decodeStringList(pre); err != nil {
			return nil, err
		}
		sk.ExpiresAt = deref(expires)
		out = append(out, sk)
	}
	return out, rows.Err()
}

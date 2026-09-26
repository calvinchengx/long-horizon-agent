package persistence

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Postgres migrations (python: lha.persistence.db; `lha db migrate`).
//
// Each db/migrations/NNNN_*.sql file is ONE migration, identified by its file stem (e.g.
// "0001_init"). ApplyMigrations:
//
//   - takes a session-level advisory lock, so concurrent deployers/workers serialize;
//   - records applied versions in schema_migrations and skips them on later runs;
//   - executes each pending file as a single statement batch (the simple query protocol: the
//     whole script unsplit) inside its own transaction, inserting its schema_migrations row in
//     the same transaction: a failing migration leaves no partial schema and no record.
//
// The migration files also insert their own schema_migrations row, so a database initialized by
// Postgres' docker-entrypoint-initdb.d is recognized as already migrated. The bookkeeping is
// Python's, so either implementation's `lha db migrate` sees what the other applied.

// MigrationLockKey is the pg_advisory_lock key (ASCII "lhamigr8").
const MigrationLockKey int64 = 0x6C68616D69677238

const createMigrationsTable = "CREATE TABLE IF NOT EXISTS schema_migrations (" +
	"version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())"

// Migration is one *.sql file.
type Migration struct {
	Version string // file stem, e.g. "0001_init"
	Path    string
}

// SQL is the file's text.
func (m Migration) SQL() (string, error) {
	data, err := os.ReadFile(m.Path)
	return string(data), err
}

// DiscoverMigrations lists every *.sql file in dir, ordered by name.
func DiscoverMigrations(dir string) ([]Migration, error) {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("migrations directory not found: %s", dir)
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	out := make([]Migration, 0, len(paths))
	for _, p := range paths {
		out = append(out, Migration{Version: strings.TrimSuffix(filepath.Base(p), ".sql"), Path: p})
	}
	return out, nil
}

// ApplyPending applies the not-yet-applied migrations over an open connection; returns the
// versions applied by THIS call, in order.
func ApplyPending(ctx context.Context, conn *pgx.Conn, migrations []Migration) (applied []string, err error) {
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", MigrationLockKey); err != nil {
		return nil, err
	}
	defer func() {
		if _, uerr := conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", MigrationLockKey); uerr != nil && err == nil {
			err = uerr
		}
	}()
	if _, err := conn.Exec(ctx, createMigrationsTable); err != nil {
		return nil, err
	}
	rows, err := conn.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, err
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	done := map[string]bool{}
	for _, v := range versions {
		done[v] = true
	}
	newly := []string{}
	for _, m := range migrations {
		if done[m.Version] {
			continue
		}
		script, err := m.SQL()
		if err != nil {
			return newly, err
		}
		if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			// No arguments => the simple query protocol: the whole file runs as one batch.
			if _, err := tx.Exec(ctx, script); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT DO NOTHING", m.Version)
			return err
		}); err != nil {
			return newly, fmt.Errorf("migration %s: %w", m.Version, err)
		}
		newly = append(newly, m.Version)
	}
	return newly, nil
}

// ApplyMigrations applies every pending migration in dir to dsn; returns the versions newly
// applied by this call ([] when up to date).
func ApplyMigrations(ctx context.Context, dsn, dir string) ([]string, error) {
	migrations, err := DiscoverMigrations(dir)
	if err != nil {
		return nil, err
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	return ApplyPending(ctx, conn, migrations)
}

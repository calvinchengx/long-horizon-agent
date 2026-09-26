// Package pgtest gives integration tests a fresh Postgres database (python: the pg_dsn fixture of
// python/tests/integration/conftest.py).
//
// LHA_IT_POSTGRES_DSN is an admin DSN for a Postgres with the vector extension available (e.g.
// postgresql://lha:lha@127.0.0.1:55432/lha for pgvector/pgvector:pg16). Tests are skipped when
// it is unset, so `go test ./...` is always safe to run.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jackc/pgx/v5"
)

// EnvDSN is the variable that enables the Postgres integration tests.
const EnvDSN = "LHA_IT_POSTGRES_DSN"

// MigrationsDir is the repository's db/migrations.
func MigrationsDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "db", "migrations")
}

// FreshDB creates a uniquely named empty database, dropped when the test ends, and returns its
// DSN (the test is skipped without LHA_IT_POSTGRES_DSN).
func FreshDB(t *testing.T) string {
	t.Helper()
	admin := os.Getenv(EnvDSN)
	if admin == "" {
		t.Skip("set " + EnvDSN + " to run the Postgres integration tests")
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "lha_it_" + hex.EncodeToString(b[:])
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect %s: %v", EnvDSN, err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`)
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

//go:build postgres

package testdb

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const testDatabaseURL = "BLOBY_TEST_DATABASE_URL"

// Open returns an isolated database with the Bloby registry schema applied.
func Open(t testing.TB, schema string) (*pgxpool.Pool, context.Context) {
	t.Helper()

	baseURL := os.Getenv(testDatabaseURL)
	if baseURL == "" {
		t.Fatalf("%s is required for database tests", testDatabaseURL)
	}
	databaseURL := Create(t, baseURL)
	db, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Exec(t.Context(), schema); err != nil {
		t.Fatalf("apply Bloby schema: %v", err)
	}
	return db, t.Context()
}

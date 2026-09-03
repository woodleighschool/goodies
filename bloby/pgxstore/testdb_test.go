package pgxstore_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/woodleighschool/goodies/bloby/pgxstore"
)

const databaseOperationTimeout = 10 * time.Second

func openTestDatabase(t testing.TB) (*pgxpool.Pool, context.Context) {
	t.Helper()

	databaseURL := os.Getenv("BLOBY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("BLOBY_TEST_DATABASE_URL is not set")
	}
	db, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), databaseOperationTimeout)
		defer cleanupCancel()
		if _, err := db.Exec(cleanupCtx, `DROP TABLE IF EXISTS object_references, storage_objects CASCADE`); err != nil {
			t.Errorf("reset test database: %v", err)
		}
		db.Close()
	})
	if _, err := db.Exec(t.Context(), `DROP TABLE IF EXISTS object_references, storage_objects CASCADE`); err != nil {
		t.Fatalf("reset test database: %v", err)
	}
	if _, err := db.Exec(t.Context(), pgxstore.PostgresSchema()); err != nil {
		t.Fatalf("apply Bloby schema: %v", err)
	}
	return db, t.Context()
}

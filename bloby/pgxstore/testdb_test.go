package pgxstore_test

import (
	"context"
	"crypto/rand"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/woodleighschool/goodies/bloby/pgxstore"
)

const databaseOperationTimeout = 10 * time.Second

func openTestDatabase(t testing.TB) (*pgxpool.Pool, context.Context) {
	t.Helper()
	db, ctx := openEmptyTestDatabase(t)
	if err := pgxstore.Migrate(ctx, db); err != nil {
		t.Fatalf("apply Bloby migrations: %v", err)
	}
	return db, ctx
}

func openEmptyTestDatabase(t testing.TB) (*pgxpool.Pool, context.Context) {
	t.Helper()

	databaseURL := os.Getenv("BLOBY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("BLOBY_TEST_DATABASE_URL is not set")
	}
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	schema := pgx.Identifier{"bloby_test_" + rand.Text()}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), databaseOperationTimeout)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("reset test database: %v", err)
		}
		admin.Close()
	})
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db, t.Context()
}

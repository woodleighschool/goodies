package pgxstore_test

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/woodleighschool/goodies/bloby/pgxstore"
)

func TestMigrationsUpgradeAvailableObjects(t *testing.T) {
	db, ctx := openTestDatabaseAtVersion(t, 1)
	var availableID, pendingID int64
	if err := db.QueryRow(ctx, `INSERT INTO storage_objects
		(prefix, filename, content_type, size_bytes, sha256, available_at)
		VALUES ('documents', 'report.txt', 'text/plain', 7, $1, now()) RETURNING id`,
		strings.Repeat("a", 64)).Scan(&availableID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO storage_objects (prefix, filename)
		VALUES ('documents', 'pending.txt') RETURNING id`).Scan(&pendingID); err != nil {
		t.Fatal(err)
	}
	sqlDB := stdlib.OpenDBFromPool(db)
	defer func() { _ = sqlDB.Close() }()
	migrations, err := goose.NewProvider(goose.DialectPostgres, sqlDB, pgxstore.Migrations(),
		goose.WithTableName("bloby_migrations"), goose.WithLogger(goose.NopLogger()))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := migrations.UpTo(ctx, pgxstore.Version); err != nil {
			t.Fatalf("upgrade Bloby schema: %v", err)
		}
	}
	registry := pgxstore.New(db)
	available, err := registry.GetByID(ctx, availableID)
	if err != nil {
		t.Fatal(err)
	}
	if available.Key() != "documents/1/report.txt" || available.SizeBytesValue() != 7 {
		t.Fatalf("existing representation changed: %+v", available)
	}
	pending, err := registry.GetByID(ctx, pendingID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.StorageKey != nil || pending.Available() {
		t.Fatalf("pending upload was published: %+v", pending)
	}
}

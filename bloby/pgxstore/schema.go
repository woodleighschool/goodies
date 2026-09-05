package pgxstore

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

const migrationLockID int64 = 0x626c6f6279 // "bloby"

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrate applies the schema required by this version of Bloby. Applications
// call it before applying migrations that reference storage objects. Concurrent
// calls are serialized, and the pool remains open when Migrate returns.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("bloby: open migrations: %w", err)
	}
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockID(migrationLockID))
	if err != nil {
		return fmt.Errorf("bloby: create migration lock: %w", err)
	}
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()
	provider, err := goose.NewProvider(
		goose.DialectPostgres, db, migrations,
		goose.WithTableName("bloby_migrations"),
		goose.WithLogger(goose.NopLogger()),
		goose.WithSessionLocker(locker),
	)
	if err != nil {
		return fmt.Errorf("bloby: create migration provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("bloby: apply migrations: %w", err)
	}
	return nil
}

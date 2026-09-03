package pgxstore

import (
	"embed"
	"io/fs"
)

// Version is the latest migration supplied by this package.
const Version int64 = 2

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrations returns ordered Goose SQL migrations for Bloby-owned tables.
// Applications choose when and up to which version to apply them, using a
// separate Goose version table for this migration history.
func Migrations() fs.FS {
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		panic(err)
	}
	return migrations
}

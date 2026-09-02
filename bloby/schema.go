package bloby

import _ "embed"

//go:embed schema.sql
var postgresSchema string

// PostgresSchema returns the PostgreSQL schema required by ObjectStore.
// Applications remain responsible for applying it through their own migration history.
func PostgresSchema() string {
	return postgresSchema
}

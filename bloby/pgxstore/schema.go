package pgxstore

import _ "embed"

//go:embed schema.sql
var postgresSchema string

// PostgresSchema returns the schema required by Store. Applications remain
// responsible for applying it through their own migration history.
func PostgresSchema() string {
	return postgresSchema
}

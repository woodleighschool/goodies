// Package pgxstore implements Bloby's object registry with pgx and PostgreSQL.
package pgxstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/woodleighschool/goodies/bloby"
)

const (
	objectColumnsSQL = `id, prefix, filename, content_type, size_bytes, sha256, available_at, multipart_upload_id, storage_key, created_at, updated_at`
	objectSelectSQL  = `SELECT ` + objectColumnsSQL + ` FROM storage_objects`
)

// Store is a PostgreSQL implementation of bloby.Registry.
type Store struct {
	pool *pgxpool.Pool
}

var _ bloby.Registry = (*Store)(nil)

// New returns a PostgreSQL object registry.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) CreatePending(ctx context.Context, prefix, filename string) (*bloby.Object, error) {
	const sql = `INSERT INTO storage_objects (prefix, filename) VALUES (@prefix, @filename) RETURNING ` + objectColumnsSQL
	object, err := s.getObject(ctx, sql, pgx.NamedArgs{"prefix": prefix, "filename": filename})
	if err != nil {
		return nil, mutationError(err)
	}
	return &object, nil
}

func (s *Store) MarkAvailable(ctx context.Context, id, sizeBytes int64, contentType, sha256sum, storageKey string) (*bloby.Object, error) {
	const sql = `UPDATE storage_objects
SET size_bytes = @size_bytes, sha256 = @sha256, content_type = @content_type, storage_key = @storage_key,
    available_at = now(), updated_at = now()
WHERE id = @id AND available_at IS NULL AND expired_at IS NULL
RETURNING ` + objectColumnsSQL
	object, err := s.getObject(ctx, sql, pgx.NamedArgs{
		"id": id, "size_bytes": &sizeBytes, "sha256": &sha256sum, "content_type": contentType, "storage_key": storageKey,
	})
	if errors.Is(err, bloby.ErrNotFound) {
		current, getErr := s.GetByID(ctx, id)
		if getErr != nil {
			return nil, getErr
		}
		if current.Available() {
			return current, nil
		}
	}
	if err != nil {
		return nil, mutationError(err)
	}
	return &object, nil
}

func (s *Store) RefreshPending(ctx context.Context, id int64) (*bloby.Object, error) {
	const sql = `UPDATE storage_objects SET updated_at = now()
WHERE id = $1 AND available_at IS NULL AND expired_at IS NULL RETURNING ` + objectColumnsSQL
	object, err := s.getObject(ctx, sql, id)
	if err != nil {
		return nil, mutationError(err)
	}
	return &object, nil
}

func (s *Store) RecordMultipartUploadID(ctx context.Context, id int64, uploadID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE storage_objects
SET multipart_upload_id = $2, updated_at = now()
WHERE id = $1 AND available_at IS NULL AND expired_at IS NULL AND multipart_upload_id IS NULL`, id, uploadID)
	if err != nil {
		return mutationError(err)
	}
	if tag.RowsAffected() == 0 {
		return bloby.ErrNotFound
	}
	return nil
}

func (s *Store) ClearMultipartUploadID(ctx context.Context, id int64, uploadID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE storage_objects
SET multipart_upload_id = NULL, updated_at = now()
WHERE id = $1 AND expired_at IS NULL AND multipart_upload_id = $2`, id, uploadID)
	if err != nil {
		return mutationError(err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	object, err := s.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if object.MultipartUploadID == nil {
		return nil
	}
	return fmt.Errorf("%w: multipart upload ID changed", bloby.ErrConflict)
}

func (s *Store) GetByID(ctx context.Context, id int64) (*bloby.Object, error) {
	object, err := s.getObject(ctx, objectSelectSQL+" WHERE id = $1 AND expired_at IS NULL", id)
	if err != nil {
		return nil, getError(err)
	}
	return &object, nil
}

func (s *Store) ListByIDs(ctx context.Context, ids []int64) (map[int64]bloby.Object, error) {
	rows, err := s.pool.Query(ctx, objectSelectSQL+" WHERE id = ANY($1::bigint[]) AND expired_at IS NULL", ids)
	if err != nil {
		return nil, err
	}
	objects, err := pgx.CollectRows(rows, pgx.RowToStructByPos[bloby.Object])
	if err != nil {
		return nil, err
	}
	result := make(map[int64]bloby.Object, len(objects))
	for _, object := range objects {
		result[object.ID] = object
	}
	return result, nil
}

func (s *Store) ListByPrefix(ctx context.Context, prefix string, options bloby.ListOptions) ([]bloby.Object, int, error) {
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*)::integer FROM storage_objects
WHERE prefix = $1 AND available_at IS NOT NULL AND expired_at IS NULL`, prefix).Scan(&count); err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, objectSelectSQL+`
WHERE prefix = $1 AND available_at IS NOT NULL AND expired_at IS NULL
ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3`, prefix, options.Limit, options.Offset)
	if err != nil {
		return nil, 0, err
	}
	objects, err := pgx.CollectRows(rows, pgx.RowToStructByPos[bloby.Object])
	return objects, count, err
}

func (s *Store) Delete(ctx context.Context, id int64) (*bloby.Object, error) {
	const sql = `DELETE FROM storage_objects WHERE id = $1 AND expired_at IS NULL RETURNING ` + objectColumnsSQL
	object, err := s.getObject(ctx, sql, id)
	if err != nil {
		return nil, deleteConflict(err, "storage object is still referenced")
	}
	return &object, nil
}

func (s *Store) ClaimExpiredPending(ctx context.Context, updatedBefore, retryBefore time.Time, limit int) ([]bloby.Object, error) {
	rows, err := s.pool.Query(ctx, `WITH candidates AS (
    SELECT id FROM storage_objects
    WHERE available_at IS NULL AND (expired_at < $2 OR (expired_at IS NULL AND updated_at < $1))
    ORDER BY (expired_at IS NULL), COALESCE(expired_at, updated_at), id
    LIMIT $3 FOR UPDATE SKIP LOCKED
)
UPDATE storage_objects AS objects SET expired_at = now()
FROM candidates WHERE objects.id = candidates.id AND objects.available_at IS NULL
RETURNING objects.id, objects.prefix, objects.filename, objects.content_type,
          objects.size_bytes, objects.sha256, objects.available_at,
          objects.multipart_upload_id, objects.storage_key, objects.created_at, objects.updated_at`, updatedBefore, retryBefore, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[bloby.Object])
}

func (s *Store) DeleteExpiredPending(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM storage_objects WHERE id = $1 AND expired_at IS NOT NULL`, id)
	if err != nil {
		return deleteConflict(err, "expired storage upload is still referenced")
	}
	if tag.RowsAffected() == 0 {
		return bloby.ErrNotFound
	}
	return nil
}

func (s *Store) getObject(ctx context.Context, sql string, args ...any) (bloby.Object, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return bloby.Object{}, err
	}
	object, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[bloby.Object])
	return object, getError(err)
}

func sqlState(err error) string {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok {
		return ""
	}
	return pgErr.Code
}

func getError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return bloby.ErrNotFound
	}
	return err
}

func mutationError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return bloby.ErrNotFound
	}
	switch sqlState(err) {
	case pgerrcode.ForeignKeyViolation:
		return bloby.ErrNotFound
	case pgerrcode.UniqueViolation:
		return bloby.ErrAlreadyExists
	case pgerrcode.InvalidTextRepresentation, pgerrcode.NotNullViolation, pgerrcode.CheckViolation:
		return fmt.Errorf("%w: %w", bloby.ErrInvalidInput, err)
	}
	return err
}

func deleteConflict(err error, message string) error {
	switch sqlState(err) {
	case pgerrcode.ForeignKeyViolation, pgerrcode.RestrictViolation:
		return fmt.Errorf("%w: %s", bloby.ErrConflict, message)
	}
	return mutationError(err)
}

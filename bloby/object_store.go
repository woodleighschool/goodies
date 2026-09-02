package bloby

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Object is a row in the storage registry: one stored (or pending) blob. The
// byte key is derived, never stored, so the path format lives in one place.
type Object struct {
	ID                int64      `db:"id"`
	Prefix            string     `db:"prefix"`
	Filename          string     `db:"filename"`
	ContentType       string     `db:"content_type"`
	SizeBytes         *int64     `db:"size_bytes"`
	SHA256            *string    `db:"sha256"`
	AvailableAt       *time.Time `db:"available_at"`
	MultipartUploadID *string    `db:"multipart_upload_id"`
	CreatedAt         time.Time  `db:"created_at"`
	UpdatedAt         time.Time  `db:"updated_at"`
}

const (
	objectColumnsSQL = `id, prefix, filename, content_type, size_bytes, sha256, available_at, multipart_upload_id, created_at, updated_at`
	objectSelectSQL  = `SELECT ` + objectColumnsSQL + `
FROM storage_objects`
)

// Key builds a storage key from its parts: <prefix>/<id>/<filename>. This is the
// one place the key format lives.
func Key(prefix string, id int64, filename string) string {
	return fmt.Sprintf("%s/%d/%s", prefix, id, filename)
}

// Key is the object's storage key.
func (o Object) Key() string {
	return Key(o.Prefix, o.ID, o.Filename)
}

// Available reports whether the bytes have been finalized.
func (o Object) Available() bool {
	return o.AvailableAt != nil
}

// SHA256Value returns the recorded hash, or "" while the object is pending.
func (o Object) SHA256Value() string {
	if o.SHA256 == nil {
		return ""
	}
	return *o.SHA256
}

// SizeBytesValue returns the recorded byte length, or 0 while the object is pending.
func (o Object) SizeBytesValue() int64 {
	if o.SizeBytes == nil {
		return 0
	}
	return *o.SizeBytes
}

// SizeKBValue returns the rounded-up recorded size in KiB.
func (o Object) SizeKBValue() int64 {
	sizeBytes := o.SizeBytesValue()
	if sizeBytes <= 0 {
		return 0
	}
	return (sizeBytes + 1023) / 1024
}

// ObjectStore is the database registry of stored objects.
type ObjectStore struct {
	pool    *pgxpool.Pool
	backend objectBackend
	logger  *slog.Logger
}

type objectBackend interface {
	Delete(ctx context.Context, key string) error
}

// NewObjectStore returns a registry backed by PostgreSQL.
func NewObjectStore(pool *pgxpool.Pool, backend objectBackend, logger *slog.Logger) *ObjectStore {
	return &ObjectStore{pool: pool, backend: backend, logger: logger}
}

// CreatePending reserves an object in the registry without classifying content.
func (s *ObjectStore) CreatePending(ctx context.Context, prefix, filename string) (*Object, error) {
	if !prefixPattern.MatchString(prefix) {
		return nil, fmt.Errorf("%w: invalid storage prefix %q", ErrInvalidInput, prefix)
	}
	filename = normalizeUploadFilename(filename)
	if err := validateUploadFilename(filename); err != nil {
		return nil, err
	}
	const sql = `INSERT INTO storage_objects (prefix, filename)
VALUES (@prefix, @filename)
RETURNING ` + objectColumnsSQL
	obj, err := getOne[Object](ctx, s.pool, sql, pgx.NamedArgs{
		"prefix":   prefix,
		"filename": filename,
	})
	if err != nil {
		return nil, mutationError(err)
	}
	return &obj, nil
}

// MarkAvailable records application-derived representation metadata for an object.
func (s *ObjectStore) MarkAvailable(
	ctx context.Context,
	id int64,
	sizeBytes int64,
	contentType string,
	sha256sum string,
) (*Object, error) {
	contentType, err := normalizeContentType(contentType)
	if err != nil {
		return nil, err
	}
	if err := validateAvailableObjectMetadata(sizeBytes, sha256sum); err != nil {
		return nil, err
	}
	const sql = `UPDATE storage_objects
SET size_bytes = @size_bytes,
    sha256 = @sha256,
    content_type = @content_type,
    available_at = now(),
    updated_at = now()
WHERE id = @id
  AND expired_at IS NULL
RETURNING ` + objectColumnsSQL
	obj, err := getOne[Object](ctx, s.pool, sql, pgx.NamedArgs{
		"id":           id,
		"size_bytes":   &sizeBytes,
		"sha256":       &sha256sum,
		"content_type": contentType,
	})
	if err != nil {
		return nil, mutationError(err)
	}
	return &obj, nil
}

// RefreshPending keeps an active upload outside the abandoned-upload window.
func (s *ObjectStore) RefreshPending(ctx context.Context, id int64) (*Object, error) {
	const sql = `UPDATE storage_objects
SET updated_at = now()
WHERE id = $1
  AND available_at IS NULL
  AND expired_at IS NULL
RETURNING ` + objectColumnsSQL
	object, err := getOne[Object](ctx, s.pool, sql, id)
	if err != nil {
		return nil, mutationError(err)
	}
	return &object, nil
}

// RecordMultipartUploadID records the provider upload ID, or returns the ID
// already recorded by a concurrent or retried creation request.
func (s *ObjectStore) RecordMultipartUploadID(
	ctx context.Context,
	id int64,
	uploadID string,
) (string, bool, error) {
	var err error
	uploadID, err = normalizeMultipartUploadID(uploadID)
	if err != nil {
		return "", false, err
	}
	var recorded string
	err = s.pool.QueryRow(ctx, `
UPDATE storage_objects
SET multipart_upload_id = $2,
    updated_at = now()
WHERE id = $1
  AND available_at IS NULL
  AND expired_at IS NULL
  AND multipart_upload_id IS NULL
RETURNING multipart_upload_id`, id, uploadID).Scan(&recorded)
	if err == nil {
		return recorded, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, mutationError(err)
	}
	object, err := s.GetByID(ctx, id)
	if err != nil {
		return "", false, err
	}
	if object.MultipartUploadID != nil {
		return *object.MultipartUploadID, false, nil
	}
	return "", false, fmt.Errorf("%w: storage object is already finalized", ErrInvalidInput)
}

// ClearMultipartUploadID closes the recorded provider upload after assembly.
func (s *ObjectStore) ClearMultipartUploadID(ctx context.Context, id int64, uploadID string) error {
	tag, err := s.pool.Exec(ctx, `
UPDATE storage_objects
SET multipart_upload_id = NULL,
    updated_at = now()
WHERE id = $1
  AND expired_at IS NULL
  AND multipart_upload_id = $2`, id, uploadID)
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
	return fmt.Errorf("%w: multipart upload ID changed", ErrConflict)
}

// GetByID returns one object.
func (s *ObjectStore) GetByID(ctx context.Context, id int64) (*Object, error) {
	obj, err := getOne[Object](ctx, s.pool, objectSelectSQL+"\nWHERE id = $1 AND expired_at IS NULL", id)
	if err != nil {
		return nil, getError(err)
	}
	return &obj, nil
}

// ETag returns the SHA-256 entity tag for an available object.
func (o Object) ETag() string {
	if o.SHA256 == nil || *o.SHA256 == "" {
		return ""
	}
	return `"` + *o.SHA256 + `"`
}

// ListByIDs returns objects keyed by id. Missing IDs are ignored.
func (s *ObjectStore) ListByIDs(ctx context.Context, ids []int64) (map[int64]Object, error) {
	rows, err := s.pool.Query(ctx,
		objectSelectSQL+"\nWHERE id = ANY($1::bigint[]) AND expired_at IS NULL", ids)
	if err != nil {
		return nil, err
	}
	objects, err := pgx.CollectRows(rows, pgx.RowToStructByName[Object])
	if err != nil {
		return nil, err
	}
	result := make(map[int64]Object, len(objects))
	for _, obj := range objects {
		result[obj.ID] = obj
	}
	return result, nil
}

// ListOptions controls registry pagination.
type ListOptions struct {
	Limit  int
	Offset int
}

// ListByPrefix returns available objects under a prefix, newest first.
func (s *ObjectStore) ListByPrefix(
	ctx context.Context,
	prefix string,
	options ListOptions,
) ([]Object, int, error) {
	if options.Limit == 0 {
		options.Limit = 50
	}
	if options.Limit < 1 || options.Limit > 1000 || options.Offset < 0 {
		return nil, 0, fmt.Errorf("%w: invalid list options", ErrInvalidInput)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*)::integer
FROM storage_objects
WHERE prefix = $1 AND available_at IS NOT NULL AND expired_at IS NULL`, prefix).Scan(&count); err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, objectSelectSQL+`
WHERE prefix = $1 AND available_at IS NOT NULL AND expired_at IS NULL
ORDER BY created_at DESC, id DESC
LIMIT $2 OFFSET $3`, prefix, options.Limit, options.Offset)
	if err != nil {
		return nil, 0, err
	}
	objects, err := pgx.CollectRows(rows, pgx.RowToStructByName[Object])
	return objects, count, err
}

// Delete removes one object from the registry and then best-effort removes its bytes.
func (s *ObjectStore) Delete(ctx context.Context, id int64) error {
	object, err := s.deleteRegistryObject(ctx, id)
	if err != nil {
		return err
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), objectCleanupTimeout)
	defer cancel()
	s.deleteBytes(cleanupCtx, object)
	return nil
}

// DeleteUnreferenced best-effort removes objects after their owning mutation commits.
func (s *ObjectStore) DeleteUnreferenced(ctx context.Context, ids ...int64) {
	if len(ids) == 0 {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), objectCleanupTimeout)
	defer cancel()
	for _, id := range ids {
		object, err := s.deleteRegistryObject(cleanupCtx, id)
		switch {
		case err == nil:
			s.deleteBytes(cleanupCtx, object)
		case errors.Is(err, ErrNotFound), errors.Is(err, ErrConflict):
		default:
			s.logger.WarnContext(cleanupCtx, "storage object cleanup failed", "object_id", id, "err", err)
		}
	}
}

func (s *ObjectStore) deleteRegistryObject(ctx context.Context, id int64) (*Object, error) {
	const sql = `DELETE FROM storage_objects
WHERE id = $1
  AND expired_at IS NULL
RETURNING ` + objectColumnsSQL
	object, err := getOne[Object](ctx, s.pool, sql, id)
	if err != nil {
		return nil, deleteConflict(err, "storage object is still referenced")
	}
	return &object, nil
}

func (s *ObjectStore) deleteBytes(ctx context.Context, object *Object) {
	if s.backend == nil {
		return
	}
	if err := s.backend.Delete(ctx, object.Key()); err != nil {
		s.logger.WarnContext(
			ctx,
			"storage object bytes could not be removed",
			"object_id", object.ID,
			"key", object.Key(),
			"err", err,
		)
	}
}

func normalizeContentType(value string) (string, error) {
	mediaType, params, err := mime.ParseMediaType(value)
	if err != nil {
		return "", fmt.Errorf("%w: invalid content type: %w", ErrInvalidInput, err)
	}
	value = mime.FormatMediaType(mediaType, params)
	if value == "" {
		return "", fmt.Errorf("%w: invalid content type", ErrInvalidInput)
	}
	return value, nil
}

func normalizeMultipartUploadID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%w: multipart upload ID is blank", ErrInvalidInput)
	}
	return value, nil
}

func validateAvailableObjectMetadata(sizeBytes int64, sha256sum string) error {
	if sizeBytes < 0 || strings.TrimSpace(sha256sum) == "" {
		return fmt.Errorf("%w: incomplete storage object metadata", ErrInvalidInput)
	}
	return nil
}

// prefixPattern constrains a storage prefix to the lowercase slash-separated
// segments that make up a key namespace.
var prefixPattern = regexp.MustCompile(`^[a-z0-9]+(/[a-z0-9]+)*$`)

func normalizeUploadFilename(name string) string {
	name = strings.ReplaceAll(name, `\`, `/`)
	name = path.Base(name)
	return strings.TrimSpace(name)
}

func validateUploadFilename(name string) error {
	if name == "" || name == "." || name == ".." || name == "/" || !utf8.ValidString(name) {
		return fmt.Errorf("%w: invalid upload filename", ErrInvalidInput)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: invalid upload filename", ErrInvalidInput)
		}
	}
	return nil
}

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
)

// Object is one stored or pending blob. Its byte key is derived from its
// prefix, ID and filename.
type Object struct {
	ID                int64
	Prefix            string
	Filename          string
	ContentType       string
	SizeBytes         *int64
	SHA256            *string
	AvailableAt       *time.Time
	MultipartUploadID *string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// ListOptions controls registry pagination.
type ListOptions struct {
	Limit  int
	Offset int
}

// Registry persists Bloby's object lifecycle. Implementations must preserve
// atomic state transitions and return Bloby's sentinel errors.
type Registry interface {
	CreatePending(ctx context.Context, prefix, filename string) (*Object, error)
	MarkAvailable(ctx context.Context, id, sizeBytes int64, contentType, sha256 string) (*Object, error)
	RefreshPending(ctx context.Context, id int64) (*Object, error)
	RecordMultipartUploadID(ctx context.Context, id int64, uploadID string) (recorded string, created bool, err error)
	ClearMultipartUploadID(ctx context.Context, id int64, uploadID string) error
	GetByID(ctx context.Context, id int64) (*Object, error)
	ListByIDs(ctx context.Context, ids []int64) (map[int64]Object, error)
	ListByPrefix(ctx context.Context, prefix string, options ListOptions) ([]Object, int, error)
	Delete(ctx context.Context, id int64) (*Object, error)
	ClaimExpiredPending(ctx context.Context, updatedBefore, retryBefore time.Time, limit int) ([]Object, error)
	DeleteExpiredPending(ctx context.Context, id int64) error
}

// Key builds a storage key from its parts: <prefix>/<id>/<filename>.
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

// ETag returns the SHA-256 entity tag for an available object.
func (o Object) ETag() string {
	if o.SHA256 == nil || *o.SHA256 == "" {
		return ""
	}
	return `"` + *o.SHA256 + `"`
}

// ObjectStore applies Bloby's model around a persistence registry.
type ObjectStore struct {
	registry Registry
	backend  ObjectDeleter
	logger   *slog.Logger
}

// ObjectDeleter removes stored bytes after a registry object is deleted.
type ObjectDeleter interface {
	Delete(ctx context.Context, key string) error
}

// NewObjectStore returns an object store composed from a registry and byte backend.
func NewObjectStore(registry Registry, backend ObjectDeleter, logger *slog.Logger) *ObjectStore {
	return &ObjectStore{registry: registry, backend: backend, logger: logger}
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
	return s.registry.CreatePending(ctx, prefix, filename)
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
	return s.registry.MarkAvailable(ctx, id, sizeBytes, contentType, sha256sum)
}

// RefreshPending keeps an active upload outside the abandoned-upload window.
func (s *ObjectStore) RefreshPending(ctx context.Context, id int64) (*Object, error) {
	return s.registry.RefreshPending(ctx, id)
}

// RecordMultipartUploadID records a provider upload ID.
func (s *ObjectStore) RecordMultipartUploadID(ctx context.Context, id int64, uploadID string) (string, bool, error) {
	uploadID, err := normalizeMultipartUploadID(uploadID)
	if err != nil {
		return "", false, err
	}
	return s.registry.RecordMultipartUploadID(ctx, id, uploadID)
}

// ClearMultipartUploadID closes a recorded provider upload after assembly.
func (s *ObjectStore) ClearMultipartUploadID(ctx context.Context, id int64, uploadID string) error {
	return s.registry.ClearMultipartUploadID(ctx, id, uploadID)
}

// GetByID returns one object.
func (s *ObjectStore) GetByID(ctx context.Context, id int64) (*Object, error) {
	return s.registry.GetByID(ctx, id)
}

// ListByIDs returns objects keyed by ID. Missing IDs are ignored.
func (s *ObjectStore) ListByIDs(ctx context.Context, ids []int64) (map[int64]Object, error) {
	return s.registry.ListByIDs(ctx, ids)
}

// ListByPrefix returns available objects under a prefix, newest first.
func (s *ObjectStore) ListByPrefix(ctx context.Context, prefix string, options ListOptions) ([]Object, int, error) {
	if options.Limit == 0 {
		options.Limit = 50
	}
	if options.Limit < 1 || options.Limit > 1000 || options.Offset < 0 {
		return nil, 0, fmt.Errorf("%w: invalid list options", ErrInvalidInput)
	}
	return s.registry.ListByPrefix(ctx, prefix, options)
}

// Delete removes one object from the registry and then best-effort removes its bytes.
func (s *ObjectStore) Delete(ctx context.Context, id int64) error {
	object, err := s.registry.Delete(ctx, id)
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
		object, err := s.registry.Delete(cleanupCtx, id)
		switch {
		case err == nil:
			s.deleteBytes(cleanupCtx, object)
		case errors.Is(err, ErrNotFound), errors.Is(err, ErrConflict):
		default:
			s.logger.WarnContext(cleanupCtx, "storage object cleanup failed", "object_id", id, "err", err)
		}
	}
}

func (s *ObjectStore) deleteBytes(ctx context.Context, object *Object) {
	if s.backend == nil {
		return
	}
	if err := s.backend.Delete(ctx, object.Key()); err != nil {
		s.logger.WarnContext(ctx, "storage object bytes could not be removed", "object_id", object.ID, "key", object.Key(), "err", err)
	}
}

func (s *ObjectStore) claimExpiredPending(
	ctx context.Context,
	updatedBefore time.Time,
	retryBefore time.Time,
	limit int,
) ([]Object, error) {
	return s.registry.ClaimExpiredPending(ctx, updatedBefore, retryBefore, limit)
}

func (s *ObjectStore) deleteExpiredPending(ctx context.Context, id int64) error {
	return s.registry.DeleteExpiredPending(ctx, id)
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

var prefixPattern = regexp.MustCompile(`^[a-z0-9]+(/[a-z0-9]+)*$`)

func normalizeUploadFilename(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
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

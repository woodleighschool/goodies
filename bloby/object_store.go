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

// Object is one pending or available blob. SizeBytes, SHA256 and CRC64NVME are
// the uploader's declaration while pending and what storage holds once
// available. Objects published before declarations carry no CRC64NVME.
type Object struct {
	ID                int64
	Prefix            string
	Filename          string
	ContentType       string
	SizeBytes         *int64
	SHA256            *string
	CRC64NVME         *string
	AvailableAt       *time.Time
	MultipartUploadID *string
	StorageKey        *string
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
	CreatePending(ctx context.Context, prefix, filename string, content Content) (*Object, error)
	// MarkAvailable publishes a pending object once. Retries return the original
	// available object unchanged; expired and missing objects return ErrNotFound.
	MarkAvailable(ctx context.Context, id int64, contentType, storageKey string) (*Object, error)
	// RefreshPending only touches active pending rows; all others return ErrNotFound.
	RefreshPending(ctx context.Context, id int64) (*Object, error)
	RecordMultipartUploadID(ctx context.Context, id int64, uploadID string) error
	ClearMultipartUploadID(ctx context.Context, id int64, uploadID string) error
	GetByID(ctx context.Context, id int64) (*Object, error)
	ListByIDs(ctx context.Context, ids []int64) (map[int64]Object, error)
	ListByPrefix(ctx context.Context, prefix string, options ListOptions) ([]Object, int, error)
	Delete(ctx context.Context, id int64) (*Object, error)
	ClaimExpiredPending(ctx context.Context, updatedBefore, retryBefore time.Time, limit int) ([]Object, error)
	DeleteExpiredPending(ctx context.Context, id int64) error
	// ListUnreferenced returns available objects under the prefixes, finalized
	// before the cutoff, that no foreign key references.
	ListUnreferenced(ctx context.Context, prefixes []string, availableBefore time.Time, limit int) ([]Object, error)
}

// Key returns the immutable stored key, or an empty string while pending.
func (o Object) Key() string {
	if o.StorageKey == nil {
		return ""
	}
	return *o.StorageKey
}

// key returns where the object's bytes live. An object keeps one key for life,
// so its upload capabilities can only address the bytes it declared.
func (o Object) key() string {
	if o.StorageKey != nil {
		return *o.StorageKey
	}
	return fmt.Sprintf("%s%d/%s", objectPrefix, o.ID, o.Filename)
}

// content returns the declaration recorded when the upload began.
func (o Object) content() (Content, error) {
	if o.SizeBytes == nil || o.SHA256 == nil || o.CRC64NVME == nil {
		return Content{}, fmt.Errorf("%w: object has no declared content", ErrInvalidInput)
	}
	return Content{SizeBytes: *o.SizeBytes, SHA256: *o.SHA256, CRC64NVME: *o.CRC64NVME}, nil
}

// Available reports whether the bytes have been finalized.
func (o Object) Available() bool {
	return o.AvailableAt != nil
}

// SHA256Value returns the recorded hash, or "" when none is recorded.
func (o Object) SHA256Value() string {
	if o.SHA256 == nil {
		return ""
	}
	return *o.SHA256
}

// SizeBytesValue returns the recorded byte length, or 0 when none is recorded.
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

// ETag returns the SHA-256 entity tag.
func (o Object) ETag() string {
	if o.SHA256 == nil || *o.SHA256 == "" {
		return ""
	}
	return `"` + *o.SHA256 + `"`
}

// Service owns object ingestion, delivery, deletion, and abandoned-upload cleanup.
// Applications authorize access to objects; only Service publishes their bytes.
type Service struct {
	registry           Registry
	backend            backend
	logger             *slog.Logger
	transferTTL        time.Duration
	referencedPrefixes []string
}

// objectPrefix holds every object's bytes, each under its registry ID.
const objectPrefix = "_objects/"

// createPending reserves an object for bytes matching content.
func (s *Service) createPending(ctx context.Context, prefix, filename string, content Content) (*Object, error) {
	if !prefixPattern.MatchString(prefix) {
		return nil, fmt.Errorf("%w: invalid storage prefix %q", ErrInvalidInput, prefix)
	}
	filename = normalizeUploadFilename(filename)
	if err := validateUploadFilename(filename); err != nil {
		return nil, err
	}
	if err := content.validate(); err != nil {
		return nil, err
	}
	return s.registry.CreatePending(ctx, prefix, filename, content)
}

// markAvailable publishes an object whose stored bytes match its declaration.
func (s *Service) markAvailable(ctx context.Context, object *Object, contentType string) (*Object, error) {
	contentType, err := normalizeContentType(contentType)
	if err != nil {
		return nil, err
	}
	return s.registry.MarkAvailable(ctx, object.ID, contentType, object.key())
}

// GetByID returns one object.
func (s *Service) GetByID(ctx context.Context, id int64) (*Object, error) {
	return s.registry.GetByID(ctx, id)
}

// ListByIDs returns objects keyed by ID. Missing IDs are ignored.
func (s *Service) ListByIDs(ctx context.Context, ids []int64) (map[int64]Object, error) {
	return s.registry.ListByIDs(ctx, ids)
}

// ListByPrefix returns available objects under a prefix, newest first.
func (s *Service) ListByPrefix(ctx context.Context, prefix string, options ListOptions) ([]Object, int, error) {
	if options.Limit == 0 {
		options.Limit = 50
	}
	if options.Limit < 1 || options.Limit > 1000 || options.Offset < 0 {
		return nil, 0, fmt.Errorf("%w: invalid list options", ErrInvalidInput)
	}
	return s.registry.ListByPrefix(ctx, prefix, options)
}

// DeleteUnreferenced best-effort removes objects after their owning mutation commits.
func (s *Service) DeleteUnreferenced(ctx context.Context, ids ...int64) {
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

func (s *Service) deleteBytes(ctx context.Context, object *Object) {
	if err := s.removeBytes(ctx, object); err != nil {
		s.logger.WarnContext(ctx, "storage object bytes could not be removed", "object_id", object.ID, "err", err)
	}
}

func (s *Service) removeBytes(ctx context.Context, object *Object) error {
	var abortErr error
	if object.MultipartUploadID != nil {
		if backend, ok := s.backend.(multipartBackend); ok {
			abortErr = backend.AbortMultipartUpload(ctx, object.key(), *object.MultipartUploadID)
			if errors.Is(abortErr, ErrMultipartUploadNotFound) {
				abortErr = nil
			}
		}
	}
	return errors.Join(abortErr, s.backend.Delete(ctx, object.key()))
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

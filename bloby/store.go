package bloby

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"
)

// ErrObjectNotFound reports that a backend has no object for a key.
var ErrObjectNotFound = errors.New("storage object not found")

// ErrMultipartUploadNotFound reports that a provider no longer has an upload ID.
var ErrMultipartUploadNotFound = errors.New("storage multipart upload not found")

// backend is a configured storage backend. All runtime backends can read/write
// bytes and mint direct transfer URLs.
type backend interface {
	Open(ctx context.Context, key string) (io.ReadCloser, objectInfo, error)
	Put(ctx context.Context, key string, r io.Reader, opts putOptions) error
	Delete(ctx context.Context, key string) error
	PresignGet(ctx context.Context, key string, ttl time.Duration, opts getOptions) (string, error)
	PresignPut(ctx context.Context, key string, ttl time.Duration) (UploadTarget, error)
	TransferOrigin() string
	beginUpload(ctx context.Context, key string, sizeBytes int64) (UploadAction, error)
	seal(ctx context.Context, stagingKey, key string) error
	cleanupStaging(ctx context.Context, before time.Time) error
	expiredCandidates(ctx context.Context, before time.Time) ([]string, error)
}

// multipartBackend is the multipart transfer contract implemented by S3 storage.
type multipartBackend interface {
	CreateMultipartUpload(ctx context.Context, key string) (string, error)
	PresignMultipartPart(
		ctx context.Context,
		key, uploadID string,
		partNumber int32,
		ttl time.Duration,
	) (UploadTarget, error)
	CompleteMultipartUpload(ctx context.Context, key, uploadID string, parts []CompletedPart) error
	AbortMultipartUpload(ctx context.Context, key, uploadID string) error
}

// objectInfo is backend metadata for stored bytes.
type objectInfo struct {
	Size int64
}

// putOptions carries representation metadata to preserve with stored bytes.
type putOptions struct {
	ContentType string
}

// getOptions carries optional hints for a presigned read.
type getOptions struct {
	ContentType  string
	CacheControl string
}

// UploadTarget identifies where and how to put an object's bytes.
type UploadTarget struct {
	URL     string            `json:"url"`
	Method  string            `json:"method" enum:"PUT"`
	Headers map[string]string `json:"headers,omitempty"`
}

// CompletedPart identifies one uploaded S3 multipart part.
type CompletedPart struct {
	PartNumber int32  `json:"part_number" minimum:"1" maximum:"10000"`
	ETag       string `json:"etag" minLength:"1"`
}

func transferOrigin(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid storage transfer URL %q", rawURL)
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

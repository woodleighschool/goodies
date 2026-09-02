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

// BlobStore reads and writes blobs by key. Backends: file, s3.
type BlobStore interface {
	Open(ctx context.Context, key string) (ObjectReader, ObjectInfo, error)
	Put(ctx context.Context, key string, r io.Reader, opts PutOptions) error
	Delete(ctx context.Context, key string) error
}

// ObjectReader is a backend object stream that supports HTTP range reads.
type ObjectReader interface {
	io.Reader
	io.Seeker
	io.Closer
}

// Backend is a configured storage backend. All runtime backends can read/write
// bytes and mint direct transfer URLs.
type Backend interface {
	BlobStore
	Presigner
	PresignPut(ctx context.Context, key string, ttl time.Duration) (UploadTarget, error)
	TransferOrigin() string
	deliveryMode() deliveryMode
	beginUpload(ctx context.Context, key string, sizeBytes int64) (UploadAction, error)
}

// MultipartBackend is the multipart transfer contract implemented by S3 storage.
type MultipartBackend interface {
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

// Presigner mints direct read URLs.
type Presigner interface {
	PresignGet(ctx context.Context, key string, ttl time.Duration, opts GetOptions) (string, error)
}

// ObjectInfo is backend metadata for stored bytes.
type ObjectInfo struct {
	Size int64
}

// PutOptions carries representation metadata to preserve with stored bytes.
type PutOptions struct {
	ContentType string
}

// GetOptions carries optional hints for a presigned read.
type GetOptions struct {
	ContentType  string
	CacheControl string
}

// UploadTarget identifies where and how to put an object's bytes.
type UploadTarget struct {
	URL     string
	Method  string
	Headers map[string]string
}

// CompletedPart identifies one uploaded S3 multipart part.
type CompletedPart struct {
	PartNumber int32
	ETag       string
}

func transferOrigin(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid storage transfer URL %q", rawURL)
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

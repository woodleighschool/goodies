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

// backend is a configured storage backend. Every write names the content it
// carries, and the backend refuses bytes that differ from it.
type backend interface {
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Put(ctx context.Context, key string, r io.Reader, content Content, opts putOptions) error
	Delete(ctx context.Context, key string) error
	PresignGet(ctx context.Context, key string, ttl time.Duration, opts getOptions) (string, error)
	PresignPut(ctx context.Context, key string, content Content, ttl time.Duration) (UploadTarget, error)
	TransferOrigin() string
	beginUpload(ctx context.Context, key string, content Content) (UploadAction, error)
	// verify compares the backend's own record of the stored bytes with content.
	verify(ctx context.Context, key string, content Content) error
	// head returns up to n leading bytes.
	head(ctx context.Context, key string, n int64) ([]byte, error)
	// expiredObjects lists the object keys last written before the cutoff.
	expiredObjects(ctx context.Context, before time.Time) ([]string, error)
}

// multipartBackend is the multipart transfer contract implemented by S3 storage.
type multipartBackend interface {
	CreateMultipartUpload(ctx context.Context, key string) (string, error)
	PresignMultipartPart(
		ctx context.Context,
		key, uploadID string,
		partNumber int32,
		crc64nvme string,
		ttl time.Duration,
	) (UploadTarget, error)
	// CompleteMultipartUpload assembles the parts the provider holds, which must
	// total sizeBytes.
	CompleteMultipartUpload(ctx context.Context, key, uploadID string, sizeBytes int64) error
	AbortMultipartUpload(ctx context.Context, key, uploadID string) error
	// expiredUploads lists the uploads initiated before the cutoff.
	expiredUploads(ctx context.Context, before time.Time) ([]multipartUpload, error)
}

// multipartUpload identifies one incomplete upload held by the provider.
type multipartUpload struct {
	Key string
	ID  string
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

// UploadTarget identifies where and how to put an object's bytes. A client
// sends Headers unchanged; the target accepts only the bytes it was issued for.
type UploadTarget struct {
	URL     string            `json:"url"`
	Method  string            `json:"method" enum:"PUT"`
	Headers map[string]string `json:"headers,omitempty"`
}

func transferOrigin(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid storage transfer URL %q", rawURL)
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

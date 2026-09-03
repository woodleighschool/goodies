package bloby

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

type s3Store struct {
	bucket         string
	transferOrigin string
	ttl            time.Duration
	client         *s3.Client
	presigner      *s3.PresignClient
}

var _ multipartBackend = (*s3Store)(nil)

const s3MultipartThreshold = 100 * 1024 * 1024

func (s *s3Store) beginUpload(ctx context.Context, key string, sizeBytes int64) (UploadAction, error) {
	if sizeBytes > s3MultipartThreshold {
		return UploadAction{Strategy: StrategyMultipart}, nil
	}
	target, err := s.PresignPut(ctx, key, 0)
	if err != nil {
		return UploadAction{}, err
	}
	return UploadAction{Strategy: StrategyDirectPut, Target: &target}, nil
}

func newS3Store(ctx context.Context, cfg S3Config, transferTTL time.Duration) (*s3Store, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(
		ctx,
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.AccessKey,
			cfg.SecretKey,
			"",
		)),
	)
	if err != nil {
		return nil, fmt.Errorf("load storage s3 config: %w", err)
	}
	client := newS3Client(awsCfg, cfg.Endpoint, cfg.PathStyle)
	// Presigned reads are redirects, so their signatures cannot require request headers.
	presigner := s3.NewPresignClient(client, s3.WithPresignClientFromClientOptions(func(options *s3.Options) {
		options.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	}))
	origin, err := presignedTransferOrigin(ctx, presigner, cfg.Bucket)
	if err != nil {
		return nil, err
	}
	return &s3Store{
		bucket:         cfg.Bucket,
		transferOrigin: origin,
		ttl:            transferTTL,
		client:         client,
		presigner:      presigner,
	}, nil
}

func newS3Client(cfg aws.Config, endpoint string, pathStyle bool) *s3.Client {
	return s3.NewFromConfig(cfg, func(options *s3.Options) {
		options.UsePathStyle = pathStyle
		if endpoint != "" {
			options.BaseEndpoint = aws.String(endpoint)
		}
	})
}

func presignedTransferOrigin(
	ctx context.Context,
	presigner *s3.PresignClient,
	bucket string,
) (string, error) {
	output, err := presigner.PresignUploadPart(
		ctx,
		&s3.UploadPartInput{
			Bucket:     aws.String(bucket),
			Key:        aws.String("bloby-transfer-origin"),
			UploadId:   aws.String("bloby-transfer-origin"),
			PartNumber: aws.Int32(1),
		},
		func(options *s3.PresignOptions) {
			options.Expires = time.Minute
		},
	)
	if err != nil {
		return "", fmt.Errorf("resolve storage transfer origin: %w", err)
	}
	origin, err := transferOrigin(output.URL)
	if err != nil {
		return "", fmt.Errorf("resolve storage transfer origin: %w", err)
	}
	return origin, nil
}

func (s *s3Store) TransferOrigin() string {
	return s.transferOrigin
}

func (s *s3Store) Open(ctx context.Context, key string) (io.ReadSeekCloser, objectInfo, error) {
	output, err := s.getObject(ctx, key, 0)
	if err != nil {
		return nil, objectInfo{}, err
	}
	size := aws.ToInt64(output.ContentLength)
	reader := &s3ObjectReader{
		body:   output.Body,
		ctx:    ctx,
		key:    key,
		size:   size,
		openAt: s.openObjectAt,
	}
	return reader, objectInfo{Size: size}, nil
}

func (s *s3Store) getObject(ctx context.Context, key string, offset int64) (*s3.GetObjectOutput, error) {
	input := &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}
	if offset > 0 {
		input.Range = aws.String(fmt.Sprintf("bytes=%d-", offset))
	}
	output, err := s.client.GetObject(ctx, input)
	if s3NotFound(err) {
		return nil, ErrObjectNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get %q: %w", key, err)
	}
	return output, nil
}

func (s *s3Store) openObjectAt(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	output, err := s.getObject(ctx, key, offset)
	if err != nil {
		return nil, err
	}
	return output.Body, nil
}

type s3ObjectReader struct {
	body   io.ReadCloser
	ctx    context.Context
	key    string
	size   int64
	offset int64
	closed bool
	openAt func(context.Context, string, int64) (io.ReadCloser, error)
}

var errObjectReaderClosed = errors.New("storage object reader is closed")

func (r *s3ObjectReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, errObjectReaderClosed
	}
	if r.offset >= r.size {
		return 0, io.EOF
	}
	if r.body == nil {
		body, err := r.openAt(r.ctx, r.key, r.offset)
		if err != nil {
			return 0, err
		}
		r.body = body
	}
	n, err := r.body.Read(p)
	r.offset += int64(n)
	return n, err
}

func (r *s3ObjectReader) Seek(offset int64, whence int) (int64, error) {
	if r.closed {
		return 0, errObjectReaderClosed
	}
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = r.offset + offset
	case io.SeekEnd:
		next = r.size + offset
	default:
		return 0, fmt.Errorf("invalid seek whence %d", whence)
	}
	if next < 0 {
		return 0, errors.New("negative storage object seek")
	}
	if next == r.offset {
		return next, nil
	}
	if r.body != nil {
		if err := r.body.Close(); err != nil {
			return 0, err
		}
		r.body = nil
	}
	r.offset = next
	return next, nil
}

func (r *s3ObjectReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	if r.body != nil {
		return r.body.Close()
	}
	return nil
}

// Put buffers the body to make it seekable for signing. The presigned upload
// path is the norm for large objects; server-side Put is for modest writes.
func (s *s3Store) Put(ctx context.Context, key string, r io.Reader, opts putOptions) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read body for %q: %w", key, err)
	}
	input := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(body),
	}
	if opts.ContentType != "" {
		input.ContentType = aws.String(opts.ContentType)
	}
	if _, err := s.client.PutObject(ctx, input); err != nil {
		return fmt.Errorf("put %q: %w", key, err)
	}
	return nil
}

func (s *s3Store) Delete(ctx context.Context, key string) error {
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("delete %q: %w", key, err)
	}
	return nil
}

func (*s3Store) deliveryMode() deliveryMode {
	return deliveryRedirect
}

func (s *s3Store) PresignGet(
	ctx context.Context,
	key string,
	ttl time.Duration,
	opts getOptions,
) (string, error) {
	input := &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}
	if opts.ContentType != "" {
		input.ResponseContentType = aws.String(opts.ContentType)
	}
	if opts.CacheControl != "" {
		input.ResponseCacheControl = aws.String(opts.CacheControl)
	}
	output, err := s.presigner.PresignGetObject(ctx, input, s.expires(ttl))
	if err != nil {
		return "", fmt.Errorf("presign get %q: %w", key, err)
	}
	return output.URL, nil
}

func (s *s3Store) PresignPut(
	ctx context.Context,
	key string,
	ttl time.Duration,
) (UploadTarget, error) {
	input := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}
	output, err := s.presigner.PresignPutObject(ctx, input, s.expires(ttl))
	if err != nil {
		return UploadTarget{}, fmt.Errorf("presign put %q: %w", key, err)
	}
	return UploadTarget{
		URL:     output.URL,
		Method:  http.MethodPut,
		Headers: singleValueHeaders(output.SignedHeader),
	}, nil
}

func (s *s3Store) CreateMultipartUpload(ctx context.Context, key string) (string, error) {
	output, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return "", fmt.Errorf("create multipart upload for %q: %w", key, err)
	}
	uploadID := aws.ToString(output.UploadId)
	if uploadID == "" {
		return "", fmt.Errorf("create multipart upload for %q: provider returned an empty upload ID", key)
	}
	return uploadID, nil
}

func (s *s3Store) PresignMultipartPart(
	ctx context.Context,
	key string,
	uploadID string,
	partNumber int32,
	ttl time.Duration,
) (UploadTarget, error) {
	output, err := s.presigner.PresignUploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String(s.bucket),
		Key:        aws.String(key),
		UploadId:   aws.String(uploadID),
		PartNumber: aws.Int32(partNumber),
	}, s.expires(ttl))
	if err != nil {
		return UploadTarget{}, fmt.Errorf("presign multipart part %d for %q: %w", partNumber, key, err)
	}
	return UploadTarget{
		URL:     output.URL,
		Method:  http.MethodPut,
		Headers: singleValueHeaders(output.SignedHeader),
	}, nil
}

func (s *s3Store) CompleteMultipartUpload(
	ctx context.Context,
	key string,
	uploadID string,
	parts []CompletedPart,
) error {
	completed := make([]types.CompletedPart, len(parts))
	for i, part := range parts {
		completed[i] = types.CompletedPart{
			ETag:       aws.String(part.ETag),
			PartNumber: aws.Int32(part.PartNumber),
		}
	}
	_, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: completed,
		},
	})
	if s3NoSuchUpload(err) {
		return ErrMultipartUploadNotFound
	}
	if err != nil {
		return fmt.Errorf("complete multipart upload for %q: %w", key, err)
	}
	return nil
}

func (s *s3Store) AbortMultipartUpload(ctx context.Context, key string, uploadID string) error {
	_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	})
	if s3NoSuchUpload(err) {
		return ErrMultipartUploadNotFound
	}
	if err != nil {
		return fmt.Errorf("abort multipart upload for %q: %w", key, err)
	}
	return nil
}

func (s *s3Store) expires(ttl time.Duration) func(*s3.PresignOptions) {
	ttl = ttlOrDefault(ttl, s.ttl)
	return func(options *s3.PresignOptions) {
		options.Expires = ttl
	}
}

func s3NotFound(err error) bool {
	apiErr, ok := errors.AsType[smithy.APIError](err)
	if !ok {
		return false
	}
	switch apiErr.ErrorCode() {
	case "NotFound", "NoSuchKey", "404":
		return true
	default:
		return false
	}
}

func s3NoSuchUpload(err error) bool {
	apiErr, ok := errors.AsType[smithy.APIError](err)
	return ok && apiErr.ErrorCode() == "NoSuchUpload"
}

func singleValueHeaders(headers http.Header) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	out := make(map[string]string, len(headers))
	for key, values := range headers {
		if key == "Host" {
			continue
		}
		if len(values) > 0 {
			out[key] = values[0]
		}
	}
	return out
}

const (
	s3CopyPartSize    int64 = 5 * 1024 * 1024 * 1024
	s3MinimumCopySize int64 = 5 * 1024 * 1024
)

// seal copies a pinned staging representation to an attempt-specific key.
// The registry selects one candidate only after its immutable bytes are inspected.
func (s *s3Store) seal(ctx context.Context, stagingKey, key string) error {
	source, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(stagingKey)})
	if s3NotFound(err) {
		return ErrObjectNotFound
	}
	if err != nil {
		return fmt.Errorf("head staging object %q: %w", stagingKey, err)
	}
	if aws.ToString(source.ETag) == "" {
		return fmt.Errorf("seal %q: source ETag is missing", key)
	}
	if aws.ToInt64(source.ContentLength) < s3MinimumCopySize {
		return s.sealSmall(ctx, stagingKey, key, source)
	}
	uploadID, err := s.CreateMultipartUpload(ctx, key)
	if err != nil {
		return err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), objectCleanupTimeout)
		defer cancel()
		_ = s.AbortMultipartUpload(cleanupCtx, key, uploadID)
	}()

	copySource := escapePath(s.bucket + "/" + stagingKey)
	if version := aws.ToString(source.VersionId); version != "" {
		copySource += "?versionId=" + url.QueryEscape(version)
	}
	size := aws.ToInt64(source.ContentLength)
	parts := make([]types.CompletedPart, 0, (size+s3CopyPartSize-1)/s3CopyPartSize)
	for offset := int64(0); offset < size; offset += s3CopyPartSize {
		partNumber := int32(len(parts) + 1) //nolint:gosec // S3 object and part limits keep this below 10001.
		input := &s3.UploadPartCopyInput{
			Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: aws.String(uploadID), PartNumber: aws.Int32(partNumber),
			CopySource: aws.String(copySource), CopySourceIfMatch: source.ETag,
		}
		// S3 permits byte-range copy only for sources larger than 5 MiB.
		// A single part copies the whole object without a range.
		if size > s3CopyPartSize {
			input.CopySourceRange = aws.String(fmt.Sprintf("bytes=%d-%d", offset, min(offset+s3CopyPartSize, size)-1))
		}
		output, err := s.client.UploadPartCopy(ctx, input)
		if err != nil {
			return fmt.Errorf("copy part %d: %w", partNumber, err)
		}
		if output.CopyPartResult == nil || aws.ToString(output.CopyPartResult.ETag) == "" {
			return fmt.Errorf("seal %q part %d: provider returned no ETag", key, partNumber)
		}
		parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(partNumber), ETag: output.CopyPartResult.ETag})
	}
	_, err = s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	return err
}

// S3-compatible providers may reject multipart copies below the minimum part
// size. A bounded read pins their source ETag before writing a unique candidate.
func (s *s3Store) sealSmall(ctx context.Context, stagingKey, key string, source *s3.HeadObjectOutput) error {
	output, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(stagingKey), IfMatch: source.ETag, VersionId: source.VersionId,
	})
	if err != nil {
		return err
	}
	body, readErr := io.ReadAll(io.LimitReader(output.Body, s3MinimumCopySize))
	closeErr := output.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return fmt.Errorf("read staging object %q: %w", stagingKey, err)
	}
	if int64(len(body)) != aws.ToInt64(source.ContentLength) {
		return fmt.Errorf("seal %q: staging size changed", key)
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), Body: bytes.NewReader(body),
	})
	return err
}

func (s *s3Store) cleanupStaging(ctx context.Context, before time.Time) error {
	objects := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket), Prefix: aws.String(stagingPrefix),
	})
	for objects.HasMorePages() {
		page, err := objects.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list staging objects: %w", err)
		}
		for _, object := range page.Contents {
			if object.LastModified != nil && object.LastModified.Before(before) {
				if err := s.Delete(ctx, aws.ToString(object.Key)); err != nil {
					return err
				}
			}
		}
	}
	// A process can die while copying sealed bytes. Expire those server-owned
	// multipart uploads as well as abandoned client multipart uploads.
	uploads := s3.NewListMultipartUploadsPaginator(s.client, &s3.ListMultipartUploadsInput{Bucket: aws.String(s.bucket)})
	for uploads.HasMorePages() {
		page, err := uploads.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list incomplete uploads: %w", err)
		}
		for _, upload := range page.Uploads {
			if upload.Initiated != nil && upload.Initiated.Before(before) {
				if err := s.AbortMultipartUpload(ctx, aws.ToString(upload.Key), aws.ToString(upload.UploadId)); err != nil && !errors.Is(err, ErrMultipartUploadNotFound) {
					return err
				}
			}
		}
	}
	return nil
}

func (s *s3Store) expiredCandidates(ctx context.Context, before time.Time) ([]string, error) {
	objects := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(candidatePrefix)})
	var keys []string
	for objects.HasMorePages() {
		page, err := objects.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, object := range page.Contents {
			if object.LastModified != nil && object.LastModified.Before(before) {
				keys = append(keys, aws.ToString(object.Key))
			}
		}
	}
	return keys, nil
}

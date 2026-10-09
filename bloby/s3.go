package bloby

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
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

func (s *s3Store) beginUpload(ctx context.Context, key string, content Content) (UploadAction, error) {
	if content.SizeBytes > s3MultipartThreshold {
		return UploadAction{Strategy: StrategyMultipart}, nil
	}
	target, err := s.PresignPut(ctx, key, content, 0)
	if err != nil {
		return UploadAction{}, err
	}
	return UploadAction{Strategy: StrategyDirectPut, Target: &target}, nil
}

func newS3Store(ctx context.Context, cfg S3Config, transferTTL time.Duration) (*s3Store, error) {
	// Bloby names the checksum on every write and compares checksums itself, so
	// the SDK adds none of its own and validates no responses. Fixing both here
	// keeps ambient AWS configuration from changing signed requests.
	awsCfg, err := awsconfig.LoadDefaultConfig(
		ctx,
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.AccessKey,
			cfg.SecretKey,
			"",
		)),
		awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		awsconfig.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	)
	if err != nil {
		return nil, fmt.Errorf("load storage s3 config: %w", err)
	}
	client := newS3Client(awsCfg, cfg.Endpoint, cfg.PathStyle)
	presigner := s3.NewPresignClient(client)
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

func (s *s3Store) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	output, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if s3NotFound(err) {
		return nil, ErrObjectNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get %q: %w", key, err)
	}
	return output.Body, nil
}

// Put buffers the body to make it seekable for signing. The presigned upload
// path is the norm for large objects; server-side Put is for modest writes.
func (s *s3Store) Put(ctx context.Context, key string, r io.Reader, content Content, opts putOptions) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read body for %q: %w", key, err)
	}
	input := &s3.PutObjectInput{
		Bucket:         aws.String(s.bucket),
		Key:            aws.String(key),
		Body:           bytes.NewReader(body),
		ChecksumSHA256: aws.String(base64Digest(content.SHA256)),
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

// PresignPut binds the declared SHA-256 and length into the signature. The
// provider rejects any other body, so the URL can only ever store those bytes.
func (s *s3Store) PresignPut(
	ctx context.Context,
	key string,
	content Content,
	ttl time.Duration,
) (UploadTarget, error) {
	input := &s3.PutObjectInput{
		Bucket:         aws.String(s.bucket),
		Key:            aws.String(key),
		ChecksumSHA256: aws.String(base64Digest(content.SHA256)),
		ContentLength:  aws.Int64(content.SizeBytes),
	}
	output, err := s.presigner.PresignPutObject(ctx, input, s.expires(ttl))
	if err != nil {
		return UploadTarget{}, fmt.Errorf("presign put %q: %w", key, err)
	}
	return UploadTarget{
		URL:     output.URL,
		Method:  http.MethodPut,
		Headers: uploadHeaders(output.SignedHeader),
	}, nil
}

// CreateMultipartUpload asks the provider for a full-object CRC64NVME, the one
// multipart checksum that covers the assembled object rather than its parts.
func (s *s3Store) CreateMultipartUpload(ctx context.Context, key string) (string, error) {
	output, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:            aws.String(s.bucket),
		Key:               aws.String(key),
		ChecksumAlgorithm: types.ChecksumAlgorithmCrc64nvme,
		ChecksumType:      types.ChecksumTypeFullObject,
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
	crc64nvme string,
	ttl time.Duration,
) (UploadTarget, error) {
	output, err := s.presigner.PresignUploadPart(ctx, &s3.UploadPartInput{
		Bucket:            aws.String(s.bucket),
		Key:               aws.String(key),
		UploadId:          aws.String(uploadID),
		PartNumber:        aws.Int32(partNumber),
		ChecksumCRC64NVME: aws.String(base64Digest(crc64nvme)),
	}, s.expires(ttl))
	if err != nil {
		return UploadTarget{}, fmt.Errorf("presign multipart part %d for %q: %w", partNumber, key, err)
	}
	return UploadTarget{
		URL:     output.URL,
		Method:  http.MethodPut,
		Headers: uploadHeaders(output.SignedHeader),
	}, nil
}

// CompleteMultipartUpload assembles the parts the provider lists. Providers
// differ on whether completion checks a declared size or checksum, so the
// size is checked here and the checksum by verify.
func (s *s3Store) CompleteMultipartUpload(ctx context.Context, key, uploadID string, sizeBytes int64) error {
	var (
		parts []types.CompletedPart
		total int64
	)
	pages := s3.NewListPartsPaginator(s.client, &s3.ListPartsInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if s3NoSuchUpload(err) {
			return ErrMultipartUploadNotFound
		}
		if err != nil {
			return fmt.Errorf("list multipart parts for %q: %w", key, err)
		}
		for _, part := range page.Parts {
			total += aws.ToInt64(part.Size)
			parts = append(parts, types.CompletedPart{
				PartNumber:        part.PartNumber,
				ETag:              part.ETag,
				ChecksumCRC64NVME: part.ChecksumCRC64NVME,
			})
		}
	}
	if len(parts) == 0 {
		return fmt.Errorf("%w: upload has not arrived: %w", ErrInvalidInput, ErrObjectNotFound)
	}
	if total != sizeBytes {
		return contentMismatch("uploaded parts total %d bytes, declared %d", total, sizeBytes)
	}
	_, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: parts,
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

// verify compares the checksum the provider computed for the object with the
// declaration: SHA-256 for a single PUT, CRC64NVME for an assembled upload. A
// provider that reports neither cannot vouch for the bytes.
func (s *s3Store) verify(ctx context.Context, key string, content Content) error {
	object, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket:       aws.String(s.bucket),
		Key:          aws.String(key),
		ChecksumMode: types.ChecksumModeEnabled,
	})
	if s3NotFound(err) {
		return ErrObjectNotFound
	}
	if err != nil {
		return fmt.Errorf("head %q: %w", key, err)
	}
	if size := aws.ToInt64(object.ContentLength); size != content.SizeBytes {
		return contentMismatch("storage holds %d bytes, declared %d", size, content.SizeBytes)
	}
	verified := false
	if object.ChecksumType != types.ChecksumTypeComposite {
		for _, checksum := range []struct{ name, stored, declared string }{
			{"SHA-256", aws.ToString(object.ChecksumSHA256), base64Digest(content.SHA256)},
			{"CRC64NVME", aws.ToString(object.ChecksumCRC64NVME), base64Digest(content.CRC64NVME)},
		} {
			if checksum.stored == "" {
				continue
			}
			if checksum.stored != checksum.declared {
				return contentMismatch("storage does not hold the declared %s", checksum.name)
			}
			verified = true
		}
	}
	if !verified {
		return fmt.Errorf("verify %q: storage reported no full-object SHA-256 or CRC64NVME", key)
	}
	return nil
}

func (s *s3Store) head(ctx context.Context, key string, n int64) ([]byte, error) {
	output, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Range:  aws.String(fmt.Sprintf("bytes=0-%d", n-1)),
	})
	if s3NotFound(err) {
		return nil, ErrObjectNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get head of %q: %w", key, err)
	}
	head, readErr := io.ReadAll(io.LimitReader(output.Body, n))
	if err := errors.Join(readErr, output.Body.Close()); err != nil {
		return nil, fmt.Errorf("read head of %q: %w", key, err)
	}
	return head, nil
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

// uploadHeaders returns the signed headers an uploader must send. HTTP clients
// derive Host and Content-Length from the request themselves.
func uploadHeaders(headers http.Header) map[string]string {
	out := make(map[string]string, len(headers))
	for key, values := range headers {
		if key == "Host" || key == "Content-Length" || len(values) == 0 {
			continue
		}
		out[key] = values[0]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *s3Store) expiredObjects(ctx context.Context, before time.Time) ([]string, error) {
	objects := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(objectPrefix)})
	var keys []string
	for objects.HasMorePages() {
		page, err := objects.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list objects: %w", err)
		}
		for _, object := range page.Contents {
			if object.LastModified != nil && object.LastModified.Before(before) {
				keys = append(keys, aws.ToString(object.Key))
			}
		}
	}
	return keys, nil
}

func (s *s3Store) expiredUploads(ctx context.Context, before time.Time) ([]multipartUpload, error) {
	pages := s3.NewListMultipartUploadsPaginator(s.client, &s3.ListMultipartUploadsInput{Bucket: aws.String(s.bucket)})
	var uploads []multipartUpload
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list incomplete uploads: %w", err)
		}
		for _, upload := range page.Uploads {
			if upload.Initiated != nil && upload.Initiated.Before(before) {
				uploads = append(uploads, multipartUpload{Key: aws.ToString(upload.Key), ID: aws.ToString(upload.UploadId)})
			}
		}
	}
	return uploads, nil
}

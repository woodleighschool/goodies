package bloby

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gabriel-vasile/mimetype"
)

// S3 limits each multipart upload to 10,000 parts.
const s3MaximumMultipartParts = 10_000

// Ingestor reserves, uploads, classifies, and finalizes storage objects.
type Ingestor struct {
	objects *ObjectStore
	backend Backend
}

// UploadAction is the backend-selected action for uploading an object's bytes.
type UploadAction interface {
	isUploadAction()
}

// DirectUploadAction uploads the complete object to one target.
type DirectUploadAction struct {
	Target UploadTarget
}

func (DirectUploadAction) isUploadAction() {}

// MultipartUploadAction uploads the object through the multipart API.
type MultipartUploadAction struct{}

func (MultipartUploadAction) isUploadAction() {}

// NewIngestor returns an object ingestion service for backend.
func NewIngestor(objects *ObjectStore, backend Backend) *Ingestor {
	return &Ingestor{objects: objects, backend: backend}
}

// Begin reserves an object and selects the configured backend's upload action.
func (s *Ingestor) Begin(
	ctx context.Context,
	prefix string,
	filename string,
	sizeBytes int64,
) (*Object, UploadAction, error) {
	if sizeBytes < 0 {
		return nil, nil, fmt.Errorf("%w: size_bytes must not be negative", ErrInvalidInput)
	}
	object, err := s.objects.CreatePending(ctx, prefix, filename)
	if err != nil {
		return nil, nil, err
	}

	action, err := s.backend.beginUpload(ctx, object.Key(), sizeBytes)
	if err != nil {
		return nil, nil, errors.Join(err, s.Delete(ctx, object.ID, prefix))
	}
	if _, ok := action.(MultipartUploadAction); ok {
		if err := s.createMultipart(ctx, object.ID, prefix); err != nil {
			return nil, nil, errors.Join(err, s.Delete(ctx, object.ID, prefix))
		}
	}
	return object, action, nil
}

// BeginDirect reserves an object and returns its direct upload target.
func (s *Ingestor) BeginDirect(
	ctx context.Context,
	prefix string,
	filename string,
) (*Object, UploadTarget, error) {
	object, err := s.objects.CreatePending(ctx, prefix, filename)
	if err != nil {
		return nil, UploadTarget{}, err
	}
	target, err := s.backend.PresignPut(ctx, object.Key(), 0)
	if err != nil {
		return nil, UploadTarget{}, errors.Join(err, s.Delete(ctx, object.ID, prefix))
	}
	return object, target, nil
}

// Write ingests server-generated content directly into the registry.
func (s *Ingestor) Write(
	ctx context.Context,
	prefix string,
	filename string,
	contentType string,
	body []byte,
) (*Object, error) {
	object, err := s.objects.CreatePending(ctx, prefix, filename)
	if err != nil {
		return nil, err
	}
	if err := s.backend.Put(
		ctx,
		object.Key(),
		bytes.NewReader(body),
		PutOptions{ContentType: contentType},
	); err != nil {
		return nil, errors.Join(err, s.objects.Delete(ctx, object.ID))
	}
	hash := sha256.Sum256(body)
	available, err := s.objects.MarkAvailable(
		ctx,
		object.ID,
		int64(len(body)),
		contentType,
		hex.EncodeToString(hash[:]),
	)
	if err != nil {
		return nil, errors.Join(err, s.objects.Delete(ctx, object.ID))
	}
	return available, nil
}

// Finalize classifies and hashes uploaded bytes, then marks the object available.
func (s *Ingestor) Finalize(
	ctx context.Context,
	objectID int64,
	prefix string,
) (*Object, error) {
	object, err := s.objects.GetByID(ctx, objectID)
	if err != nil {
		return nil, err
	}
	if object.Prefix != prefix {
		return nil, fmt.Errorf("%w: object has the wrong storage prefix", ErrInvalidInput)
	}
	if object.Available() {
		return object, nil
	}
	object, err = s.objects.RefreshPending(ctx, object.ID)
	if err != nil {
		return nil, err
	}
	if object.MultipartUploadID != nil {
		return nil, fmt.Errorf("%w: multipart upload must be completed before finalization", ErrInvalidInput)
	}
	metadata, err := s.inspect(ctx, object.Key())
	if err != nil {
		return nil, err
	}
	return s.objects.MarkAvailable(
		ctx,
		object.ID,
		metadata.sizeBytes,
		metadata.contentType,
		metadata.sha256,
	)
}

func (s *Ingestor) createMultipart(
	ctx context.Context,
	objectID int64,
	prefix string,
) error {
	object, backend, err := s.multipartObject(ctx, objectID, prefix)
	if err != nil {
		return err
	}
	if object.MultipartUploadID != nil {
		return nil
	}
	objectExists, err := s.objectExists(ctx, object.Key())
	if err != nil {
		return err
	}
	if objectExists {
		return fmt.Errorf(
			"%w: multipart upload is already completed and ready to finalize",
			ErrInvalidInput,
		)
	}
	uploadID, err := backend.CreateMultipartUpload(ctx, object.Key())
	if err != nil {
		return err
	}
	_, created, err := s.objects.RecordMultipartUploadID(ctx, object.ID, uploadID)
	if err != nil {
		return errors.Join(err, backend.AbortMultipartUpload(ctx, object.Key(), uploadID))
	}
	if !created {
		abortErr := backend.AbortMultipartUpload(ctx, object.Key(), uploadID)
		if abortErr != nil && !errors.Is(abortErr, ErrMultipartUploadNotFound) {
			return abortErr
		}
	}
	return nil
}

// PresignMultipartPart returns an S3 PUT target for a recorded multipart upload.
func (s *Ingestor) PresignMultipartPart(
	ctx context.Context,
	objectID int64,
	prefix string,
	partNumber int32,
) (UploadTarget, error) {
	if partNumber < 1 || partNumber > s3MaximumMultipartParts {
		return UploadTarget{}, fmt.Errorf("%w: part_number must be between 1 and 10000", ErrInvalidInput)
	}
	object, backend, err := s.multipartObject(ctx, objectID, prefix)
	if err != nil {
		return UploadTarget{}, err
	}
	if object.MultipartUploadID == nil {
		return UploadTarget{}, fmt.Errorf("%w: multipart upload has not been created", ErrInvalidInput)
	}
	return backend.PresignMultipartPart(ctx, object.Key(), *object.MultipartUploadID, partNumber, 0)
}

// CompleteMultipart assembles uploaded parts at the object's storage key.
func (s *Ingestor) CompleteMultipart(
	ctx context.Context,
	objectID int64,
	prefix string,
	parts []CompletedPart,
) error {
	if err := validateCompletedParts(parts); err != nil {
		return err
	}
	object, backend, err := s.multipartObject(ctx, objectID, prefix)
	if err != nil {
		return err
	}
	if object.MultipartUploadID == nil {
		exists, existsErr := s.objectExists(ctx, object.Key())
		if existsErr != nil {
			return existsErr
		}
		if exists {
			return nil
		}
		return fmt.Errorf("%w: multipart upload has not been created", ErrInvalidInput)
	}
	uploadID := *object.MultipartUploadID
	err = backend.CompleteMultipartUpload(ctx, object.Key(), uploadID, parts)
	if errors.Is(err, ErrMultipartUploadNotFound) {
		exists, existsErr := s.objectExists(ctx, object.Key())
		if existsErr != nil {
			return existsErr
		}
		if !exists {
			return err
		}
	} else if err != nil {
		return err
	}
	return s.objects.ClearMultipartUploadID(ctx, object.ID, uploadID)
}

// Delete removes an object under prefix and aborts its multipart upload, if any.
func (s *Ingestor) Delete(ctx context.Context, objectID int64, prefix string) error {
	object, err := s.objects.GetByID(ctx, objectID)
	if err != nil {
		return err
	}
	if object.Prefix != prefix {
		return fmt.Errorf("%w: object has the wrong storage prefix", ErrInvalidInput)
	}
	if !object.Available() {
		object, err = s.objects.RefreshPending(ctx, object.ID)
		if err != nil {
			return err
		}
	}
	if object.MultipartUploadID != nil {
		backend, err := s.multipartBackend()
		if err != nil {
			return err
		}
		if err := backend.AbortMultipartUpload(ctx, object.Key(), *object.MultipartUploadID); err != nil &&
			!errors.Is(err, ErrMultipartUploadNotFound) {
			return err
		}
	}
	if !object.Available() {
		if err := s.backend.Delete(ctx, object.Key()); err != nil {
			return fmt.Errorf("delete %q: %w", object.Key(), err)
		}
	}
	return s.objects.Delete(ctx, object.ID)
}

func (s *Ingestor) multipartObject(
	ctx context.Context,
	objectID int64,
	prefix string,
) (*Object, MultipartBackend, error) {
	backend, err := s.multipartBackend()
	if err != nil {
		return nil, nil, err
	}
	object, err := s.objects.GetByID(ctx, objectID)
	if err != nil {
		return nil, nil, err
	}
	if object.Prefix != prefix {
		return nil, nil, fmt.Errorf("%w: object has the wrong storage prefix", ErrInvalidInput)
	}
	if object.Available() {
		return nil, nil, fmt.Errorf("%w: storage object is already finalized", ErrInvalidInput)
	}
	object, err = s.objects.RefreshPending(ctx, object.ID)
	if err != nil {
		return nil, nil, err
	}
	return object, backend, nil
}

func (s *Ingestor) multipartBackend() (MultipartBackend, error) {
	backend, ok := s.backend.(MultipartBackend)
	if !ok {
		return nil, fmt.Errorf("%w: multipart uploads require S3 storage", ErrInvalidInput)
	}
	return backend, nil
}

func (s *Ingestor) objectExists(ctx context.Context, key string) (bool, error) {
	reader, _, err := s.backend.Open(ctx, key)
	if errors.Is(err, ErrObjectNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := reader.Close(); err != nil {
		return false, err
	}
	return true, nil
}

func validateCompletedParts(parts []CompletedPart) error {
	if len(parts) == 0 {
		return fmt.Errorf("%w: multipart completion requires at least one part", ErrInvalidInput)
	}
	var previous int32
	for _, part := range parts {
		if part.PartNumber < 1 || part.PartNumber > s3MaximumMultipartParts {
			return fmt.Errorf("%w: part_number must be between 1 and 10000", ErrInvalidInput)
		}
		if part.PartNumber <= previous {
			return fmt.Errorf("%w: multipart parts must be strictly ascending", ErrInvalidInput)
		}
		if strings.TrimSpace(part.ETag) == "" {
			return fmt.Errorf("%w: multipart part etag must not be blank", ErrInvalidInput)
		}
		previous = part.PartNumber
	}
	return nil
}

type objectMetadata struct {
	sizeBytes   int64
	contentType string
	sha256      string
}

func (s *Ingestor) inspect(ctx context.Context, key string) (objectMetadata, error) {
	reader, info, err := s.backend.Open(ctx, key)
	if err != nil {
		return objectMetadata{}, err
	}
	hash := sha256.New()
	var size byteCount
	metadata := io.MultiWriter(hash, &size)
	detected, err := mimetype.DetectReader(io.TeeReader(reader, metadata))
	if err == nil {
		_, err = io.Copy(metadata, reader)
	}
	closeErr := reader.Close()
	if err != nil {
		return objectMetadata{}, fmt.Errorf("read %q: %w", key, err)
	}
	if closeErr != nil {
		return objectMetadata{}, fmt.Errorf("close %q: %w", key, closeErr)
	}
	if int64(size) != info.Size {
		return objectMetadata{}, fmt.Errorf("read %q: backend size changed during ingestion", key)
	}
	return objectMetadata{
		sizeBytes:   int64(size),
		contentType: detected.String(),
		sha256:      hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

type byteCount int64

func (c *byteCount) Write(p []byte) (int, error) {
	*c += byteCount(len(p))
	return len(p), nil
}

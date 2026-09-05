package bloby

import (
	"bytes"
	"context"
	"crypto/rand"
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

// Begin reserves an object and selects the configured backend's upload action.
func (s *Service) Begin(
	ctx context.Context,
	prefix string,
	filename string,
	sizeBytes int64,
) (*Object, UploadAction, error) {
	if sizeBytes < 0 {
		return nil, UploadAction{}, fmt.Errorf("%w: size_bytes must not be negative", ErrInvalidInput)
	}
	object, err := s.createPending(ctx, prefix, filename)
	if err != nil {
		return nil, UploadAction{}, err
	}

	action, err := s.backend.beginUpload(ctx, object.stagingKey(), sizeBytes)
	if err != nil {
		return nil, UploadAction{}, errors.Join(err, s.Delete(ctx, object.ID, prefix))
	}
	if action.Strategy == StrategyMultipart {
		if err := s.createMultipart(ctx, object); err != nil {
			return nil, UploadAction{}, errors.Join(err, s.Delete(ctx, object.ID, prefix))
		}
	}
	return object, action, nil
}

// BeginDirect reserves an object and returns its direct upload target.
func (s *Service) BeginDirect(
	ctx context.Context,
	prefix string,
	filename string,
) (*Object, UploadAction, error) {
	object, err := s.createPending(ctx, prefix, filename)
	if err != nil {
		return nil, UploadAction{}, err
	}
	target, err := s.backend.PresignPut(ctx, object.stagingKey(), 0)
	if err != nil {
		return nil, UploadAction{}, errors.Join(err, s.Delete(ctx, object.ID, prefix))
	}
	return object, UploadAction{Strategy: StrategyDirectPut, Target: &target}, nil
}

// Write ingests server-generated content directly into the registry.
func (s *Service) Write(ctx context.Context, prefix, filename, contentType string, body []byte) (*Object, error) {
	contentType, err := normalizeContentType(contentType)
	if err != nil {
		return nil, err
	}
	object, err := s.createPending(ctx, prefix, filename)
	if err != nil {
		return nil, err
	}
	key := candidateKey(object)
	if err := s.backend.Put(ctx, key, bytes.NewReader(body), putOptions{ContentType: contentType}); err != nil {
		s.deleteCandidate(ctx, key)
		s.DeleteUnreferenced(ctx, object.ID)
		return nil, err
	}
	hash := sha256.Sum256(body)
	available, err := s.markAvailable(ctx, object.ID, int64(len(body)), contentType, hex.EncodeToString(hash[:]), key)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), objectCleanupTimeout)
		defer cancel()
		deleted, deleteErr := s.registry.Delete(cleanupCtx, object.ID)
		switch {
		case deleteErr == nil:
			s.deleteBytes(cleanupCtx, deleted)
			if deleted.Key() != key {
				s.deleteCandidate(cleanupCtx, key)
			}
		case errors.Is(deleteErr, ErrNotFound):
			s.deleteCandidate(cleanupCtx, key)
		default:
			// A publish may have committed before its response was lost. Keep its
			// bytes unless the registry confirms that the object was removed.
			s.logger.WarnContext(cleanupCtx, "failed write cleanup could not remove object", "object_id", object.ID, "err", deleteErr)
		}
		return nil, err
	}
	return available, nil
}

func candidateKey(object *Object) string {
	return fmt.Sprintf("%s%d/%s/%s", candidatePrefix, object.ID, rand.Text(), object.Filename)
}

// Finalize classifies and hashes uploaded bytes, then marks the object available.
func (s *Service) Finalize(
	ctx context.Context,
	objectID int64,
	prefix string,
) (*Object, error) {
	object, err := s.registry.GetByID(ctx, objectID)
	if err != nil {
		return nil, err
	}
	if object.Prefix != prefix {
		return nil, fmt.Errorf("%w: object has the wrong storage prefix", ErrInvalidInput)
	}
	if object.Available() {
		return object, nil
	}
	object, err = s.registry.RefreshPending(ctx, object.ID)
	if errors.Is(err, ErrNotFound) {
		current, getErr := s.registry.GetByID(ctx, objectID)
		if getErr == nil && current.Available() {
			return current, nil
		}
	}
	if err != nil {
		return nil, err
	}
	if object.MultipartUploadID != nil {
		return nil, fmt.Errorf("%w: multipart upload must be completed before finalization", ErrInvalidInput)
	}
	key := candidateKey(object)
	if err := s.backend.seal(ctx, object.stagingKey(), key); err != nil {
		s.deleteCandidate(ctx, key)
		if current, getErr := s.registry.GetByID(ctx, object.ID); getErr == nil && current.Available() {
			return current, nil
		}
		return nil, err
	}
	metadata, err := s.inspect(ctx, key)
	if err != nil {
		s.deleteCandidate(ctx, key)
		if current, getErr := s.registry.GetByID(ctx, object.ID); getErr == nil && current.Available() {
			return current, nil
		}
		return nil, err
	}
	available, err := s.markAvailable(
		ctx,
		object.ID,
		metadata.sizeBytes,
		metadata.contentType,
		metadata.sha256,
		key,
	)
	if errors.Is(err, ErrNotFound) || (err == nil && available.Key() != key) {
		// A deletion, expiry, or another finalizer won the registry transition.
		s.deleteCandidate(ctx, key)
	}
	if err == nil {
		s.deleteStaging(ctx, object)
	}
	// Other registry errors can be ambiguous commits. Keep the candidate until
	// a retry reads the winning key or age-based cleanup resolves ownership.
	return available, err
}

func (s *Service) deleteCandidate(ctx context.Context, key string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), objectCleanupTimeout)
	defer cancel()
	if err := s.backend.Delete(cleanupCtx, key); err != nil {
		s.logger.WarnContext(cleanupCtx, "storage candidate could not be removed", "key", key, "err", err)
	}
}

func (s *Service) deleteStaging(ctx context.Context, object *Object) {
	if err := s.backend.Delete(ctx, object.stagingKey()); err != nil {
		s.logger.WarnContext(ctx, "storage staging bytes could not be removed", "object_id", object.ID, "err", err)
	}
}

func (s *Service) createMultipart(ctx context.Context, object *Object) error {
	backend, err := s.multipartBackend()
	if err != nil {
		return err
	}
	uploadID, err := backend.CreateMultipartUpload(ctx, object.stagingKey())
	if err != nil {
		return err
	}
	if err := s.registry.RecordMultipartUploadID(ctx, object.ID, uploadID); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), objectCleanupTimeout)
		defer cancel()
		return errors.Join(err, backend.AbortMultipartUpload(cleanupCtx, object.stagingKey(), uploadID))
	}
	return nil
}

// PresignMultipartPart returns an S3 PUT target for a recorded multipart upload.
func (s *Service) PresignMultipartPart(
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
	return backend.PresignMultipartPart(ctx, object.stagingKey(), *object.MultipartUploadID, partNumber, 0)
}

// CompleteMultipart assembles uploaded parts at the object's storage key.
func (s *Service) CompleteMultipart(
	ctx context.Context,
	objectID int64,
	prefix string,
	parts []CompletedPart,
) error {
	if err := validateCompletedParts(parts); err != nil {
		return err
	}
	current, err := s.registry.GetByID(ctx, objectID)
	if err != nil {
		return err
	}
	if current.Prefix != prefix {
		return fmt.Errorf("%w: object has the wrong storage prefix", ErrInvalidInput)
	}
	if current.Available() {
		return nil
	}
	object, backend, err := s.multipartObject(ctx, objectID, prefix)
	if err != nil {
		return err
	}
	if object.MultipartUploadID == nil {
		exists, existsErr := s.objectExists(ctx, object.stagingKey())
		if existsErr != nil {
			return existsErr
		}
		if exists {
			return nil
		}
		return fmt.Errorf("%w: multipart upload has not been created", ErrInvalidInput)
	}
	uploadID := *object.MultipartUploadID
	err = backend.CompleteMultipartUpload(ctx, object.stagingKey(), uploadID, parts)
	if errors.Is(err, ErrMultipartUploadNotFound) {
		exists, existsErr := s.objectExists(ctx, object.stagingKey())
		if existsErr != nil {
			return existsErr
		}
		if !exists {
			return err
		}
	} else if err != nil {
		return err
	}
	return s.registry.ClearMultipartUploadID(ctx, object.ID, uploadID)
}

// Delete removes an authorized object under prefix and then removes its bytes.
// Registry reference constraints are checked before touching stored content.
func (s *Service) Delete(ctx context.Context, objectID int64, prefix string) error {
	object, err := s.registry.GetByID(ctx, objectID)
	if err != nil {
		return err
	}
	if object.Prefix != prefix {
		return fmt.Errorf("%w: object has the wrong storage prefix", ErrInvalidInput)
	}
	object, err = s.registry.Delete(ctx, object.ID)
	if err != nil {
		return err
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), objectCleanupTimeout)
	defer cancel()
	s.deleteBytes(cleanupCtx, object)
	return nil
}

func (s *Service) multipartObject(
	ctx context.Context,
	objectID int64,
	prefix string,
) (*Object, multipartBackend, error) {
	backend, err := s.multipartBackend()
	if err != nil {
		return nil, nil, err
	}
	object, err := s.registry.GetByID(ctx, objectID)
	if err != nil {
		return nil, nil, err
	}
	if object.Prefix != prefix {
		return nil, nil, fmt.Errorf("%w: object has the wrong storage prefix", ErrInvalidInput)
	}
	if object.Available() {
		return nil, nil, fmt.Errorf("%w: storage object is already finalized", ErrInvalidInput)
	}
	object, err = s.registry.RefreshPending(ctx, object.ID)
	if err != nil {
		return nil, nil, err
	}
	return object, backend, nil
}

func (s *Service) multipartBackend() (multipartBackend, error) {
	backend, ok := s.backend.(multipartBackend)
	if !ok {
		return nil, fmt.Errorf("%w: multipart uploads require S3 storage", ErrInvalidInput)
	}
	return backend, nil
}

func (s *Service) objectExists(ctx context.Context, key string) (bool, error) {
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

func (s *Service) inspect(ctx context.Context, key string) (objectMetadata, error) {
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

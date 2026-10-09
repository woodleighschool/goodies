package bloby

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gabriel-vasile/mimetype"
)

// S3 limits each multipart upload to 10,000 parts.
const s3MaximumMultipartParts = 10_000

// Content types are detected from this many leading bytes, the detector's limit.
const contentTypeHeadBytes = 3072

// Begin reserves an object for the declared content and selects the configured
// backend's upload action.
func (s *Service) Begin(
	ctx context.Context,
	prefix string,
	filename string,
	content Content,
) (*Object, UploadAction, error) {
	object, err := s.createPending(ctx, prefix, filename, content)
	if err != nil {
		return nil, UploadAction{}, err
	}

	action, err := s.backend.beginUpload(ctx, object.key(), content)
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

// BeginDirect reserves an object for the declared content and returns a single
// upload target, whatever its size.
func (s *Service) BeginDirect(
	ctx context.Context,
	prefix string,
	filename string,
	content Content,
) (*Object, UploadAction, error) {
	object, err := s.createPending(ctx, prefix, filename, content)
	if err != nil {
		return nil, UploadAction{}, err
	}
	target, err := s.backend.PresignPut(ctx, object.key(), content, 0)
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
	content, err := Digest(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	object, err := s.createPending(ctx, prefix, filename, content)
	if err != nil {
		return nil, err
	}
	if err := s.backend.Put(ctx, object.key(), bytes.NewReader(body), content, putOptions{ContentType: contentType}); err != nil {
		s.DeleteUnreferenced(ctx, object.ID)
		return nil, err
	}
	available, err := s.markAvailable(ctx, object, contentType)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), objectCleanupTimeout)
		defer cancel()
		deleted, deleteErr := s.registry.Delete(cleanupCtx, object.ID)
		switch {
		case deleteErr == nil:
			s.deleteBytes(cleanupCtx, deleted)
		case errors.Is(deleteErr, ErrNotFound):
			s.deleteBytes(cleanupCtx, object)
		default:
			// A publish may have committed before its response was lost. Keep its
			// bytes unless the registry confirms that the object was removed.
			s.logger.WarnContext(cleanupCtx, "failed write cleanup could not remove object", "object_id", object.ID, "err", deleteErr)
		}
		return nil, err
	}
	return available, nil
}

// Finalize publishes an upload once storage holds exactly the declared bytes.
// It completes a multipart upload from the parts the provider received, checks
// the provider's record of the object against the declaration, and detects the
// content type from the leading bytes.
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
	content, err := object.content()
	if err != nil {
		return nil, err
	}
	if object.MultipartUploadID != nil {
		if err := s.completeMultipart(ctx, object, content); err != nil {
			return nil, err
		}
	}
	started := time.Now()
	head, err := s.inspect(ctx, object.key(), content)
	if errors.Is(err, ErrObjectNotFound) {
		return nil, fmt.Errorf("%w: upload has not arrived: %w", ErrInvalidInput, err)
	}
	if err != nil {
		return nil, err
	}
	available, err := s.markAvailable(ctx, object, mimetype.Detect(head).String())
	if err != nil {
		return nil, err
	}
	s.logger.DebugContext(ctx, "storage object published", "object_id", object.ID, "size_bytes", content.SizeBytes, "duration_ms", time.Since(started).Milliseconds())
	return available, nil
}

// inspect checks the stored bytes against content and returns the leading
// bytes that identify their type.
func (s *Service) inspect(ctx context.Context, key string, content Content) ([]byte, error) {
	if err := s.backend.verify(ctx, key, content); err != nil {
		return nil, err
	}
	if content.SizeBytes == 0 {
		return nil, nil
	}
	return s.backend.head(ctx, key, contentTypeHeadBytes)
}

func (s *Service) createMultipart(ctx context.Context, object *Object) error {
	backend, err := s.multipartBackend()
	if err != nil {
		return err
	}
	uploadID, err := backend.CreateMultipartUpload(ctx, object.key())
	if err != nil {
		return err
	}
	if err := s.registry.RecordMultipartUploadID(ctx, object.ID, uploadID); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), objectCleanupTimeout)
		defer cancel()
		return errors.Join(err, backend.AbortMultipartUpload(cleanupCtx, object.key(), uploadID))
	}
	return nil
}

// completeMultipart assembles the uploaded parts at the object's key.
func (s *Service) completeMultipart(ctx context.Context, object *Object, content Content) error {
	backend, err := s.multipartBackend()
	if err != nil {
		return err
	}
	uploadID := *object.MultipartUploadID
	err = backend.CompleteMultipartUpload(ctx, object.key(), uploadID, content.SizeBytes)
	// A completion whose response was lost leaves no upload behind; verification
	// then decides whether its object exists.
	if err != nil && !errors.Is(err, ErrMultipartUploadNotFound) {
		return err
	}
	return s.registry.ClearMultipartUploadID(ctx, object.ID, uploadID)
}

// PresignMultipartPart returns an S3 PUT target that accepts only the part
// bytes matching crc64nvme.
func (s *Service) PresignMultipartPart(
	ctx context.Context,
	objectID int64,
	prefix string,
	partNumber int32,
	crc64nvme string,
) (UploadTarget, error) {
	if partNumber < 1 || partNumber > s3MaximumMultipartParts {
		return UploadTarget{}, fmt.Errorf("%w: part_number must be between 1 and 10000", ErrInvalidInput)
	}
	if !crc64NVMEPattern.MatchString(crc64nvme) {
		return UploadTarget{}, fmt.Errorf("%w: part needs a CRC64NVME", ErrInvalidInput)
	}
	backend, err := s.multipartBackend()
	if err != nil {
		return UploadTarget{}, err
	}
	object, err := s.registry.GetByID(ctx, objectID)
	if err != nil {
		return UploadTarget{}, err
	}
	if object.Prefix != prefix {
		return UploadTarget{}, fmt.Errorf("%w: object has the wrong storage prefix", ErrInvalidInput)
	}
	if object.Available() {
		return UploadTarget{}, fmt.Errorf("%w: storage object is already finalized", ErrInvalidInput)
	}
	object, err = s.registry.RefreshPending(ctx, object.ID)
	if err != nil {
		return UploadTarget{}, err
	}
	if object.MultipartUploadID == nil {
		return UploadTarget{}, fmt.Errorf("%w: object has no multipart upload", ErrInvalidInput)
	}
	return backend.PresignMultipartPart(ctx, object.key(), *object.MultipartUploadID, partNumber, crc64nvme, 0)
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

func (s *Service) multipartBackend() (multipartBackend, error) {
	backend, ok := s.backend.(multipartBackend)
	if !ok {
		return nil, fmt.Errorf("%w: multipart uploads require S3 storage", ErrInvalidInput)
	}
	return backend, nil
}

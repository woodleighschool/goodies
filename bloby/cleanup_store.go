package bloby

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	objectCleanupTimeout       = 15 * time.Second
	uploadCleanupInterval      = time.Hour
	uploadCleanupBatchSize     = 100
	uploadCleanupRetryDelay    = uploadCleanupInterval
	minimumPendingUploadMaxAge = 24 * time.Hour
)

// UploadCleanup removes abandoned pending uploads independently of request lifetimes.
type UploadCleanup struct {
	stop context.CancelFunc
	done <-chan struct{}
}

// Stop cancels cleanup and waits for the in-flight backend operation to exit.
func (c *UploadCleanup) Stop() {
	c.stop()
	<-c.done
}

// StartUploadCleanup starts the storage-owned abandoned-upload cleanup loop.
func StartUploadCleanup(
	ctx context.Context,
	ingestor *Ingestor,
	transferTTL time.Duration,
	logger *slog.Logger,
) *UploadCleanup {
	ctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		uploadCleanupLoop(ctx, ingestor, pendingUploadMaxAge(transferTTL), logger)
	}()
	return &UploadCleanup{stop: stop, done: done}
}

func uploadCleanupLoop(
	ctx context.Context,
	ingestor *Ingestor,
	maxAge time.Duration,
	logger *slog.Logger,
) {
	sweepExpiredUploads(ctx, ingestor, maxAge, logger)
	ticker := time.NewTicker(uploadCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepExpiredUploads(ctx, ingestor, maxAge, logger)
		}
	}
}

func sweepExpiredUploads(
	ctx context.Context,
	ingestor *Ingestor,
	maxAge time.Duration,
	logger *slog.Logger,
) {
	now := time.Now()
	objects, err := ingestor.objects.claimExpiredPending(
		ctx,
		now.Add(-maxAge),
		now.Add(-uploadCleanupRetryDelay),
		uploadCleanupBatchSize,
	)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			logger.WarnContext(ctx, "abandoned upload cleanup failed", "operation", "claim", "err", err)
		}
		return
	}
	for i := range objects {
		cleanupCtx, cancel := context.WithTimeout(ctx, objectCleanupTimeout)
		err := ingestor.deleteExpiredUpload(cleanupCtx, &objects[i])
		cancel()
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.WarnContext(
				ctx,
				"abandoned upload cleanup failed",
				"object_id", objects[i].ID,
				"err", err,
			)
		}
	}
}

func pendingUploadMaxAge(transferTTL time.Duration) time.Duration {
	if transferTTL >= minimumPendingUploadMaxAge {
		return transferTTL + time.Hour
	}
	return minimumPendingUploadMaxAge
}

func (s *ObjectStore) claimExpiredPending(
	ctx context.Context,
	updatedBefore time.Time,
	retryBefore time.Time,
	limit int,
) ([]Object, error) {
	rows, err := s.pool.Query(ctx, `
WITH candidates AS (
    SELECT id
    FROM storage_objects
    WHERE available_at IS NULL
      AND (
          expired_at < $2
          OR (expired_at IS NULL AND updated_at < $1)
      )
    ORDER BY (expired_at IS NULL), COALESCE(expired_at, updated_at), id
    LIMIT $3
    FOR UPDATE SKIP LOCKED
)
UPDATE storage_objects AS objects
SET expired_at = now()
FROM candidates
WHERE objects.id = candidates.id
  AND objects.available_at IS NULL
RETURNING objects.id, objects.prefix, objects.filename, objects.content_type,
          objects.size_bytes, objects.sha256, objects.available_at,
          objects.multipart_upload_id, objects.created_at, objects.updated_at`,
		updatedBefore,
		retryBefore,
		limit,
	)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[Object])
}

func (s *ObjectStore) deleteExpiredPending(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `
DELETE FROM storage_objects
WHERE id = $1
  AND expired_at IS NOT NULL`, id)
	if err != nil {
		return deleteConflict(err, "expired storage upload is still referenced")
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Ingestor) deleteExpiredUpload(ctx context.Context, object *Object) error {
	if object.MultipartUploadID != nil {
		backend, err := s.multipartBackend()
		if err != nil {
			return err
		}
		if err := backend.AbortMultipartUpload(ctx, object.Key(), *object.MultipartUploadID); err != nil &&
			!errors.Is(err, ErrMultipartUploadNotFound) {
			return fmt.Errorf("abort multipart upload for %q: %w", object.Key(), err)
		}
	}
	if err := s.backend.Delete(ctx, object.Key()); err != nil {
		return fmt.Errorf("delete %q: %w", object.Key(), err)
	}
	return s.objects.deleteExpiredPending(ctx, object.ID)
}

package bloby

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
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

package bloby

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	objectCleanupTimeout       = 15 * time.Second
	uploadCleanupInterval      = time.Hour
	uploadCleanupBatchSize     = 100
	uploadCleanupRetryDelay    = uploadCleanupInterval
	minimumPendingUploadMaxAge = 24 * time.Hour
)

// RunCleanup removes abandoned uploads until ctx is canceled. The caller owns
// its goroutine and shutdown; all expiry policy and cleanup work belong here.
func (s *Service) RunCleanup(ctx context.Context) {
	s.sweepExpiredUploads(ctx)
	ticker := time.NewTicker(uploadCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepExpiredUploads(ctx)
		}
	}
}

func (s *Service) sweepExpiredUploads(ctx context.Context) {
	now := time.Now()
	before := now.Add(-pendingUploadMaxAge(s.transferTTL))
	objects, err := s.registry.ClaimExpiredPending(ctx, before, now.Add(-uploadCleanupRetryDelay), uploadCleanupBatchSize)
	if err != nil && !errors.Is(err, context.Canceled) {
		s.logger.WarnContext(ctx, "abandoned upload cleanup failed", "operation", "claim", "err", err)
	}
	for i := range objects {
		cleanupCtx, cancel := context.WithTimeout(ctx, objectCleanupTimeout)
		err := s.removeBytes(cleanupCtx, &objects[i])
		if err == nil {
			err = s.registry.DeleteExpiredPending(cleanupCtx, objects[i].ID)
		}
		cancel()
		if err != nil && !errors.Is(err, context.Canceled) {
			s.logger.WarnContext(ctx, "abandoned upload cleanup failed", "object_id", objects[i].ID, "err", err)
		}
	}
	// Signed PUTs can finish after finalization or deletion. Sweep staging by
	// age independently of registry rows so those late writes are also removed.
	if err := s.backend.cleanupStaging(ctx, before); err != nil && !errors.Is(err, context.Canceled) {
		s.logger.WarnContext(ctx, "abandoned upload cleanup failed", "operation", "staging", "err", err)
	}
	keys, err := s.backend.expiredCandidates(ctx, before)
	if err != nil && !errors.Is(err, context.Canceled) {
		s.logger.WarnContext(ctx, "abandoned upload cleanup failed", "operation", "candidates", "err", err)
	}
	for batch := range slices.Chunk(keys, uploadCleanupBatchSize) {
		candidateIDs := make(map[string]int64, len(batch))
		ids := make([]int64, 0, len(batch))
		for _, key := range batch {
			idText, _, ok := strings.Cut(strings.TrimPrefix(key, candidatePrefix), "/")
			id, err := strconv.ParseInt(idText, 10, 64)
			if ok && err == nil {
				candidateIDs[key] = id
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			continue
		}
		objects, err := s.registry.ListByIDs(ctx, ids)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				s.logger.WarnContext(ctx, "candidate cleanup could not resolve objects", "err", err)
			}
			continue
		}
		for key, id := range candidateIDs {
			if object, ok := objects[id]; ok && (!object.Available() || object.Key() == key) {
				continue
			}
			if err := s.backend.Delete(ctx, key); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.WarnContext(ctx, "candidate cleanup failed", "key", key, "err", err)
			}
		}
	}
}

func pendingUploadMaxAge(transferTTL time.Duration) time.Duration {
	if transferTTL >= minimumPendingUploadMaxAge {
		return transferTTL + time.Hour
	}
	return minimumPendingUploadMaxAge
}

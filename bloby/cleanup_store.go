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
	s.sweepUnreferenced(ctx, before)
	s.sweepOrphans(ctx, before)
}

// sweepOrphans removes bytes that no registry object owns. A signed upload can
// land after its object was deleted, and a failed delete can strand bytes.
func (s *Service) sweepOrphans(ctx context.Context, before time.Time) {
	keys, err := s.backend.expiredObjects(ctx, before)
	if err != nil && !errors.Is(err, context.Canceled) {
		s.logger.WarnContext(ctx, "orphan cleanup failed", "operation", "list objects", "err", err)
	}
	for batch := range slices.Chunk(keys, uploadCleanupBatchSize) {
		owners, err := s.owners(ctx, batch)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				s.logger.WarnContext(ctx, "orphan cleanup could not resolve objects", "err", err)
			}
			continue
		}
		for _, key := range batch {
			id, ok := ownerID(key)
			if !ok {
				continue
			}
			if owner, exists := owners[id]; exists && owner.key() == key {
				continue
			}
			if err := s.backend.Delete(ctx, key); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.WarnContext(ctx, "orphan cleanup failed", "key", key, "err", err)
			}
		}
	}

	backend, ok := s.backend.(multipartBackend)
	if !ok {
		return
	}
	uploads, err := backend.expiredUploads(ctx, before)
	if err != nil && !errors.Is(err, context.Canceled) {
		s.logger.WarnContext(ctx, "orphan cleanup failed", "operation", "list uploads", "err", err)
	}
	for batch := range slices.Chunk(uploads, uploadCleanupBatchSize) {
		keys := make([]string, len(batch))
		for i, upload := range batch {
			keys[i] = upload.Key
		}
		owners, err := s.owners(ctx, keys)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				s.logger.WarnContext(ctx, "orphan cleanup could not resolve uploads", "err", err)
			}
			continue
		}
		for _, upload := range batch {
			if id, ok := ownerID(upload.Key); ok {
				owner, exists := owners[id]
				if exists && owner.MultipartUploadID != nil && *owner.MultipartUploadID == upload.ID {
					continue
				}
			}
			err := backend.AbortMultipartUpload(ctx, upload.Key, upload.ID)
			if err != nil && !errors.Is(err, ErrMultipartUploadNotFound) && !errors.Is(err, context.Canceled) {
				s.logger.WarnContext(ctx, "orphan cleanup failed", "key", upload.Key, "err", err)
			}
		}
	}
}

// owners returns the registry objects named by the keys' ID segments.
func (s *Service) owners(ctx context.Context, keys []string) (map[int64]Object, error) {
	ids := make([]int64, 0, len(keys))
	for _, key := range keys {
		if id, ok := ownerID(key); ok {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return s.registry.ListByIDs(ctx, ids)
}

// ownerID parses the registry ID that leads every object key.
func ownerID(key string) (int64, bool) {
	rest, ok := strings.CutPrefix(key, objectPrefix)
	if !ok {
		return 0, false
	}
	idText, _, ok := strings.Cut(rest, "/")
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(idText, 10, 64)
	return id, err == nil
}

// sweepUnreferenced removes finalized objects whose owner never attached them, or
// whose best-effort removal failed after a detaching mutation committed. The
// registry's reference constraints still decide each delete, so an object
// attached since the listing survives.
func (s *Service) sweepUnreferenced(ctx context.Context, before time.Time) {
	if len(s.referencedPrefixes) == 0 {
		return
	}
	objects, err := s.registry.ListUnreferenced(ctx, s.referencedPrefixes, before, uploadCleanupBatchSize)
	if err != nil && !errors.Is(err, context.Canceled) {
		s.logger.WarnContext(ctx, "unreferenced object cleanup failed", "operation", "list", "err", err)
	}
	ids := make([]int64, len(objects))
	for i := range objects {
		ids[i] = objects[i].ID
	}
	s.DeleteUnreferenced(ctx, ids...)
}

func pendingUploadMaxAge(transferTTL time.Duration) time.Duration {
	if transferTTL >= minimumPendingUploadMaxAge {
		return transferTTL + time.Hour
	}
	return minimumPendingUploadMaxAge
}

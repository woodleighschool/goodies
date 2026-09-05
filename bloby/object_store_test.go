package bloby

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// memoryRegistry models the persistence seam for real backend lifecycle tests.
// PostgreSQL's own transitions and concurrency are tested in pgxstore.
type memoryRegistry struct {
	mu         sync.Mutex
	nextID     int64
	objects    map[int64]Object
	expired    map[int64]time.Time
	beforeMark func() error
	afterMark  func() error
	getFailure error
}

func newMemoryRegistry() *memoryRegistry {
	return &memoryRegistry{objects: make(map[int64]Object), expired: make(map[int64]time.Time)}
}

func (r *memoryRegistry) CreatePending(_ context.Context, prefix, filename string) (*Object, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	now := time.Now()
	object := Object{ID: r.nextID, Prefix: prefix, Filename: filename, CreatedAt: now, UpdatedAt: now}
	r.objects[object.ID] = object
	return &object, nil
}

func (r *memoryRegistry) get(id int64) (*Object, error) {
	if r.getFailure != nil {
		return nil, r.getFailure
	}
	object, ok := r.objects[id]
	if !ok || !r.expired[id].IsZero() {
		return nil, ErrNotFound
	}
	return &object, nil
}

func (r *memoryRegistry) GetByID(_ context.Context, id int64) (*Object, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.get(id)
}

func (r *memoryRegistry) MarkAvailable(_ context.Context, id, size int64, contentType, hash, storageKey string) (*Object, error) {
	if r.beforeMark != nil {
		if err := r.beforeMark(); err != nil {
			return nil, err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	object, err := r.get(id)
	if err != nil || object.Available() {
		return object, err
	}
	if object.MultipartUploadID != nil {
		return nil, ErrInvalidInput
	}
	now := time.Now()
	object.StorageKey = &storageKey
	object.SizeBytes = &size
	object.SHA256 = &hash
	object.ContentType = contentType
	object.AvailableAt = &now
	object.UpdatedAt = now
	r.objects[id] = *object
	if r.afterMark != nil {
		if err := r.afterMark(); err != nil {
			return nil, err
		}
	}
	return object, nil
}

func (r *memoryRegistry) RefreshPending(_ context.Context, id int64) (*Object, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	object, err := r.get(id)
	if err != nil {
		return nil, err
	}
	if object.Available() {
		return nil, ErrNotFound
	}
	object.UpdatedAt = time.Now()
	r.objects[id] = *object
	return object, nil
}

func (r *memoryRegistry) RecordMultipartUploadID(_ context.Context, id int64, uploadID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	object, err := r.get(id)
	if err != nil {
		return err
	}
	if object.Available() || object.MultipartUploadID != nil {
		return ErrNotFound
	}
	object.MultipartUploadID = &uploadID
	r.objects[id] = *object
	return nil
}

func (r *memoryRegistry) ClearMultipartUploadID(_ context.Context, id int64, uploadID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	object, err := r.get(id)
	if err != nil {
		return err
	}
	if object.MultipartUploadID != nil && *object.MultipartUploadID != uploadID {
		return ErrConflict
	}
	object.MultipartUploadID = nil
	r.objects[id] = *object
	return nil
}

func (r *memoryRegistry) Delete(_ context.Context, id int64) (*Object, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	object, err := r.get(id)
	if err != nil {
		return nil, err
	}
	delete(r.objects, id)
	return object, nil
}

func (r *memoryRegistry) ListByIDs(_ context.Context, ids []int64) (map[int64]Object, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getFailure != nil {
		return nil, r.getFailure
	}
	result := make(map[int64]Object)
	for _, id := range ids {
		if object, err := r.get(id); err == nil {
			result[id] = *object
		}
	}
	return result, nil
}

func (r *memoryRegistry) ListByPrefix(_ context.Context, prefix string, opts ListOptions) ([]Object, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result []Object
	for _, object := range r.objects {
		if object.Prefix == prefix && object.Available() && r.expired[object.ID].IsZero() {
			result = append(result, object)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID > result[j].ID })
	count := len(result)
	return result[min(opts.Offset, count):min(opts.Offset+opts.Limit, count)], count, nil
}

func (r *memoryRegistry) ClaimExpiredPending(_ context.Context, before, retry time.Time, limit int) ([]Object, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result []Object
	for id, object := range r.objects {
		expired := r.expired[id]
		if !object.Available() && ((expired.IsZero() && object.UpdatedAt.Before(before)) || (!expired.IsZero() && expired.Before(retry))) {
			r.expired[id] = time.Now()
			result = append(result, object)
			if len(result) == limit {
				break
			}
		}
	}
	return result, nil
}

func (r *memoryRegistry) DeleteExpiredPending(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.expired[id].IsZero() {
		return ErrNotFound
	}
	delete(r.objects, id)
	delete(r.expired, id)
	return nil
}

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
func hashString(body string) string {
	hash := sha256.Sum256([]byte(body))
	return hex.EncodeToString(hash[:])
}

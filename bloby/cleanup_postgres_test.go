//go:build postgres

package bloby

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/woodleighschool/goodies/bloby/internal/testdb"
)

func TestUploadCleanupRemovesAbandonedDirectUpload(t *testing.T) {
	db, ctx := testdb.Open(t, PostgresSchema())
	backend := newTestBackend(t)
	objects := NewObjectStore(db, backend, testLogger())
	ingestor := NewIngestor(objects, backend)
	object, _, err := ingestor.BeginDirect(ctx, "munki/packages", "installer.pkg")
	if err != nil {
		t.Fatalf("begin direct upload: %v", err)
	}
	if err := backend.Put(ctx, object.Key(), strings.NewReader("partial"), PutOptions{}); err != nil {
		t.Fatalf("write pending bytes: %v", err)
	}
	backdatePendingUpload(t, ctx, db, object.ID)

	sweepExpiredUploads(ctx, ingestor, minimumPendingUploadMaxAge, testLogger())

	if _, err := objects.GetByID(ctx, object.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get expired upload error = %v, want ErrNotFound", err)
	}
	if _, _, err := backend.Open(ctx, object.Key()); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("open pending bytes error = %v, want ErrObjectNotFound", err)
	}
	assertStorageObjectCount(t, ctx, db, object.ID, 0)
}

func TestUploadCleanupLeavesRecentPendingUpload(t *testing.T) {
	db, ctx := testdb.Open(t, PostgresSchema())
	backend := newTestBackend(t)
	objects := NewObjectStore(db, backend, testLogger())
	ingestor := NewIngestor(objects, backend)
	object, _, err := ingestor.BeginDirect(ctx, "munki/packages", "installer.pkg")
	if err != nil {
		t.Fatalf("begin direct upload: %v", err)
	}
	if err := backend.Put(ctx, object.Key(), strings.NewReader("partial"), PutOptions{}); err != nil {
		t.Fatalf("write pending bytes: %v", err)
	}

	sweepExpiredUploads(ctx, ingestor, minimumPendingUploadMaxAge, testLogger())

	if _, err := objects.GetByID(ctx, object.ID); err != nil {
		t.Fatalf("get recent upload: %v", err)
	}
	reader, _, err := backend.Open(ctx, object.Key())
	if err != nil {
		t.Fatalf("open recent pending bytes: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close recent staged bytes: %v", err)
	}
}

func TestUploadCleanupDoesNotClaimInFlightFinalization(t *testing.T) {
	db, ctx := testdb.Open(t, PostgresSchema())
	openStarted := make(chan struct{})
	releaseOpen := make(chan struct{})
	defer close(releaseOpen)
	backend := &blockingOpenBackend{
		Backend:     newTestBackend(t),
		openStarted: openStarted,
		releaseOpen: releaseOpen,
	}
	objects := NewObjectStore(db, backend, testLogger())
	ingestor := NewIngestor(objects, backend)
	object, _, err := ingestor.BeginDirect(ctx, "munki/packages", "installer.pkg")
	if err != nil {
		t.Fatalf("begin direct upload: %v", err)
	}
	if err := backend.Put(ctx, object.Key(), strings.NewReader("complete"), PutOptions{}); err != nil {
		t.Fatalf("write pending bytes: %v", err)
	}
	backdatePendingUpload(t, ctx, db, object.ID)

	finalized := make(chan error, 1)
	go func() {
		_, err := ingestor.Finalize(ctx, object.ID, object.Prefix)
		finalized <- err
	}()
	<-openStarted

	sweepExpiredUploads(ctx, ingestor, minimumPendingUploadMaxAge, testLogger())
	if _, err := objects.GetByID(ctx, object.ID); err != nil {
		t.Errorf("get in-flight upload: %v", err)
	}

	releaseOpen <- struct{}{}
	if err := <-finalized; err != nil {
		t.Fatalf("finalize upload: %v", err)
	}
}

func TestUploadCleanupRetriesMultipartFailure(t *testing.T) {
	db, ctx := testdb.Open(t, PostgresSchema())
	backend := &multipartCleanupBackend{
		Backend:  newTestBackend(t),
		abortErr: errors.New("provider unavailable"),
	}
	objects := NewObjectStore(db, backend, testLogger())
	ingestor := NewIngestor(objects, backend)
	object, err := objects.CreatePending(ctx, "munki/packages", "installer.pkg")
	if err != nil {
		t.Fatalf("create pending object: %v", err)
	}
	if _, _, err := objects.RecordMultipartUploadID(ctx, object.ID, "upload-1"); err != nil {
		t.Fatalf("record multipart upload: %v", err)
	}
	if err := backend.Put(ctx, object.Key(), strings.NewReader("assembled"), PutOptions{}); err != nil {
		t.Fatalf("write object bytes: %v", err)
	}
	backdatePendingUpload(t, ctx, db, object.ID)

	sweepExpiredUploads(ctx, ingestor, minimumPendingUploadMaxAge, testLogger())

	if _, err := objects.GetByID(ctx, object.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get claimed upload error = %v, want ErrNotFound", err)
	}
	assertStorageObjectCount(t, ctx, db, object.ID, 1)
	if _, err := objects.MarkAvailable(ctx, object.ID, 9, "application/octet-stream", validHash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finalize claimed upload error = %v, want ErrNotFound", err)
	}

	backend.abortErr = nil
	backdateCleanupAttempt(t, ctx, db, object.ID)
	sweepExpiredUploads(ctx, ingestor, minimumPendingUploadMaxAge, testLogger())

	assertStorageObjectCount(t, ctx, db, object.ID, 0)
	if backend.abortCalls.Load() != 2 {
		t.Fatalf("multipart abort calls = %d, want 2", backend.abortCalls.Load())
	}
	if _, _, err := backend.Open(ctx, object.Key()); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("open object bytes error = %v, want ErrObjectNotFound", err)
	}
}

func TestUploadCleanupClaimsRetriesBeforeNewExpirations(t *testing.T) {
	db, ctx := testdb.Open(t, PostgresSchema())
	objects := NewObjectStore(db, nil, testLogger())
	retry, err := objects.CreatePending(ctx, "munki/packages", "retry.pkg")
	if err != nil {
		t.Fatalf("create retry upload: %v", err)
	}
	backdatePendingUpload(t, ctx, db, retry.ID)
	claimed, err := objects.claimExpiredPending(ctx, time.Now(), time.Now(), 1)
	if err != nil {
		t.Fatalf("claim retry upload: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != retry.ID {
		t.Fatalf("first claimed uploads = %+v, want %d", claimed, retry.ID)
	}
	backdateCleanupAttempt(t, ctx, db, retry.ID)

	newlyExpired, err := objects.CreatePending(ctx, "munki/packages", "new.pkg")
	if err != nil {
		t.Fatalf("create newly expired upload: %v", err)
	}
	backdatePendingUpload(t, ctx, db, newlyExpired.ID)
	claimed, err = objects.claimExpiredPending(ctx, time.Now(), time.Now(), 1)
	if err != nil {
		t.Fatalf("reclaim upload: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != retry.ID {
		t.Fatalf("reclaimed uploads = %+v, want retry %d", claimed, retry.ID)
	}
}

func TestUploadCleanupLeasePreventsConcurrentWorkers(t *testing.T) {
	db, ctx := testdb.Open(t, PostgresSchema())
	abortStarted := make(chan struct{})
	releaseAbort := make(chan struct{})
	backend := &multipartCleanupBackend{
		Backend: newTestBackend(t),
		abort: func(context.Context) error {
			close(abortStarted)
			<-releaseAbort
			return nil
		},
	}
	objects := NewObjectStore(db, backend, testLogger())
	ingestor := NewIngestor(objects, backend)
	object, err := objects.CreatePending(ctx, "munki/packages", "installer.pkg")
	if err != nil {
		t.Fatalf("create pending object: %v", err)
	}
	if _, _, err := objects.RecordMultipartUploadID(ctx, object.ID, "upload-1"); err != nil {
		t.Fatalf("record multipart upload: %v", err)
	}
	backdatePendingUpload(t, ctx, db, object.ID)

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		sweepExpiredUploads(ctx, ingestor, minimumPendingUploadMaxAge, testLogger())
	}()
	<-abortStarted

	sweepExpiredUploads(ctx, ingestor, minimumPendingUploadMaxAge, testLogger())
	if backend.abortCalls.Load() != 1 {
		t.Errorf("multipart abort calls while leased = %d, want 1", backend.abortCalls.Load())
	}

	close(releaseAbort)
	<-firstDone
	assertStorageObjectCount(t, ctx, db, object.ID, 0)
}

func TestUploadCleanupLeaseCoversClaimedBatch(t *testing.T) {
	worstCaseBatchTime := time.Duration(uploadCleanupBatchSize) * objectCleanupTimeout
	if uploadCleanupRetryDelay < worstCaseBatchTime {
		t.Fatalf(
			"upload cleanup retry delay = %s, want at least %s",
			uploadCleanupRetryDelay,
			worstCaseBatchTime,
		)
	}
}

func TestUploadCleanupStopCancelsBackendWork(t *testing.T) {
	db, ctx := testdb.Open(t, PostgresSchema())
	started := make(chan struct{})
	canceled := make(chan struct{})
	backend := &multipartCleanupBackend{
		Backend: newTestBackend(t),
		abort: func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			close(canceled)
			return ctx.Err()
		},
	}
	objects := NewObjectStore(db, backend, testLogger())
	ingestor := NewIngestor(objects, backend)
	object, err := objects.CreatePending(ctx, "munki/packages", "installer.pkg")
	if err != nil {
		t.Fatalf("create pending object: %v", err)
	}
	if _, _, err := objects.RecordMultipartUploadID(ctx, object.ID, "upload-1"); err != nil {
		t.Fatalf("record multipart upload: %v", err)
	}
	backdatePendingUpload(t, ctx, db, object.ID)

	cleanup := StartUploadCleanup(t.Context(), ingestor, time.Minute, testLogger())
	<-started
	cleanup.Stop()
	<-canceled

	assertStorageObjectCount(t, ctx, db, object.ID, 1)
}

const validHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type multipartCleanupBackend struct {
	Backend

	abort      func(context.Context) error
	abortErr   error
	abortCalls atomic.Int32
}

type blockingOpenBackend struct {
	Backend

	openStarted chan struct{}
	releaseOpen chan struct{}
}

func (b *blockingOpenBackend) Open(ctx context.Context, key string) (ObjectReader, ObjectInfo, error) {
	close(b.openStarted)
	select {
	case <-ctx.Done():
		return nil, ObjectInfo{}, ctx.Err()
	case <-b.releaseOpen:
		return b.Backend.Open(ctx, key)
	}
}

func (*multipartCleanupBackend) CreateMultipartUpload(context.Context, string) (string, error) {
	return "upload-1", nil
}

func (*multipartCleanupBackend) PresignMultipartPart(
	context.Context,
	string,
	string,
	int32,
	time.Duration,
) (UploadTarget, error) {
	return UploadTarget{}, nil
}

func (*multipartCleanupBackend) CompleteMultipartUpload(
	context.Context,
	string,
	string,
	[]CompletedPart,
) error {
	return nil
}

func (b *multipartCleanupBackend) AbortMultipartUpload(ctx context.Context, _, _ string) error {
	b.abortCalls.Add(1)
	if b.abort != nil {
		return b.abort(ctx)
	}
	return b.abortErr
}

func backdatePendingUpload(t *testing.T, ctx context.Context, q *pgxpool.Pool, objectID int64) {
	t.Helper()
	if _, err := q.Exec(ctx, `
UPDATE storage_objects
SET updated_at = now() - interval '25 hours'
WHERE id = $1`, objectID); err != nil {
		t.Fatalf("backdate pending upload: %v", err)
	}
}

func backdateCleanupAttempt(t *testing.T, ctx context.Context, q *pgxpool.Pool, objectID int64) {
	t.Helper()
	if _, err := q.Exec(ctx, `
UPDATE storage_objects
SET expired_at = now() - interval '2 hours'
WHERE id = $1`, objectID); err != nil {
		t.Fatalf("backdate upload cleanup attempt: %v", err)
	}
}

func assertStorageObjectCount(
	t *testing.T,
	ctx context.Context,
	q *pgxpool.Pool,
	objectID int64,
	want int,
) {
	t.Helper()
	var count int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM storage_objects WHERE id = $1`, objectID).Scan(&count); err != nil {
		t.Fatalf("count storage object: %v", err)
	}
	if count != want {
		t.Fatalf("storage object count = %d, want %d", count, want)
	}
}

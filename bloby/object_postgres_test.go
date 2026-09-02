//go:build postgres

package bloby

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/woodleighschool/goodies/bloby/internal/testdb"
)

func TestListByPrefixReturnsAvailableObjectsNewestFirst(t *testing.T) {
	db, ctx := testdb.Open(t, PostgresSchema())
	store := NewObjectStore(db, nil, testLogger())

	first, err := store.CreatePending(ctx, "munki/icons", "first.png")
	if err != nil {
		t.Fatalf("create first object: %v", err)
	}
	if _, err := store.MarkAvailable(
		ctx,
		first.ID,
		1,
		"image/png",
		strings.Repeat("a", 64),
	); err != nil {
		t.Fatalf("finalize first object: %v", err)
	}
	second, err := store.CreatePending(ctx, "munki/icons", "second.png")
	if err != nil {
		t.Fatalf("create second object: %v", err)
	}
	if _, err := store.MarkAvailable(
		ctx,
		second.ID,
		1,
		"image/png",
		strings.Repeat("b", 64),
	); err != nil {
		t.Fatalf("finalize second object: %v", err)
	}
	if _, err := store.CreatePending(ctx, "munki/icons", "pending.png"); err != nil {
		t.Fatalf("create pending object: %v", err)
	}
	other, err := store.CreatePending(ctx, "munki/packages", "other.pkg")
	if err != nil {
		t.Fatalf("create other-prefix object: %v", err)
	}
	if _, err := store.MarkAvailable(
		ctx,
		other.ID,
		1,
		"application/octet-stream",
		strings.Repeat("c", 64),
	); err != nil {
		t.Fatalf("finalize other-prefix object: %v", err)
	}

	objects, count, err := store.ListByPrefix(ctx, "munki/icons", ListOptions{})
	if err != nil {
		t.Fatalf("list objects: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}
	if len(objects) != 2 || objects[0].ID != second.ID || objects[1].ID != first.ID {
		t.Fatalf("object IDs = %v, want [%d %d]", objectIDs(objects), second.ID, first.ID)
	}
}

func TestMarkAvailableNormalizesContentType(t *testing.T) {
	db, ctx := testdb.Open(t, PostgresSchema())
	store := NewObjectStore(db, nil, testLogger())
	object, err := store.CreatePending(ctx, "munki/icons", "icon.png")
	if err != nil {
		t.Fatalf("create pending object: %v", err)
	}

	available, err := store.MarkAvailable(
		ctx,
		object.ID,
		1,
		"IMAGE/PNG; profile=\"screen\"",
		strings.Repeat("a", 64),
	)
	if err != nil {
		t.Fatalf("mark available: %v", err)
	}
	if available.ContentType != "image/png; profile=screen" {
		t.Fatalf("content type = %q, want normalized media type", available.ContentType)
	}
}

func TestMultipartUploadMustBeClosedBeforeAvailability(t *testing.T) {
	db, ctx := testdb.Open(t, PostgresSchema())
	store := NewObjectStore(db, nil, testLogger())
	object, err := store.CreatePending(ctx, "munki/packages", "installer.pkg")
	if err != nil {
		t.Fatalf("create pending object: %v", err)
	}
	uploadID, created, err := store.RecordMultipartUploadID(ctx, object.ID, "upload-1")
	if err != nil {
		t.Fatalf("record multipart ID: %v", err)
	}
	if !created || uploadID != "upload-1" {
		t.Fatalf("recorded multipart = %q/%t, want upload-1/true", uploadID, created)
	}
	_, err = store.MarkAvailable(
		ctx,
		object.ID,
		1,
		"application/octet-stream",
		strings.Repeat("a", 64),
	)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("finalize open multipart error = %v, want ErrInvalidInput", err)
	}
	if err := store.ClearMultipartUploadID(ctx, object.ID, uploadID); err != nil {
		t.Fatalf("clear multipart ID: %v", err)
	}
	available, err := store.MarkAvailable(
		ctx,
		object.ID,
		1,
		"application/octet-stream",
		strings.Repeat("a", 64),
	)
	if err != nil {
		t.Fatalf("finalize closed multipart: %v", err)
	}
	if available.MultipartUploadID != nil {
		t.Fatalf("available multipart ID = %v, want nil", available.MultipartUploadID)
	}
}

func TestDeleteRemovesRegistryWhenBackendDeletionFails(t *testing.T) {
	db, ctx := testdb.Open(t, PostgresSchema())
	backend := &deletionBackend{err: errors.New("backend unavailable")}
	store := NewObjectStore(db, backend, testLogger())
	object, err := store.CreatePending(ctx, "munki/icons", "icon.png")
	if err != nil {
		t.Fatalf("create pending object: %v", err)
	}

	if err := store.Delete(ctx, object.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.GetByID(ctx, object.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted object error = %v, want ErrNotFound", err)
	}
	if backend.calls != 1 {
		t.Fatalf("backend delete calls = %d, want 1", backend.calls)
	}
}

func TestDeleteUnreferencedUsesDetachedContext(t *testing.T) {
	db, ctx := testdb.Open(t, PostgresSchema())
	backend := &deletionBackend{}
	store := NewObjectStore(db, backend, testLogger())
	object, err := store.CreatePending(ctx, "munki/icons", "icon.png")
	if err != nil {
		t.Fatalf("create pending object: %v", err)
	}
	requestCtx, cancelRequest := context.WithCancel(ctx)
	cancelRequest()

	store.DeleteUnreferenced(requestCtx, object.ID)
	if _, err := store.GetByID(ctx, object.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get object after cleanup error = %v, want ErrNotFound", err)
	}
	if backend.sawCanceledContext {
		t.Fatal("cleanup used the canceled request context")
	}
}

func TestDeleteConflictDoesNotScheduleReferencedObject(t *testing.T) {
	db, ctx := testdb.Open(t, PostgresSchema())
	backend := &deletionBackend{}
	store := NewObjectStore(db, backend, testLogger())
	object, err := store.CreatePending(ctx, "munki/icons", "icon.png")
	if err != nil {
		t.Fatalf("create pending object: %v", err)
	}
	object, err = store.MarkAvailable(ctx, object.ID, 1, "image/png", strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("mark object available: %v", err)
	}
	if _, err := db.Exec(ctx, `
CREATE TABLE object_references (
    object_id BIGINT PRIMARY KEY REFERENCES storage_objects(id) ON DELETE RESTRICT
);`); err != nil {
		t.Fatalf("create object reference table: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO object_references (object_id) VALUES ($1)`, object.ID); err != nil {
		t.Fatalf("reference object: %v", err)
	}
	if err := store.Delete(ctx, object.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete referenced object error = %v, want ErrConflict", err)
	}
	if _, err := store.GetByID(ctx, object.ID); err != nil {
		t.Fatalf("get referenced object after conflict: %v", err)
	}
	if backend.calls != 0 {
		t.Fatalf("backend delete calls = %d, want 0", backend.calls)
	}
	if _, err := db.Exec(ctx, `DELETE FROM object_references WHERE object_id = $1`, object.ID); err != nil {
		t.Fatalf("remove object reference: %v", err)
	}
	if err := store.Delete(ctx, object.ID); err != nil {
		t.Fatalf("delete after removing reference: %v", err)
	}
	if _, err := store.GetByID(ctx, object.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get object after final reference removal error = %v, want ErrNotFound", err)
	}
	if backend.calls != 1 {
		t.Fatalf("backend delete calls after final unlink = %d, want 1", backend.calls)
	}
}

type deletionBackend struct {
	err                error
	sawCanceledContext bool
	calls              int
}

func (b *deletionBackend) Delete(ctx context.Context, _ string) error {
	b.calls++
	b.sawCanceledContext = b.sawCanceledContext || ctx.Err() != nil
	return b.err
}

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func objectIDs(objects []Object) []int64 {
	ids := make([]int64, len(objects))
	for i, object := range objects {
		ids[i] = object.ID
	}
	return ids
}

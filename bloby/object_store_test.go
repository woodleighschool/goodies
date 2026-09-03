package bloby

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

type registryStub struct {
	createPending           func(context.Context, string, string) (*Object, error)
	markAvailable           func(context.Context, int64, int64, string, string) (*Object, error)
	refreshPending          func(context.Context, int64) (*Object, error)
	recordMultipartUploadID func(context.Context, int64, string) (string, bool, error)
	clearMultipartUploadID  func(context.Context, int64, string) error
	getByID                 func(context.Context, int64) (*Object, error)
	listByIDs               func(context.Context, []int64) (map[int64]Object, error)
	listByPrefix            func(context.Context, string, ListOptions) ([]Object, int, error)
	delete                  func(context.Context, int64) (*Object, error)
	claimExpiredPending     func(context.Context, time.Time, time.Time, int) ([]Object, error)
	deleteExpiredPending    func(context.Context, int64) error
}

func (r registryStub) CreatePending(ctx context.Context, prefix, filename string) (*Object, error) {
	return r.createPending(ctx, prefix, filename)
}

func (r registryStub) MarkAvailable(
	ctx context.Context,
	id int64,
	sizeBytes int64,
	contentType string,
	sha256sum string,
) (*Object, error) {
	return r.markAvailable(ctx, id, sizeBytes, contentType, sha256sum)
}

func (r registryStub) RefreshPending(ctx context.Context, id int64) (*Object, error) {
	return r.refreshPending(ctx, id)
}

func (r registryStub) RecordMultipartUploadID(
	ctx context.Context,
	id int64,
	uploadID string,
) (string, bool, error) {
	return r.recordMultipartUploadID(ctx, id, uploadID)
}

func (r registryStub) ClearMultipartUploadID(ctx context.Context, id int64, uploadID string) error {
	return r.clearMultipartUploadID(ctx, id, uploadID)
}

func (r registryStub) GetByID(ctx context.Context, id int64) (*Object, error) {
	return r.getByID(ctx, id)
}

func (r registryStub) ListByIDs(ctx context.Context, ids []int64) (map[int64]Object, error) {
	return r.listByIDs(ctx, ids)
}

func (r registryStub) ListByPrefix(
	ctx context.Context,
	prefix string,
	options ListOptions,
) ([]Object, int, error) {
	return r.listByPrefix(ctx, prefix, options)
}

func (r registryStub) Delete(ctx context.Context, id int64) (*Object, error) {
	return r.delete(ctx, id)
}

func (r registryStub) ClaimExpiredPending(
	ctx context.Context,
	updatedBefore time.Time,
	retryBefore time.Time,
	limit int,
) ([]Object, error) {
	return r.claimExpiredPending(ctx, updatedBefore, retryBefore, limit)
}

func (r registryStub) DeleteExpiredPending(ctx context.Context, id int64) error {
	return r.deleteExpiredPending(ctx, id)
}

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

type recordingDeleter struct {
	keys []string
}

func (d *recordingDeleter) Delete(_ context.Context, key string) error {
	d.keys = append(d.keys, key)
	return nil
}

func TestObjectStoreAppliesModelAtRegistryBoundary(t *testing.T) {
	object := &Object{ID: 42, Prefix: "munki/icons", Filename: "App.png"}
	detachedDelete := false
	registry := registryStub{
		createPending: func(_ context.Context, prefix, filename string) (*Object, error) {
			if prefix != object.Prefix || filename != object.Filename {
				t.Errorf("CreatePending(%q, %q)", prefix, filename)
			}
			return object, nil
		},
		markAvailable: func(
			_ context.Context,
			id int64,
			sizeBytes int64,
			contentType string,
			sha256sum string,
		) (*Object, error) {
			if id != object.ID || sizeBytes != 1025 || contentType != "image/png; profile=screen" || sha256sum != "sha256" {
				t.Errorf("MarkAvailable(%d, %d, %q, %q)", id, sizeBytes, contentType, sha256sum)
			}
			return object, nil
		},
		refreshPending: func(_ context.Context, id int64) (*Object, error) {
			if id != object.ID {
				t.Errorf("RefreshPending(%d)", id)
			}
			return object, nil
		},
		recordMultipartUploadID: func(_ context.Context, id int64, uploadID string) (string, bool, error) {
			if id != object.ID || uploadID != "upload-1" {
				t.Errorf("RecordMultipartUploadID(%d, %q)", id, uploadID)
			}
			return uploadID, true, nil
		},
		clearMultipartUploadID: func(_ context.Context, id int64, uploadID string) error {
			if id != object.ID || uploadID != "upload-1" {
				t.Errorf("ClearMultipartUploadID(%d, %q)", id, uploadID)
			}
			return nil
		},
		getByID: func(_ context.Context, id int64) (*Object, error) {
			if id != object.ID {
				t.Errorf("GetByID(%d)", id)
			}
			return object, nil
		},
		listByIDs: func(_ context.Context, ids []int64) (map[int64]Object, error) {
			if len(ids) != 1 || ids[0] != object.ID {
				t.Errorf("ListByIDs(%v)", ids)
			}
			return map[int64]Object{object.ID: *object}, nil
		},
		listByPrefix: func(_ context.Context, prefix string, options ListOptions) ([]Object, int, error) {
			if prefix != object.Prefix || options != (ListOptions{Limit: 50}) {
				t.Errorf("ListByPrefix(%q, %#v)", prefix, options)
			}
			return []Object{*object}, 1, nil
		},
		delete: func(ctx context.Context, id int64) (*Object, error) {
			if id == 43 {
				detachedDelete = ctx.Err() == nil
				return &Object{ID: id, Prefix: object.Prefix, Filename: "Other.png"}, nil
			}
			return object, nil
		},
		claimExpiredPending: func(_ context.Context, _, _ time.Time, limit int) ([]Object, error) {
			if limit != 7 {
				t.Errorf("ClaimExpiredPending limit = %d", limit)
			}
			return []Object{*object}, nil
		},
		deleteExpiredPending: func(_ context.Context, id int64) error {
			if id != object.ID {
				t.Errorf("DeleteExpiredPending(%d)", id)
			}
			return nil
		},
	}
	deleter := &recordingDeleter{}
	store := NewObjectStore(registry, deleter, testLogger())

	if _, err := store.CreatePending(t.Context(), object.Prefix, `folder\\App.png`); err != nil {
		t.Fatalf("CreatePending: %v", err)
	}
	if _, err := store.MarkAvailable(
		t.Context(), object.ID, 1025, `IMAGE/PNG; profile="screen"`, "sha256",
	); err != nil {
		t.Fatalf("MarkAvailable: %v", err)
	}
	if _, err := store.RefreshPending(t.Context(), object.ID); err != nil {
		t.Fatalf("RefreshPending: %v", err)
	}
	if _, _, err := store.RecordMultipartUploadID(t.Context(), object.ID, " upload-1 "); err != nil {
		t.Fatalf("RecordMultipartUploadID: %v", err)
	}
	if err := store.ClearMultipartUploadID(t.Context(), object.ID, "upload-1"); err != nil {
		t.Fatalf("ClearMultipartUploadID: %v", err)
	}
	if _, err := store.GetByID(t.Context(), object.ID); err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if _, err := store.ListByIDs(t.Context(), []int64{object.ID}); err != nil {
		t.Fatalf("ListByIDs: %v", err)
	}
	if _, _, err := store.ListByPrefix(t.Context(), object.Prefix, ListOptions{}); err != nil {
		t.Fatalf("ListByPrefix: %v", err)
	}
	if err := store.Delete(t.Context(), object.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	store.DeleteUnreferenced(canceled, 43)
	if !detachedDelete {
		t.Fatal("DeleteUnreferenced used canceled request context")
	}
	if len(deleter.keys) != 2 {
		t.Fatalf("deleted keys = %v", deleter.keys)
	}
	if _, err := store.claimExpiredPending(t.Context(), time.Now(), time.Now(), 7); err != nil {
		t.Fatalf("claimExpiredPending: %v", err)
	}
	if err := store.deleteExpiredPending(t.Context(), object.ID); err != nil {
		t.Fatalf("deleteExpiredPending: %v", err)
	}
}

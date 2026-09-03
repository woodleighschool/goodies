package bloby

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestIngestorDirectLifecycle(t *testing.T) {
	object := &Object{ID: 42, Prefix: "munki/packages", Filename: "Installer.pkg"}
	var markedSize int64
	registry := registryStub{
		createPending: func(_ context.Context, prefix, filename string) (*Object, error) {
			if prefix != object.Prefix || filename != object.Filename {
				t.Errorf("CreatePending(%q, %q)", prefix, filename)
			}
			return object, nil
		},
		getByID: func(_ context.Context, _ int64) (*Object, error) {
			return object, nil
		},
		refreshPending: func(_ context.Context, _ int64) (*Object, error) {
			return object, nil
		},
		markAvailable: func(
			_ context.Context,
			_ int64,
			sizeBytes int64,
			contentType string,
			_ string,
		) (*Object, error) {
			markedSize = sizeBytes
			now := time.Now()
			available := *object
			available.ContentType = contentType
			available.AvailableAt = &now
			return &available, nil
		},
	}
	backend := newTestFileStore(t)
	ingestor := NewIngestor(NewObjectStore(registry, backend, testLogger()), backend)

	created, action, err := ingestor.Begin(t.Context(), object.Prefix, object.Filename, 10)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	direct, ok := action.(DirectUploadAction)
	if !ok || direct.Target.Method != http.MethodPut || created.ID != object.ID {
		t.Fatalf("Begin = %#v, %#v", created, action)
	}
	if _, err := ingestor.Finalize(t.Context(), object.ID, "munki/icons"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Finalize with wrong prefix = %v", err)
	}
	if err := backend.Put(t.Context(), object.Key(), strings.NewReader("installer"), PutOptions{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	available, err := ingestor.Finalize(t.Context(), object.ID, object.Prefix)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if !available.Available() || markedSize != int64(len("installer")) {
		t.Fatalf("available = %#v, marked size = %d", available, markedSize)
	}
}

func TestIngestorDirectOperations(t *testing.T) {
	t.Run("presign and delete", func(t *testing.T) {
		object := &Object{ID: 42, Prefix: "munki/packages", Filename: "Direct.pkg"}
		deleted := false
		registry := registryStub{
			createPending: func(context.Context, string, string) (*Object, error) {
				return object, nil
			},
			getByID: func(context.Context, int64) (*Object, error) {
				return object, nil
			},
			refreshPending: func(context.Context, int64) (*Object, error) {
				return object, nil
			},
			delete: func(context.Context, int64) (*Object, error) {
				deleted = true
				return object, nil
			},
		}
		backend := newTestFileStore(t)
		ingestor := NewIngestor(NewObjectStore(registry, backend, testLogger()), backend)

		created, target, err := ingestor.BeginDirect(t.Context(), object.Prefix, object.Filename)
		if err != nil || target.Method != http.MethodPut || created.ID != object.ID {
			t.Fatalf("BeginDirect = %#v, %#v, %v", created, target, err)
		}
		if err := ingestor.Delete(t.Context(), object.ID, object.Prefix); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if !deleted {
			t.Fatal("Delete did not remove the registry object")
		}
	})

	t.Run("server write", func(t *testing.T) {
		object := &Object{ID: 43, Prefix: "munki/catalogues", Filename: "production.json"}
		registry := registryStub{
			createPending: func(context.Context, string, string) (*Object, error) {
				return object, nil
			},
			markAvailable: func(
				_ context.Context,
				_ int64,
				_ int64,
				_ string,
				_ string,
			) (*Object, error) {
				now := time.Now()
				available := *object
				available.AvailableAt = &now
				return &available, nil
			},
		}
		backend := newTestFileStore(t)
		ingestor := NewIngestor(NewObjectStore(registry, backend, testLogger()), backend)

		written, err := ingestor.Write(
			t.Context(), object.Prefix, object.Filename, "application/json", []byte(`{"name":"production"}`),
		)
		if err != nil || !written.Available() {
			t.Fatalf("Write = %#v, %v", written, err)
		}
	})
}

func TestIngestorMultipartLifecycle(t *testing.T) {
	object := &Object{ID: 42, Prefix: "munki/packages", Filename: "Installer.pkg"}
	registry := registryStub{
		createPending: func(context.Context, string, string) (*Object, error) {
			return object, nil
		},
		getByID: func(context.Context, int64) (*Object, error) {
			return object, nil
		},
		refreshPending: func(context.Context, int64) (*Object, error) {
			return object, nil
		},
		recordMultipartUploadID: func(_ context.Context, _ int64, uploadID string) (string, bool, error) {
			object.MultipartUploadID = &uploadID
			return uploadID, true, nil
		},
		clearMultipartUploadID: func(context.Context, int64, string) error {
			object.MultipartUploadID = nil
			return nil
		},
		markAvailable: func(
			_ context.Context,
			_ int64,
			_ int64,
			_ string,
			_ string,
		) (*Object, error) {
			now := time.Now()
			available := *object
			available.AvailableAt = &now
			return &available, nil
		},
	}
	backend := &recordingMultipartBackend{Backend: newTestFileStore(t)}
	ingestor := NewIngestor(NewObjectStore(registry, backend, testLogger()), backend)

	created, action, err := ingestor.Begin(t.Context(), object.Prefix, object.Filename, 1)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, ok := action.(MultipartUploadAction); !ok || created.ID != object.ID {
		t.Fatalf("Begin = %#v, %T", created, action)
	}
	if _, err := ingestor.PresignMultipartPart(t.Context(), object.ID, object.Prefix, 7); err != nil {
		t.Fatalf("PresignMultipartPart: %v", err)
	}
	if err := ingestor.CompleteMultipart(
		t.Context(), object.ID, object.Prefix, []CompletedPart{{PartNumber: 1, ETag: `"part"`}},
	); err != nil {
		t.Fatalf("CompleteMultipart: %v", err)
	}
	available, err := ingestor.Finalize(t.Context(), object.ID, object.Prefix)
	if err != nil || !available.Available() {
		t.Fatalf("Finalize = %#v, %v", available, err)
	}
	if backend.presignCalls != 1 || backend.completed != 1 {
		t.Fatalf("multipart calls = presign %d, complete %d", backend.presignCalls, backend.completed)
	}
}

func TestIngestorDeleteAbortsMultipart(t *testing.T) {
	uploadID := "upload-1"
	object := &Object{
		ID: 42, Prefix: "munki/packages", Filename: "Installer.pkg", MultipartUploadID: &uploadID,
	}
	registry := registryStub{
		getByID: func(context.Context, int64) (*Object, error) {
			return object, nil
		},
		refreshPending: func(context.Context, int64) (*Object, error) {
			return object, nil
		},
		delete: func(context.Context, int64) (*Object, error) {
			return object, nil
		},
	}
	backend := &recordingMultipartBackend{Backend: newTestFileStore(t)}
	ingestor := NewIngestor(NewObjectStore(registry, backend, testLogger()), backend)

	if err := ingestor.Delete(t.Context(), object.ID, object.Prefix); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if backend.aborted != 1 {
		t.Fatalf("abort calls = %d, want 1", backend.aborted)
	}
}

func TestUploadCleanupRemovesAbandonedUpload(t *testing.T) {
	object := Object{ID: 42, Prefix: "munki/packages", Filename: "Partial.pkg"}
	deleted := make(chan struct{})
	registry := registryStub{
		claimExpiredPending: func(context.Context, time.Time, time.Time, int) ([]Object, error) {
			return []Object{object}, nil
		},
		deleteExpiredPending: func(context.Context, int64) error {
			close(deleted)
			return nil
		},
	}
	backend := newTestFileStore(t)
	if err := backend.Put(t.Context(), object.Key(), strings.NewReader("partial"), PutOptions{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	ingestor := NewIngestor(NewObjectStore(registry, backend, testLogger()), backend)

	cleanup := StartUploadCleanup(t.Context(), ingestor, time.Minute, testLogger())
	select {
	case <-deleted:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not remove abandoned upload")
	}
	cleanup.Stop()
	if _, _, err := backend.Open(t.Context(), object.Key()); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("Open after cleanup = %v", err)
	}
}

type recordingMultipartBackend struct {
	Backend

	presignCalls int
	completed    int
	aborted      int
}

func (*recordingMultipartBackend) beginUpload(context.Context, string, int64) (UploadAction, error) {
	return MultipartUploadAction{}, nil
}

func (*recordingMultipartBackend) CreateMultipartUpload(context.Context, string) (string, error) {
	return "upload-1", nil
}

func (b *recordingMultipartBackend) PresignMultipartPart(
	_ context.Context,
	_ string,
	_ string,
	_ int32,
	_ time.Duration,
) (UploadTarget, error) {
	b.presignCalls++
	return UploadTarget{URL: "https://storage.invalid/upload", Method: http.MethodPut}, nil
}

func (b *recordingMultipartBackend) CompleteMultipartUpload(
	ctx context.Context,
	key string,
	_ string,
	_ []CompletedPart,
) error {
	b.completed++
	return b.Put(ctx, key, strings.NewReader("assembled"), PutOptions{})
}

func (b *recordingMultipartBackend) AbortMultipartUpload(context.Context, string, string) error {
	b.aborted++
	return nil
}

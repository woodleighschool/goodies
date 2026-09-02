//go:build postgres

package bloby

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/woodleighschool/goodies/bloby/internal/capability"
	"github.com/woodleighschool/goodies/bloby/internal/testdb"
)

func TestDirectUploadTargetsPendingObjectKey(t *testing.T) {
	db, ctx := testdb.Open(t, PostgresSchema())
	backend := newTestBackend(t)
	objects := NewObjectStore(db, backend, testLogger())
	uploads := NewIngestor(objects, backend)
	object, target, err := uploads.BeginDirect(ctx, "munki/packages", "Installer.pkg")
	if err != nil {
		t.Fatalf("begin upload: %v", err)
	}
	parsed, err := url.Parse(target.URL)
	if err != nil {
		t.Fatalf("parse upload URL: %v", err)
	}
	claims, err := capability.Verify[BlobCapabilityClaims](
		testCapabilityKey,
		parsed.Query().Get("cap"),
		capability.OpPut,
		time.Now(),
	)
	if err != nil {
		t.Fatalf("verify upload capability: %v", err)
	}
	if claims.Key != object.Key() {
		t.Fatalf("upload key = %q, want object key %q", claims.Key, object.Key())
	}

	body := []byte("installer bytes")
	if err := backend.Put(ctx, object.Key(), bytes.NewReader(body), PutOptions{}); err != nil {
		t.Fatalf("put upload bytes: %v", err)
	}
	finalized, err := uploads.Finalize(ctx, object.ID, object.Prefix)
	if err != nil {
		t.Fatalf("finalize upload: %v", err)
	}
	if !finalized.Available() {
		t.Fatal("finalized object is still pending")
	}
}

func TestMultipartSigningUsesRecordedObjectUpload(t *testing.T) {
	db, ctx := testdb.Open(t, PostgresSchema())
	backend := &recordingMultipartBackend{Backend: newTestBackend(t)}
	objects := NewObjectStore(db, backend, testLogger())
	uploads := NewIngestor(objects, backend)

	unrecorded, _, err := uploads.BeginDirect(ctx, "munki/packages", "Direct.pkg")
	if err != nil {
		t.Fatalf("begin direct upload: %v", err)
	}
	first, firstAction, err := uploads.Begin(ctx, "munki/packages", "First.pkg", 1)
	if err != nil {
		t.Fatalf("begin first multipart upload: %v", err)
	}
	_, ok := firstAction.(MultipartUploadAction)
	if !ok {
		t.Fatalf("first upload action = %T, want MultipartUploadAction", firstAction)
	}

	for _, tt := range []struct {
		name     string
		objectID int64
		prefix   string
	}{
		{name: "unrecorded upload", objectID: unrecorded.ID, prefix: unrecorded.Prefix},
		{name: "wrong prefix", objectID: first.ID, prefix: "munki/icons"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := uploads.PresignMultipartPart(ctx, tt.objectID, tt.prefix, 1)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("presign error = %v, want ErrInvalidInput", err)
			}
		})
	}
	if backend.presignCalls != 0 {
		t.Fatalf("backend presign calls after rejected requests = %d, want 0", backend.presignCalls)
	}

	if _, err := uploads.PresignMultipartPart(ctx, first.ID, first.Prefix, 7); err != nil {
		t.Fatalf("presign recorded upload: %v", err)
	}
	if backend.presignCalls != 1 || backend.presignedKey != first.Key() ||
		backend.presignedUploadID != "upload-1" || backend.presignedPartNumber != 7 {
		t.Fatalf(
			"backend presign = %d/%q/%q/%+v, want first object's recorded upload",
			backend.presignCalls,
			backend.presignedKey,
			backend.presignedUploadID,
			backend.presignedPartNumber,
		)
	}
}

type recordingMultipartBackend struct {
	Backend

	created             int
	presignCalls        int
	presignedKey        string
	presignedUploadID   string
	presignedPartNumber int32
}

func (*recordingMultipartBackend) beginUpload(context.Context, string, int64) (UploadAction, error) {
	return MultipartUploadAction{}, nil
}

func (b *recordingMultipartBackend) CreateMultipartUpload(context.Context, string) (string, error) {
	b.created++
	return fmt.Sprintf("upload-%d", b.created), nil
}

func (b *recordingMultipartBackend) PresignMultipartPart(
	_ context.Context,
	key string,
	uploadID string,
	partNumber int32,
	_ time.Duration,
) (UploadTarget, error) {
	b.presignCalls++
	b.presignedKey = key
	b.presignedUploadID = uploadID
	b.presignedPartNumber = partNumber
	return UploadTarget{URL: "https://storage.invalid/upload", Method: http.MethodPut}, nil
}

func (*recordingMultipartBackend) CompleteMultipartUpload(
	context.Context,
	string,
	string,
	[]CompletedPart,
) error {
	return nil
}

func (*recordingMultipartBackend) AbortMultipartUpload(context.Context, string, string) error {
	return nil
}

func newTestBackend(t *testing.T) Backend {
	t.Helper()
	backend, err := New(t.Context(), Config{
		Kind:        KindFile,
		TransferTTL: time.Minute,
		File: FileConfig{
			Root:             t.TempDir(),
			BaseURL:          "https://woodstar.example",
			CapabilityKeyHex: hex.EncodeToString(bytes.Repeat([]byte{0x42}, 32)),
		},
	})
	if err != nil {
		t.Fatalf("create file storage: %v", err)
	}
	return backend
}

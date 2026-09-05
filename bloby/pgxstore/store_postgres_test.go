package pgxstore_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/woodleighschool/goodies/bloby"
	"github.com/woodleighschool/goodies/bloby/pgxstore"
)

func TestRegistryLifecycleAndListing(t *testing.T) {
	db, ctx := openTestDatabase(t)
	objects := pgxstore.New(db)

	first, err := objects.CreatePending(ctx, "documents/reports", "first.pdf")
	if err != nil {
		t.Fatalf("create first object: %v", err)
	}
	if _, err := objects.MarkAvailable(ctx, first.ID, 10, "application/pdf", strings.Repeat("a", 64), fmt.Sprintf("_objects/%d/candidate", first.ID)); err != nil {
		t.Fatalf("mark first object available: %v", err)
	}

	second, err := objects.CreatePending(ctx, "documents/reports", "second.pdf")
	if err != nil {
		t.Fatalf("create second object: %v", err)
	}
	if _, err := objects.MarkAvailable(ctx, second.ID, 20, "application/pdf", strings.Repeat("b", 64), fmt.Sprintf("_objects/%d/candidate", second.ID)); err != nil {
		t.Fatalf("mark second object available: %v", err)
	}

	items, count, err := objects.ListByPrefix(ctx, "documents/reports", bloby.ListOptions{Limit: 50})
	if err != nil {
		t.Fatalf("list objects: %v", err)
	}
	if count != 2 || len(items) != 2 || items[0].ID != second.ID || items[1].ID != first.ID {
		t.Fatalf("list = %#v, count = %d", items, count)
	}
}

func TestRegistryDeletePreservesReferencedObject(t *testing.T) {
	db, ctx := openTestDatabase(t)
	objects := pgxstore.New(db)
	object, err := objects.CreatePending(ctx, "documents/reports", "report.pdf")
	if err != nil {
		t.Fatalf("create object: %v", err)
	}
	if _, err := db.Exec(ctx, `CREATE TABLE object_references (
        object_id BIGINT PRIMARY KEY REFERENCES storage_objects(id) ON DELETE RESTRICT
    )`); err != nil {
		t.Fatalf("create reference table: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO object_references (object_id) VALUES ($1)`, object.ID); err != nil {
		t.Fatalf("reference object: %v", err)
	}

	if _, err := objects.Delete(ctx, object.ID); !errors.Is(err, bloby.ErrConflict) {
		t.Fatalf("delete error = %v, want ErrConflict", err)
	}
	if _, err := objects.GetByID(ctx, object.ID); err != nil {
		t.Fatalf("get referenced object: %v", err)
	}
}

func TestRegistryClaimsAbandonedPendingObjects(t *testing.T) {
	db, ctx := openTestDatabase(t)
	registry := pgxstore.New(db)
	object, err := registry.CreatePending(ctx, "documents/reports", "report.pdf")
	if err != nil {
		t.Fatalf("create object: %v", err)
	}
	if _, err := db.Exec(ctx, `UPDATE storage_objects SET updated_at = now() - interval '25 hours' WHERE id = $1`, object.ID); err != nil {
		t.Fatalf("backdate object: %v", err)
	}

	claimed, err := registry.ClaimExpiredPending(ctx, time.Now().Add(-24*time.Hour), time.Now().Add(-time.Hour), 100)
	if err != nil {
		t.Fatalf("claim abandoned objects: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != object.ID {
		t.Fatalf("claimed objects = %#v", claimed)
	}
	if err := registry.DeleteExpiredPending(ctx, object.ID); err != nil {
		t.Fatalf("delete claimed object: %v", err)
	}
}

func TestRegistryPublishIsOneWayAndIdempotent(t *testing.T) {
	db, ctx := openTestDatabase(t)
	registry := pgxstore.New(db)
	object, err := registry.CreatePending(ctx, "documents/reports", "report.txt")
	if err != nil {
		t.Fatal(err)
	}
	first, err := registry.MarkAvailable(ctx, object.ID, 7, "text/plain", strings.Repeat("a", 64), fmt.Sprintf("_objects/%d/candidate", object.ID))
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.MarkAvailable(ctx, object.ID, 999, "image/png", strings.Repeat("b", 64), fmt.Sprintf("_objects/%d/loser", object.ID))
	if err != nil {
		t.Fatal(err)
	}
	if second.Key() != first.Key() || second.SizeBytesValue() != first.SizeBytesValue() || second.SHA256Value() != first.SHA256Value() || second.ContentType != first.ContentType || !second.AvailableAt.Equal(*first.AvailableAt) || !second.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("published metadata changed: first=%#v second=%#v", first, second)
	}
	if _, err := registry.RefreshPending(ctx, object.ID); !errors.Is(err, bloby.ErrNotFound) {
		t.Fatalf("refresh available: %v", err)
	}
	if err := registry.RecordMultipartUploadID(ctx, object.ID, "upload-after-finalize"); !errors.Is(err, bloby.ErrNotFound) {
		t.Fatalf("multipart after publication: %v", err)
	}
}

func TestRegistryMultipartMustCompleteBeforePublication(t *testing.T) {
	db, ctx := openTestDatabase(t)
	registry := pgxstore.New(db)
	object, err := registry.CreatePending(ctx, "documents/reports", "report.txt")
	if err != nil {
		t.Fatal(err)
	}
	const uploadID = "provider-upload"
	if err := registry.RecordMultipartUploadID(ctx, object.ID, uploadID); err != nil {
		t.Fatal(err)
	}
	if err := registry.RecordMultipartUploadID(ctx, object.ID, "replacement"); !errors.Is(err, bloby.ErrNotFound) {
		t.Fatalf("replace existing multipart upload: %v", err)
	}
	if err := registry.ClearMultipartUploadID(ctx, object.ID, "wrong-id"); !errors.Is(err, bloby.ErrConflict) {
		t.Fatalf("clear different upload: %v", err)
	}
	if _, err := registry.MarkAvailable(ctx, object.ID, 5, "text/plain", strings.Repeat("a", 64), fmt.Sprintf("_objects/%d/candidate", object.ID)); !errors.Is(err, bloby.ErrInvalidInput) {
		t.Fatalf("publish unassembled multipart: %v", err)
	}
	if err := registry.ClearMultipartUploadID(ctx, object.ID, uploadID); err != nil {
		t.Fatal(err)
	}
	if err := registry.ClearMultipartUploadID(ctx, object.ID, uploadID); err != nil {
		t.Fatalf("clear retry: %v", err)
	}
}

func TestRegistryExpiryAndPublishCannotBothWin(t *testing.T) {
	db, ctx := openTestDatabase(t)
	registry := pgxstore.New(db)
	for range 12 {
		object, err := registry.CreatePending(ctx, "documents/reports", "report.txt")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `UPDATE storage_objects SET updated_at=now()-interval '48 hours' WHERE id=$1`, object.ID); err != nil {
			t.Fatal(err)
		}
		ready := make(chan struct{})
		published := make(chan error, 1)
		claimed := make(chan []bloby.Object, 1)
		failures := make(chan error, 1)
		go func() {
			<-ready
			_, err := registry.MarkAvailable(ctx, object.ID, 5, "text/plain", strings.Repeat("a", 64), fmt.Sprintf("_objects/%d/candidate", object.ID))
			published <- err
		}()
		go func() {
			<-ready
			objects, err := registry.ClaimExpiredPending(ctx, time.Now().Add(-24*time.Hour), time.Now().Add(-time.Hour), 100)
			claimed <- objects
			failures <- err
		}()
		close(ready)
		publishErr, claims, claimErr := <-published, <-claimed, <-failures
		if claimErr != nil {
			t.Fatal(claimErr)
		}
		if publishErr == nil {
			if len(claims) != 0 {
				t.Fatalf("published object was also claimed: %#v", claims)
			}
		} else {
			if !errors.Is(publishErr, bloby.ErrNotFound) || len(claims) != 1 || claims[0].ID != object.ID {
				t.Fatalf("publish=%v claim=%#v", publishErr, claims)
			}
			if _, err := registry.GetByID(ctx, object.ID); !errors.Is(err, bloby.ErrNotFound) {
				t.Fatalf("expired object visible: %v", err)
			}
			if _, err := registry.RefreshPending(ctx, object.ID); !errors.Is(err, bloby.ErrNotFound) {
				t.Fatalf("expired object refreshed: %v", err)
			}
			if err := registry.DeleteExpiredPending(ctx, object.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestServiceReferencedDeletePreservesBytes(t *testing.T) {
	db, ctx := openTestDatabase(t)
	service, err := bloby.New(ctx, pgxstore.New(db), bloby.Config{Kind: bloby.KindFile, TransferTTL: time.Minute, File: bloby.FileConfig{Root: t.TempDir(), BaseURL: "https://storage.invalid", CapabilityKeyHex: strings.Repeat("42", 32)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	object, err := service.Write(ctx, "documents/reports", "report.txt", "text/plain", []byte("referenced bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `CREATE TABLE object_references (object_id BIGINT PRIMARY KEY REFERENCES storage_objects(id) ON DELETE RESTRICT);`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO object_references VALUES ($1)`, object.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Delete(ctx, object.ID, object.Prefix); !errors.Is(err, bloby.ErrConflict) {
		t.Fatalf("referenced deletion: %v", err)
	}
	reader, err := service.Open(ctx, *object)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "referenced bytes" {
		t.Fatalf("bytes %q", body)
	}
}

type uncertainWriteRegistry struct {
	bloby.Registry
	db *pgxpool.Pool
}

func (r uncertainWriteRegistry) MarkAvailable(ctx context.Context, id, size int64, contentType, hash, key string) (*bloby.Object, error) {
	object, err := r.Registry.MarkAvailable(ctx, id, size, contentType, hash, key)
	if err != nil {
		return nil, err
	}
	// Another observer can see an available row before the publishing request
	// receives its result and attach the object through a real foreign key.
	if _, err := r.db.Exec(ctx, `INSERT INTO object_references VALUES ($1)`, object.ID); err != nil {
		return nil, err
	}
	return nil, errors.New("publish response lost after commit")
}

func TestWriteUncertainCommitPreservesNewlyReferencedBytes(t *testing.T) {
	db, ctx := openTestDatabase(t)
	if _, err := db.Exec(ctx, `CREATE TABLE object_references (object_id BIGINT PRIMARY KEY REFERENCES storage_objects(id) ON DELETE RESTRICT)`); err != nil {
		t.Fatal(err)
	}
	registry := pgxstore.New(db)
	service, err := bloby.New(ctx, uncertainWriteRegistry{Registry: registry, db: db}, bloby.Config{Kind: bloby.KindFile, TransferTTL: time.Minute, File: bloby.FileConfig{Root: t.TempDir(), BaseURL: "https://storage.invalid", CapabilityKeyHex: strings.Repeat("42", 32)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Write(ctx, "documents/reports", "report.txt", "text/plain", []byte("referenced after commit")); err == nil {
		t.Fatal("expected uncertain publish result")
	}
	objects, count, err := service.ListByPrefix(ctx, "documents/reports", bloby.ListOptions{})
	if err != nil || count != 1 || len(objects) != 1 {
		t.Fatalf("referenced row lost: %v %d %v", objects, count, err)
	}
	reader, err := service.Open(ctx, objects[0])
	if err != nil {
		t.Fatalf("referenced bytes lost: %v", err)
	}
	defer func() { _ = reader.Close() }()
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "referenced after commit" {
		t.Fatalf("referenced bytes %q", body)
	}
}

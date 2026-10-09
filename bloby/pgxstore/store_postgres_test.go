package pgxstore_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/woodleighschool/goodies/bloby"
	"github.com/woodleighschool/goodies/bloby/pgxstore"
)

// declare returns the declaration an uploader makes for body.
func declare(t testing.TB, body string) bloby.Content {
	t.Helper()
	content, err := bloby.Digest(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func objectKey(object *bloby.Object) string {
	return fmt.Sprintf("_objects/%d/%s", object.ID, object.Filename)
}

func newFileService(t testing.TB, ctx context.Context, registry bloby.Registry) *bloby.Service {
	t.Helper()
	service, err := bloby.New(ctx, registry, bloby.Config{Kind: bloby.KindFile, TransferTTL: time.Minute, File: bloby.FileConfig{Root: t.TempDir(), BaseURL: "https://storage.invalid", CapabilityKeyHex: strings.Repeat("42", 32)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func readObject(t testing.TB, ctx context.Context, service *bloby.Service, object bloby.Object) string {
	t.Helper()
	reader, err := service.Open(ctx, object)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestRegistryLifecycleAndListing(t *testing.T) {
	db, ctx := openTestDatabase(t)
	objects := pgxstore.New(db)
	content := declare(t, "first report")

	first, err := objects.CreatePending(ctx, "documents/reports", "first.pdf", content)
	if err != nil {
		t.Fatalf("create first object: %v", err)
	}
	if first.Available() || first.Key() != "" || first.ContentType != "" || first.CRC64NVME == nil ||
		first.SizeBytesValue() != content.SizeBytes || first.SHA256Value() != content.SHA256 || *first.CRC64NVME != content.CRC64NVME {
		t.Fatalf("pending object does not carry its declaration: %#v", first)
	}
	if items, count, err := objects.ListByPrefix(ctx, "documents/reports", bloby.ListOptions{Limit: 50}); err != nil || count != 0 || len(items) != 0 {
		t.Fatalf("pending object listed: %#v, count = %d, %v", items, count, err)
	}
	published, err := objects.MarkAvailable(ctx, first.ID, "application/pdf", objectKey(first))
	if err != nil {
		t.Fatalf("mark first object available: %v", err)
	}
	if !published.Available() || published.Key() != objectKey(first) || published.ContentType != "application/pdf" || published.CRC64NVME == nil ||
		published.SizeBytesValue() != content.SizeBytes || published.SHA256Value() != content.SHA256 || *published.CRC64NVME != content.CRC64NVME {
		t.Fatalf("published object does not carry its declaration: %#v", published)
	}

	second, err := objects.CreatePending(ctx, "documents/reports", "second.pdf", declare(t, "second report"))
	if err != nil {
		t.Fatalf("create second object: %v", err)
	}
	if _, err := objects.MarkAvailable(ctx, second.ID, "application/pdf", objectKey(second)); err != nil {
		t.Fatalf("mark second object available: %v", err)
	}

	items, count, err := objects.ListByPrefix(ctx, "documents/reports", bloby.ListOptions{Limit: 50})
	if err != nil {
		t.Fatalf("list objects: %v", err)
	}
	if count != 2 || len(items) != 2 || items[0].ID != second.ID || items[1].ID != first.ID {
		t.Fatalf("list = %#v, count = %d", items, count)
	}
	byID, err := objects.ListByIDs(ctx, []int64{first.ID, second.ID, second.ID + 1})
	if err != nil || len(byID) != 2 || byID[first.ID].Key() != objectKey(first) {
		t.Fatalf("objects by ID = %#v, %v", byID, err)
	}
}

func TestRegistryDeletePreservesReferencedObject(t *testing.T) {
	db, ctx := openTestDatabase(t)
	objects := pgxstore.New(db)
	object, err := objects.CreatePending(ctx, "documents/reports", "report.pdf", declare(t, "report"))
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

func TestRegistryListsUnreferencedAvailableObjectsByPrefix(t *testing.T) {
	db, ctx := openTestDatabase(t)
	registry := pgxstore.New(db)
	// Consumers own the referencing tables, under any names and columns.
	for _, table := range []string{
		`CREATE TABLE packages (installer_object_id BIGINT REFERENCES storage_objects(id) ON DELETE RESTRICT)`,
		`CREATE TABLE "Software Titles" ("Icon Object" BIGINT REFERENCES storage_objects(id) ON DELETE RESTRICT)`,
	} {
		if _, err := db.Exec(ctx, table); err != nil {
			t.Fatalf("create reference table: %v", err)
		}
	}
	publish := func(prefix, name string, age time.Duration) int64 {
		t.Helper()
		object, err := registry.CreatePending(ctx, prefix, name, declare(t, name))
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if _, err := registry.MarkAvailable(ctx, object.ID, "application/octet-stream", objectKey(object)); err != nil {
			t.Fatalf("publish %s: %v", name, err)
		}
		if _, err := db.Exec(ctx, `UPDATE storage_objects SET available_at = now() - $2::interval WHERE id = $1`, object.ID, age.String()); err != nil {
			t.Fatalf("backdate %s: %v", name, err)
		}
		return object.ID
	}
	abandoned := publish("munki/installers", "abandoned.pkg", 48*time.Hour)
	attached := publish("munki/installers", "attached.pkg", 48*time.Hour)
	publish("munki/installers", "recent.pkg", time.Hour)
	icon := publish("munki/icons", "attached.png", 48*time.Hour)
	publish("munki/library", "browsable.png", 48*time.Hour)
	if _, err := registry.CreatePending(ctx, "munki/installers", "pending.pkg", declare(t, "pending")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO packages VALUES ($1)`, attached); err != nil {
		t.Fatalf("reference installer: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO "Software Titles" VALUES ($1)`, icon); err != nil {
		t.Fatalf("reference icon: %v", err)
	}

	objects, err := registry.ListUnreferenced(ctx, []string{"munki/installers", "munki/icons"}, time.Now().Add(-24*time.Hour), 100)
	if err != nil {
		t.Fatalf("list unreferenced objects: %v", err)
	}
	if len(objects) != 1 || objects[0].ID != abandoned {
		t.Fatalf("unreferenced objects = %#v, want only %d", objects, abandoned)
	}
}

func TestRegistryClaimsAbandonedPendingObjects(t *testing.T) {
	db, ctx := openTestDatabase(t)
	registry := pgxstore.New(db)
	content := declare(t, "report")
	object, err := registry.CreatePending(ctx, "documents/reports", "report.pdf", content)
	if err != nil {
		t.Fatalf("create object: %v", err)
	}
	if err := registry.RecordMultipartUploadID(ctx, object.ID, "provider-upload"); err != nil {
		t.Fatal(err)
	}
	active, err := registry.CreatePending(ctx, "documents/reports", "active.pdf", content)
	if err != nil {
		t.Fatalf("create active object: %v", err)
	}
	if _, err := db.Exec(ctx, `UPDATE storage_objects SET updated_at = now() - interval '25 hours' WHERE id = $1`, object.ID); err != nil {
		t.Fatalf("backdate object: %v", err)
	}
	claim := func() []bloby.Object {
		t.Helper()
		claimed, err := registry.ClaimExpiredPending(ctx, time.Now().Add(-24*time.Hour), time.Now().Add(-time.Hour), 100)
		if err != nil {
			t.Fatalf("claim abandoned objects: %v", err)
		}
		return claimed
	}

	claimed := claim()
	// Cleanup removes the bytes a claim names, so it needs the filename and upload.
	if len(claimed) != 1 || claimed[0].ID != object.ID || claimed[0].Filename != "report.pdf" ||
		claimed[0].MultipartUploadID == nil || *claimed[0].MultipartUploadID != "provider-upload" {
		t.Fatalf("claimed objects = %#v", claimed)
	}
	if _, err := registry.GetByID(ctx, object.ID); !errors.Is(err, bloby.ErrNotFound) {
		t.Fatalf("claimed object visible: %v", err)
	}
	if _, err := registry.MarkAvailable(ctx, object.ID, "application/pdf", objectKey(object)); !errors.Is(err, bloby.ErrNotFound) {
		t.Fatalf("claimed object published: %v", err)
	}
	if again := claim(); len(again) != 0 {
		t.Fatalf("claim repeated before its retry delay: %#v", again)
	}
	if _, err := db.Exec(ctx, `UPDATE storage_objects SET expired_at = now() - interval '2 hours' WHERE id = $1`, object.ID); err != nil {
		t.Fatalf("backdate claim: %v", err)
	}
	if again := claim(); len(again) != 1 || again[0].ID != object.ID {
		t.Fatalf("failed cleanup not retried: %#v", again)
	}
	if err := registry.DeleteExpiredPending(ctx, object.ID); err != nil {
		t.Fatalf("delete claimed object: %v", err)
	}
	if err := registry.DeleteExpiredPending(ctx, active.ID); !errors.Is(err, bloby.ErrNotFound) {
		t.Fatalf("delete active object: %v", err)
	}
	if _, err := registry.RefreshPending(ctx, active.ID); err != nil {
		t.Fatalf("active object: %v", err)
	}
}

func TestRegistryPublishIsOneWayAndIdempotent(t *testing.T) {
	db, ctx := openTestDatabase(t)
	registry := pgxstore.New(db)
	object, err := registry.CreatePending(ctx, "documents/reports", "report.txt", declare(t, "report"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := registry.MarkAvailable(ctx, object.ID, "text/plain", objectKey(object))
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.MarkAvailable(ctx, object.ID, "image/png", fmt.Sprintf("_objects/%d/other.txt", object.ID))
	if err != nil {
		t.Fatal(err)
	}
	if second.Key() != first.Key() || second.ContentType != first.ContentType || !second.AvailableAt.Equal(*first.AvailableAt) || !second.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("published metadata changed: first=%#v second=%#v", first, second)
	}
	if _, err := registry.RefreshPending(ctx, object.ID); !errors.Is(err, bloby.ErrNotFound) {
		t.Fatalf("refresh available: %v", err)
	}
	if err := registry.RecordMultipartUploadID(ctx, object.ID, "upload-after-finalize"); !errors.Is(err, bloby.ErrNotFound) {
		t.Fatalf("multipart after publication: %v", err)
	}
	if claimed, err := registry.ClaimExpiredPending(ctx, time.Now().Add(time.Hour), time.Now().Add(time.Hour), 100); err != nil || len(claimed) != 0 {
		t.Fatalf("published object expired: %#v %v", claimed, err)
	}
	if _, err := registry.MarkAvailable(ctx, object.ID+1, "text/plain", "_objects/missing/report.txt"); !errors.Is(err, bloby.ErrNotFound) {
		t.Fatalf("publish missing object: %v", err)
	}
}

func TestRegistryMultipartMustCompleteBeforePublication(t *testing.T) {
	db, ctx := openTestDatabase(t)
	registry := pgxstore.New(db)
	object, err := registry.CreatePending(ctx, "documents/reports", "report.txt", declare(t, "report"))
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
	if _, err := registry.MarkAvailable(ctx, object.ID, "text/plain", objectKey(object)); !errors.Is(err, bloby.ErrInvalidInput) {
		t.Fatalf("publish unassembled multipart: %v", err)
	}
	if err := registry.ClearMultipartUploadID(ctx, object.ID, uploadID); err != nil {
		t.Fatal(err)
	}
	if err := registry.ClearMultipartUploadID(ctx, object.ID, uploadID); err != nil {
		t.Fatalf("clear retry: %v", err)
	}
	published, err := registry.MarkAvailable(ctx, object.ID, "text/plain", objectKey(object))
	if err != nil || published.MultipartUploadID != nil {
		t.Fatalf("publish assembled multipart: %#v %v", published, err)
	}
	// A finalizer that lost the race still clears the upload it completed.
	if err := registry.ClearMultipartUploadID(ctx, object.ID, uploadID); err != nil {
		t.Fatalf("clear after publication: %v", err)
	}
	if err := registry.ClearMultipartUploadID(ctx, object.ID+1, uploadID); !errors.Is(err, bloby.ErrNotFound) {
		t.Fatalf("clear missing object: %v", err)
	}
}

func TestRegistryExpiryAndPublishCannotBothWin(t *testing.T) {
	db, ctx := openTestDatabase(t)
	registry := pgxstore.New(db)
	for range 12 {
		object, err := registry.CreatePending(ctx, "documents/reports", "report.txt", declare(t, "report"))
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
			_, err := registry.MarkAvailable(ctx, object.ID, "text/plain", objectKey(object))
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

func TestServiceConcurrentFinalizersPublishOnce(t *testing.T) {
	db, ctx := openTestDatabase(t)
	service := newFileService(t, ctx, pgxstore.New(db))
	const body = "%PDF-1.7\nuploaded report"
	object, action, err := service.Begin(ctx, "documents/reports", "report.pdf", declare(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Finalize(ctx, object.ID, object.Prefix); !errors.Is(err, bloby.ErrInvalidInput) || !errors.Is(err, bloby.ErrObjectNotFound) {
		t.Fatalf("finalize before upload: %v", err)
	}
	rec := httptest.NewRecorder()
	service.TransferHandler().ServeHTTP(rec, httptest.NewRequestWithContext(ctx, action.Target.Method, action.Target.URL, strings.NewReader(body)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("upload status %d", rec.Code)
	}

	const finalizers = 8
	published := make(chan *bloby.Object, finalizers)
	var group sync.WaitGroup
	for range finalizers {
		group.Go(func() {
			available, err := service.Finalize(ctx, object.ID, object.Prefix)
			if err != nil {
				t.Errorf("finalize: %v", err)
				return
			}
			published <- available
		})
	}
	group.Wait()
	close(published)
	if t.Failed() {
		return
	}
	first := <-published
	for available := range published {
		if !available.AvailableAt.Equal(*first.AvailableAt) || !available.UpdatedAt.Equal(first.UpdatedAt) || available.Key() != first.Key() {
			t.Fatalf("finalizers published different objects: %#v %#v", first, available)
		}
	}
	if first.ContentType != "application/pdf" || first.Key() != objectKey(object) || readObject(t, ctx, service, *first) != body {
		t.Fatalf("published %#v", first)
	}
	if err := service.Delete(ctx, object.ID, object.Prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Finalize(ctx, object.ID, object.Prefix); !errors.Is(err, bloby.ErrNotFound) {
		t.Fatalf("finalize after delete: %v", err)
	}
}

func TestServiceReferencedDeletePreservesBytes(t *testing.T) {
	db, ctx := openTestDatabase(t)
	service := newFileService(t, ctx, pgxstore.New(db))
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
	if body := readObject(t, ctx, service, *object); body != "referenced bytes" {
		t.Fatalf("bytes %q", body)
	}
}

type uncertainWriteRegistry struct {
	bloby.Registry
	db *pgxpool.Pool
}

func (r uncertainWriteRegistry) MarkAvailable(ctx context.Context, id int64, contentType, key string) (*bloby.Object, error) {
	object, err := r.Registry.MarkAvailable(ctx, id, contentType, key)
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
	service := newFileService(t, ctx, uncertainWriteRegistry{Registry: pgxstore.New(db), db: db})
	if _, err := service.Write(ctx, "documents/reports", "report.txt", "text/plain", []byte("referenced after commit")); err == nil {
		t.Fatal("expected uncertain publish result")
	}
	objects, count, err := service.ListByPrefix(ctx, "documents/reports", bloby.ListOptions{})
	if err != nil || count != 1 || len(objects) != 1 {
		t.Fatalf("referenced row lost: %v %d %v", objects, count, err)
	}
	if body := readObject(t, ctx, service, objects[0]); body != "referenced after commit" {
		t.Fatalf("referenced bytes %q", body)
	}
}

package pgxstore_test

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/woodleighschool/goodies/bloby"
	"github.com/woodleighschool/goodies/bloby/pgxstore"
)

func TestMigrateSerializesInitialSchemaAndPreservesObjects(t *testing.T) {
	db, ctx := openEmptyTestDatabase(t)
	start := make(chan struct{})
	results := make(chan error, 3)
	for range cap(results) {
		go func() {
			<-start
			results <- pgxstore.Migrate(ctx, db)
		}()
	}
	close(start)
	for range cap(results) {
		if err := <-results; err != nil {
			t.Errorf("concurrent migration: %v", err)
		}
	}
	if t.Failed() {
		return
	}
	registry := pgxstore.New(db)
	object, err := registry.CreatePending(ctx, "documents", "report.txt", declare(t, "report"))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := pgxstore.Migrate(ctx, db); err != nil {
			t.Fatalf("repeat migration: %v", err)
		}
	}
	// A repeated migration leaves an upload in progress alone.
	if got, err := registry.RefreshPending(ctx, object.ID); err != nil || got.Filename != object.Filename {
		t.Fatalf("pending object after repeated migration = %+v, %v", got, err)
	}
}

// A database created before uploads declared their content holds pending rows
// that can never be published and available rows without a CRC64NVME.
func TestMigrateExpiresUndeclaredUploadsAndKeepsPublishedObjects(t *testing.T) {
	db, ctx := openEmptyTestDatabase(t)
	sqlDB := stdlib.OpenDBFromPool(db)
	defer func() { _ = sqlDB.Close() }()
	initial, err := goose.NewProvider(goose.DialectPostgres, sqlDB, os.DirFS("migrations"),
		goose.WithTableName("bloby_migrations"), goose.WithLogger(goose.NopLogger()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := initial.UpTo(ctx, 1); err != nil {
		t.Fatalf("apply initial schema: %v", err)
	}
	insert := func(columns, values string) int64 {
		t.Helper()
		var id int64
		if err := db.QueryRow(ctx, `INSERT INTO storage_objects (prefix, filename`+columns+`) VALUES ('documents', 'report.pdf'`+values+`) RETURNING id`).Scan(&id); err != nil {
			t.Fatalf("insert object: %v", err)
		}
		return id
	}
	sha256 := strings.Repeat("a", 64)
	direct := insert("", "")
	multipart := insert(", multipart_upload_id", ", 'provider-upload'")
	claimed := insert(", expired_at", ", now() - interval '2 hours'")
	available := insert(
		", content_type, size_bytes, sha256, storage_key, available_at",
		", 'application/pdf', 10, '"+sha256+"', '_objects/4/selected/report.pdf', now() - interval '30 days'",
	)
	// Consumers reference objects by foreign key before this schema changes.
	if _, err := db.Exec(ctx, `CREATE TABLE attachments (object_id BIGINT NOT NULL REFERENCES storage_objects(id) ON DELETE RESTRICT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO attachments VALUES ($1)`, available); err != nil {
		t.Fatal(err)
	}

	if err := pgxstore.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	registry := pgxstore.New(db)

	for name, id := range map[string]int64{"direct": direct, "multipart": multipart, "claimed": claimed} {
		if _, err := registry.GetByID(ctx, id); !errors.Is(err, bloby.ErrNotFound) {
			t.Fatalf("%s upload visible after migration: %v", name, err)
		}
		if _, err := registry.RefreshPending(ctx, id); !errors.Is(err, bloby.ErrNotFound) {
			t.Fatalf("%s upload refreshed after migration: %v", name, err)
		}
		if _, err := registry.MarkAvailable(ctx, id, "application/pdf", "_objects/undeclared/report.pdf"); !errors.Is(err, bloby.ErrNotFound) {
			t.Fatalf("%s upload published after migration: %v", name, err)
		}
	}
	// Cleanup retries a claim an hour after it was made; the migration is the
	// claim for the uploads it expired.
	if early, err := registry.ClaimExpiredPending(ctx, time.Now().Add(-24*time.Hour), time.Now().Add(-time.Hour), 100); err != nil || len(early) != 1 || early[0].ID != claimed {
		t.Fatalf("claims right after migration = %#v, %v", early, err)
	}
	expired, err := registry.ClaimExpiredPending(ctx, time.Now().Add(-24*time.Hour), time.Now().Add(time.Hour), 100)
	if err != nil || len(expired) != 3 {
		t.Fatalf("claims once the retry delay passed = %#v, %v", expired, err)
	}
	for _, object := range expired {
		if object.SizeBytes != nil || object.SHA256 != nil || object.CRC64NVME != nil {
			t.Fatalf("undeclared upload gained a declaration: %#v", object)
		}
		if object.ID == multipart && (object.MultipartUploadID == nil || *object.MultipartUploadID != "provider-upload") {
			t.Fatalf("multipart upload lost the upload cleanup must abort: %#v", object)
		}
		if err := registry.DeleteExpiredPending(ctx, object.ID); err != nil {
			t.Fatalf("delete expired upload %d: %v", object.ID, err)
		}
	}

	object, err := registry.GetByID(ctx, available)
	if err != nil {
		t.Fatalf("published object after migration: %v", err)
	}
	if !object.Available() || object.CRC64NVME != nil || object.SHA256Value() != sha256 || object.SizeBytesValue() != 10 ||
		object.ContentType != "application/pdf" || object.Key() != "_objects/4/selected/report.pdf" {
		t.Fatalf("published object changed: %#v", object)
	}
	if items, count, err := registry.ListByPrefix(ctx, "documents", bloby.ListOptions{Limit: 50}); err != nil || count != 1 || len(items) != 1 || items[0].ID != available {
		t.Fatalf("listing after migration = %#v, %d, %v", items, count, err)
	}
	if _, err := registry.Delete(ctx, available); !errors.Is(err, bloby.ErrConflict) {
		t.Fatalf("delete referenced object: %v", err)
	}

	declared, err := registry.CreatePending(ctx, "documents", "declared.pdf", declare(t, "declared"))
	if err != nil {
		t.Fatalf("begin upload after migration: %v", err)
	}
	if _, err := registry.MarkAvailable(ctx, declared.ID, "application/pdf", objectKey(declared)); err != nil {
		t.Fatalf("publish upload after migration: %v", err)
	}
}

func TestSchemaRejectsIncompleteObjectStates(t *testing.T) {
	db, ctx := openTestDatabase(t)
	registry := pgxstore.New(db)
	object, err := registry.CreatePending(ctx, "documents", "report.txt", declare(t, "report"))
	if err != nil {
		t.Fatal(err)
	}
	rejects := func(t *testing.T, id int64, set string) {
		t.Helper()
		_, err := db.Exec(ctx, "UPDATE storage_objects SET "+set+" WHERE id = $1", id)
		pgErr, ok := errors.AsType[*pgconn.PgError](err)
		if !ok || pgErr.Code != pgerrcode.CheckViolation {
			t.Fatalf("invalid state error = %v, want check violation", err)
		}
	}
	for _, test := range []struct {
		name string
		set  string
	}{
		{name: "pending without a size", set: "size_bytes = NULL"},
		{name: "pending without a SHA-256", set: "sha256 = NULL"},
		{name: "pending without a CRC64NVME", set: "crc64nvme = NULL"},
		{name: "pending with an uppercase CRC64NVME", set: "crc64nvme = 'AE8B14860A799888'"},
		{name: "pending with a content type", set: "content_type = 'text/plain'"},
		{name: "pending with a stored key", set: "storage_key = '_objects/1/report.txt'"},
		{name: "incomplete publication", set: "available_at = now()"},
		{name: "blank multipart ID", set: "multipart_upload_id = ' '"},
	} {
		t.Run(test.name, func(t *testing.T) { rejects(t, object.ID, test.set) })
	}
	key := objectKey(object)
	if _, err := registry.MarkAvailable(ctx, object.ID, "text/plain", key); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		set  string
	}{
		{name: "available without a stored key", set: "storage_key = NULL"},
		{name: "available with a blank stored key", set: "storage_key = ' '"},
		{name: "available without a content type", set: "content_type = ''"},
		{name: "available without a size", set: "size_bytes = NULL"},
		{name: "available with a negative size", set: "size_bytes = -1"},
		{name: "available without a SHA-256", set: "sha256 = NULL"},
		{name: "available with an invalid SHA-256", set: "sha256 = 'invalid'"},
		{name: "available with an unassembled multipart upload", set: "multipart_upload_id = 'provider-upload'"},
		{name: "available and expired", set: "expired_at = now()"},
	} {
		t.Run(test.name, func(t *testing.T) { rejects(t, object.ID, test.set) })
	}
	other, err := registry.CreatePending(ctx, "documents", "other.txt", declare(t, "other"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.MarkAvailable(ctx, other.ID, "text/plain", key); !errors.Is(err, bloby.ErrAlreadyExists) {
		t.Fatalf("duplicate storage key error = %v, want already exists", err)
	}
	if _, err := registry.CreatePending(ctx, "documents", "malformed.txt", bloby.Content{SizeBytes: 1, SHA256: "invalid", CRC64NVME: "invalid"}); !errors.Is(err, bloby.ErrInvalidInput) {
		t.Fatalf("malformed declaration error = %v, want invalid input", err)
	}
}

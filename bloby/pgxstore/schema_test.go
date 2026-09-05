package pgxstore_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
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
	object, err := registry.CreatePending(ctx, "documents", "report.txt")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := pgxstore.Migrate(ctx, db); err != nil {
			t.Fatalf("repeat migration: %v", err)
		}
	}
	if got, err := registry.GetByID(ctx, object.ID); err != nil || got.Filename != object.Filename {
		t.Fatalf("object after repeated migration = %+v, %v", got, err)
	}
}

func TestSchemaRejectsIncompleteObjectStates(t *testing.T) {
	db, ctx := openTestDatabase(t)
	registry := pgxstore.New(db)
	object, err := registry.CreatePending(ctx, "documents", "report.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		set  string
	}{
		{name: "pending metadata", set: "content_type = 'text/plain'"},
		{name: "pending stored key", set: "storage_key = 'candidate'"},
		{name: "incomplete publication", set: "available_at = now()"},
		{name: "blank multipart ID", set: "multipart_upload_id = ' '"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := db.Exec(ctx, "UPDATE storage_objects SET "+test.set+" WHERE id = $1", object.ID)
			pgErr, ok := errors.AsType[*pgconn.PgError](err)
			if !ok || pgErr.Code != pgerrcode.CheckViolation {
				t.Fatalf("invalid state error = %v, want check violation", err)
			}
		})
	}
	key := fmt.Sprintf("_objects/%d/candidate", object.ID)
	if _, err := registry.MarkAvailable(ctx, object.ID, 7, "text/plain", strings.Repeat("a", 64), key); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		set  string
	}{
		{name: "missing stored key", set: "storage_key = NULL"},
		{name: "blank stored key", set: "storage_key = ' '"},
		{name: "negative size", set: "size_bytes = -1"},
		{name: "invalid hash", set: "sha256 = 'invalid'"},
		{name: "unassembled multipart", set: "multipart_upload_id = 'provider-upload'"},
		{name: "expired publication", set: "expired_at = now()"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := db.Exec(ctx, "UPDATE storage_objects SET "+test.set+" WHERE id = $1", object.ID)
			pgErr, ok := errors.AsType[*pgconn.PgError](err)
			if !ok || pgErr.Code != pgerrcode.CheckViolation {
				t.Fatalf("invalid state error = %v, want check violation", err)
			}
		})
	}
	other, err := registry.CreatePending(ctx, "documents", "other.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.MarkAvailable(ctx, other.ID, 7, "text/plain", strings.Repeat("a", 64), key); !errors.Is(err, bloby.ErrAlreadyExists) {
		t.Fatalf("duplicate storage key error = %v, want already exists", err)
	}
}

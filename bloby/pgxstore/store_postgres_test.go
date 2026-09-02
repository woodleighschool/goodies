//go:build postgres

package pgxstore_test

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/woodleighschool/goodies/bloby"
	"github.com/woodleighschool/goodies/bloby/internal/testdb"
	"github.com/woodleighschool/goodies/bloby/pgxstore"
)

func TestRegistryLifecycleAndListing(t *testing.T) {
	db, ctx := testdb.Open(t, pgxstore.PostgresSchema())
	objects := bloby.NewObjectStore(pgxstore.New(db), nil, slog.New(slog.DiscardHandler))

	first, err := objects.CreatePending(ctx, "documents/reports", "first.pdf")
	if err != nil {
		t.Fatalf("create first object: %v", err)
	}
	if _, err := objects.MarkAvailable(ctx, first.ID, 10, "application/pdf", strings.Repeat("a", 64)); err != nil {
		t.Fatalf("mark first object available: %v", err)
	}

	second, err := objects.CreatePending(ctx, "documents/reports", "second.pdf")
	if err != nil {
		t.Fatalf("create second object: %v", err)
	}
	if _, err := objects.MarkAvailable(ctx, second.ID, 20, "application/pdf", strings.Repeat("b", 64)); err != nil {
		t.Fatalf("mark second object available: %v", err)
	}

	items, count, err := objects.ListByPrefix(ctx, "documents/reports", bloby.ListOptions{})
	if err != nil {
		t.Fatalf("list objects: %v", err)
	}
	if count != 2 || len(items) != 2 || items[0].ID != second.ID || items[1].ID != first.ID {
		t.Fatalf("list = %#v, count = %d", items, count)
	}
}

func TestRegistryDeletePreservesReferencedObject(t *testing.T) {
	db, ctx := testdb.Open(t, pgxstore.PostgresSchema())
	objects := bloby.NewObjectStore(pgxstore.New(db), nil, slog.New(slog.DiscardHandler))
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

	if err := objects.Delete(ctx, object.ID); !errors.Is(err, bloby.ErrConflict) {
		t.Fatalf("delete error = %v, want ErrConflict", err)
	}
	if _, err := objects.GetByID(ctx, object.ID); err != nil {
		t.Fatalf("get referenced object: %v", err)
	}
}

func TestRegistryClaimsAbandonedPendingObjects(t *testing.T) {
	db, ctx := testdb.Open(t, pgxstore.PostgresSchema())
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

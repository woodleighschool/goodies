package bloby

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// cleanupAge is safely past the age at which cleanup stops waiting for an upload.
const cleanupAge = 48 * time.Hour

// cleanupBackend is a service on one backend with a way to age what it stores.
type cleanupBackend struct {
	service  *Service
	registry *memoryRegistry
	// fixture is set for the S3 backend.
	fixture *s3Fixture
	// age backdates every stored object, and every incomplete upload, by cleanupAge.
	age func(t *testing.T)
}

func eachCleanupBackend(t *testing.T, test func(t *testing.T, backend cleanupBackend)) {
	t.Helper()
	t.Run("file", func(t *testing.T) {
		service, registry := newFileService(t)
		file, ok := service.backend.(*fileStore)
		if !ok {
			t.Fatal("expected file backend")
		}
		test(t, cleanupBackend{service: service, registry: registry, age: func(t *testing.T) {
			t.Helper()
			root, err := os.OpenRoot(file.root)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			old := time.Now().Add(-cleanupAge)
			err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return err
				}
				return root.Chtimes(path, old, old)
			})
			if err != nil {
				t.Fatal(err)
			}
		}})
	})
	t.Run("s3", func(t *testing.T) {
		service, fixture := newS3Fixture(t)
		test(t, cleanupBackend{service: service, registry: memoryRegistryOf(t, service), fixture: fixture, age: func(*testing.T) {
			fixture.age(cleanupAge)
		}})
	})
}

func (b cleanupBackend) put(t *testing.T, key, body string) {
	t.Helper()
	if err := b.service.backend.Put(t.Context(), key, strings.NewReader(body), declare(body), putOptions{}); err != nil {
		t.Fatal(err)
	}
}

func (b cleanupBackend) assertStored(t *testing.T, want ...string) {
	t.Helper()
	slices.Sort(want)
	if got := storedKeys(t, b.service); !slices.Equal(got, want) {
		t.Fatalf("stored keys:\n got %v\nwant %v", got, want)
	}
}

func TestCleanupSweepsBytesNoObjectOwns(t *testing.T) {
	eachCleanupBackend(t, func(t *testing.T, backend cleanupBackend) {
		service, registry := backend.service, backend.registry
		published, err := service.Write(t.Context(), "documents/reports", "published.txt", "text/plain", []byte("published"))
		if err != nil {
			t.Fatal(err)
		}
		pending, pendingAction, err := service.BeginDirect(t.Context(), "documents/reports", "pending.txt", declare("pending"))
		if err != nil {
			t.Fatal(err)
		}
		mustUpload(t, service, *pendingAction.Target, "pending")

		// An upload target outlives its object and can land bytes after the delete.
		deleted, deletedAction, err := service.BeginDirect(t.Context(), "documents/reports", "deleted.txt", declare("deleted"))
		if err != nil {
			t.Fatal(err)
		}
		if err := service.Delete(t.Context(), deleted.ID, deleted.Prefix); err != nil {
			t.Fatal(err)
		}
		mustUpload(t, service, *deletedAction.Target, "deleted")

		// An object published by an earlier release keeps the key it was stored
		// under, and bytes elsewhere under its ID belong to nothing.
		earlier, err := service.Write(t.Context(), "documents/reports", "earlier.txt", "text/plain", []byte("earlier"))
		if err != nil {
			t.Fatal(err)
		}
		earlierKey := fmt.Sprintf("_objects/%d/selected/earlier.txt", earlier.ID)
		backend.put(t, earlierKey, "earlier")
		registry.edit(earlier.ID, func(object *Object) { object.StorageKey = &earlierKey })
		unselected := fmt.Sprintf("_objects/%d/unselected/published.txt", published.ID)
		backend.put(t, unselected, "unselected")

		everything := []string{published.Key(), pending.key(), deleted.key(), earlierKey, earlier.Key(), unselected}
		service.sweepExpiredUploads(t.Context())
		backend.assertStored(t, everything...)

		backend.age(t)
		registry.getFailure = errors.New("registry unavailable")
		service.sweepExpiredUploads(t.Context())
		registry.getFailure = nil
		backend.assertStored(t, everything...)

		service.sweepExpiredUploads(t.Context())
		backend.assertStored(t, published.Key(), pending.key(), earlierKey)
		if got := readAvailable(t, service, *published); got != "published" {
			t.Fatalf("published object holds %q", got)
		}
		kept, err := service.GetByID(t.Context(), earlier.ID)
		if err != nil || readAvailable(t, service, *kept) != "earlier" {
			t.Fatalf("earlier object: %#v %v", kept, err)
		}
		if _, err := service.Finalize(t.Context(), pending.ID, pending.Prefix); err != nil {
			t.Fatalf("pending upload lost its bytes: %v", err)
		}
	})
}

func TestCleanupExpiresAbandonedPendingUploads(t *testing.T) {
	eachCleanupBackend(t, func(t *testing.T, backend cleanupBackend) {
		service, registry := backend.service, backend.registry
		abandoned, abandonedAction, err := service.BeginDirect(t.Context(), "documents/reports", "abandoned.txt", declare("abandoned"))
		if err != nil {
			t.Fatal(err)
		}
		mustUpload(t, service, *abandonedAction.Target, "abandoned")
		active, activeAction, err := service.BeginDirect(t.Context(), "documents/reports", "active.txt", declare("active"))
		if err != nil {
			t.Fatal(err)
		}
		mustUpload(t, service, *activeAction.Target, "active")
		expire := []int64{abandoned.ID}
		if backend.fixture != nil {
			multipart := beginMultipart(t, service, "documents/reports", "abandoned.pkg", declare("abandoned part"))
			mustUploadPart(t, service, multipart, 1, "abandoned part")
			expire = append(expire, multipart.ID)
		}
		for _, id := range expire {
			registry.edit(id, func(object *Object) { object.UpdatedAt = object.UpdatedAt.Add(-cleanupAge) })
		}

		service.sweepExpiredUploads(t.Context())

		for _, id := range expire {
			if _, err := service.GetByID(t.Context(), id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("abandoned upload %d: %v", id, err)
			}
		}
		backend.assertStored(t, active.key())
		if len(registry.objects) != 1 {
			t.Fatalf("registry holds %d objects", len(registry.objects))
		}
		if backend.fixture != nil && len(backend.fixture.uploadKeys()) != 0 {
			t.Fatalf("abandoned multipart upload remains: %v", backend.fixture.uploadKeys())
		}
		_, err = service.Finalize(t.Context(), abandoned.ID, abandoned.Prefix)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("finalize expired upload: %v", err)
		}
	})
}

func TestS3CleanupAbortsUploadsNoPendingObjectOwns(t *testing.T) {
	service, fixture := newS3Fixture(t)
	registry := memoryRegistryOf(t, service)
	parts := []string{"first part, ", "second part"}
	owned := beginMultipart(t, service, "uploads", "owned.pkg", declare(strings.Join(parts, "")))
	mustUploadPart(t, service, owned, 1, parts[0])
	published, err := service.Write(t.Context(), "uploads", "published.pkg", "application/octet-stream", []byte("published"))
	if err != nil {
		t.Fatal(err)
	}
	// The registry row went without its upload being aborted.
	stranded := beginMultipart(t, service, "uploads", "stranded.pkg", declare("stranded"))
	if _, err := registry.Delete(t.Context(), stranded.ID); err != nil {
		t.Fatal(err)
	}
	unowned := []string{
		stranded.key(),
		// Another upload to a key whose object records a different upload.
		owned.key(),
		// An upload to a published object's key.
		published.key(),
		// Uploads outside the object prefix; the bucket is dedicated to Bloby.
		"_staging/uploads/7/earlier.pkg",
	}
	for _, key := range unowned[1:] {
		fixture.open(key)
	}
	everything := append([]string{owned.key()}, unowned...)
	slices.Sort(everything)

	service.sweepExpiredUploads(t.Context())
	if got := fixture.uploadKeys(); !slices.Equal(got, everything) {
		t.Fatalf("recent uploads aborted: %v", got)
	}

	fixture.age(cleanupAge)
	registry.getFailure = errors.New("registry unavailable")
	service.sweepExpiredUploads(t.Context())
	registry.getFailure = nil
	if got := fixture.uploadKeys(); !slices.Equal(got, everything) {
		t.Fatalf("uploads aborted without resolving ownership: %v", got)
	}

	service.sweepExpiredUploads(t.Context())
	if got := fixture.uploadKeys(); !slices.Equal(got, []string{owned.key()}) {
		t.Fatalf("uploads after cleanup: %v", got)
	}
	if got := readAvailable(t, service, *published); got != "published" {
		t.Fatalf("published object holds %q", got)
	}
	mustUploadPart(t, service, owned, 2, parts[1])
	available, err := service.Finalize(t.Context(), owned.ID, owned.Prefix)
	if err != nil {
		t.Fatalf("owned upload lost its parts: %v", err)
	}
	if got := readAvailable(t, service, *available); got != strings.Join(parts, "") {
		t.Fatalf("assembled %q", got)
	}
}

func TestCleanupRemovesOnlyAbandonedObjectsUnderReferencedPrefixes(t *testing.T) {
	service, registry := newFileService(t, "munki/installers")
	write := func(prefix, name string, age time.Duration) *Object {
		t.Helper()
		object, err := service.Write(t.Context(), prefix, name, "application/octet-stream", []byte(name))
		if err != nil {
			t.Fatal(err)
		}
		registry.edit(object.ID, func(stored *Object) {
			available := stored.AvailableAt.Add(-age)
			stored.AvailableAt = &available
		})
		return object
	}
	abandoned := write("munki/installers", "abandoned.pkg", cleanupAge)
	attached := write("munki/installers", "attached.pkg", cleanupAge)
	recent := write("munki/installers", "recent.pkg", time.Hour)
	library := write("munki/icons", "library.png", cleanupAge)
	registry.referenced[attached.ID] = true

	service.sweepExpiredUploads(t.Context())

	if _, err := registry.GetByID(t.Context(), abandoned.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("abandoned installer: %v", err)
	}
	stored := cleanupBackend{service: service}
	stored.assertStored(t, attached.Key(), recent.Key(), library.Key())
	for name, object := range map[string]*Object{"attached": attached, "recent": recent, "library": library} {
		if readAvailable(t, service, *object) != object.Filename {
			t.Fatalf("%s object was altered", name)
		}
	}

	// A caller's request has usually ended by the time it discards an object.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	service.DeleteUnreferenced(ctx, recent.ID, attached.ID)
	stored.assertStored(t, attached.Key(), library.Key())
}

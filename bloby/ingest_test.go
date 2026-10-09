package bloby

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func newFileService(t *testing.T, referencedPrefixes ...string) (*Service, *memoryRegistry) {
	t.Helper()
	registry := newMemoryRegistry()
	service, err := New(t.Context(), registry, Config{
		Kind: KindFile, TransferTTL: time.Minute, ReferencedPrefixes: referencedPrefixes,
		File: FileConfig{Root: t.TempDir(), BaseURL: "https://storage.invalid", CapabilityKeyHex: testCapabilityKeyHex},
	}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	return service, registry
}

// upload sends body to target as an uploader would and returns the status.
func upload(t *testing.T, service *Service, target UploadTarget, body string) int {
	t.Helper()
	if _, ok := service.backend.(*fileStore); ok {
		req := httptest.NewRequestWithContext(t.Context(), target.Method, target.URL, strings.NewReader(body))
		for name, value := range target.Headers {
			req.Header.Set(name, value)
		}
		rec := httptest.NewRecorder()
		service.TransferHandler().ServeHTTP(rec, req)
		return rec.Code
	}
	return sendS3Upload(t, target, target.Headers, body)
}

func mustUpload(t *testing.T, service *Service, target UploadTarget, body string) {
	t.Helper()
	if status := upload(t, service, target, body); status != http.StatusOK && status != http.StatusNoContent {
		t.Fatalf("upload status %d", status)
	}
}

// readAvailable returns the delivered bytes after checking them against the
// object's recorded size and SHA-256.
func readAvailable(t *testing.T, service *Service, object Object) string {
	t.Helper()
	reader, err := service.Open(t.Context(), object)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(body)) != object.SizeBytesValue() || hashString(string(body)) != object.SHA256Value() {
		t.Fatalf("metadata differs from delivered bytes: object=%#v, size=%d, hash=%s", object, len(body), hashString(string(body)))
	}
	return string(body)
}

// storedKeys lists every key the backend holds for objects.
func storedKeys(t *testing.T, service *Service) []string {
	t.Helper()
	keys, err := service.backend.expiredObjects(t.Context(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(keys)
	return keys
}

func assertNotArrived(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrInvalidInput) || !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("finalize error = %v, want ErrInvalidInput and ErrObjectNotFound", err)
	}
}

func assertContentMismatch(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrInvalidInput) || !errors.Is(err, ErrContentMismatch) {
		t.Fatalf("finalize error = %v, want ErrInvalidInput and ErrContentMismatch", err)
	}
}

func TestFileFinalizeBeforeUploadReportsNotArrived(t *testing.T) {
	service, registry := newFileService(t)
	const body = "report body"
	object, action, err := service.BeginDirect(t.Context(), "documents/reports", `folder\Report.txt`, declare(body))
	if err != nil {
		t.Fatal(err)
	}
	if object.Filename != "Report.txt" || object.Available() || object.Key() != "" || object.SHA256Value() != hashString(body) {
		t.Fatalf("pending object %#v", object)
	}
	if action.Strategy != StrategyDirectPut || action.Target == nil {
		t.Fatalf("action %#v", action)
	}
	if _, err := service.DownloadURL(t.Context(), *object, 0, DeliveryOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pending download URL error: %v", err)
	}

	_, err = service.Finalize(t.Context(), object.ID, object.Prefix)
	assertNotArrived(t, err)
	if status := upload(t, service, *action.Target, "other bytes!"); status != http.StatusBadRequest {
		t.Fatalf("undeclared bytes status %d", status)
	}
	_, err = service.Finalize(t.Context(), object.ID, object.Prefix)
	assertNotArrived(t, err)
	if _, err := service.Finalize(t.Context(), object.ID, "documents/other"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("wrong prefix error: %v", err)
	}
	if registry.published != 0 {
		t.Fatal("object published before its bytes arrived")
	}

	mustUpload(t, service, *action.Target, body)
	available, err := service.Finalize(t.Context(), object.ID, object.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("_objects/%d/Report.txt", object.ID); available.Key() != want {
		t.Fatalf("stored key %q, want %q", available.Key(), want)
	}
	if got := readAvailable(t, service, *available); got != body {
		t.Fatalf("published %q", got)
	}
}

func TestFileUploadReplayCannotChangePublishedObject(t *testing.T) {
	service, registry := newFileService(t)
	const original = "%PDF-1.7\noriginal document"
	object, action, err := service.BeginDirect(t.Context(), "documents/reports", "report.pdf", declare(original))
	if err != nil {
		t.Fatal(err)
	}
	mustUpload(t, service, *action.Target, original)
	available, err := service.Finalize(t.Context(), object.ID, object.Prefix)
	if err != nil {
		t.Fatal(err)
	}

	if status := upload(t, service, *action.Target, "replacement after publication"); status != http.StatusBadRequest {
		t.Fatalf("replay with other bytes status %d", status)
	}
	mustUpload(t, service, *action.Target, original)
	again, err := service.Finalize(t.Context(), object.ID, object.Prefix)
	if err != nil || !again.AvailableAt.Equal(*available.AvailableAt) || again.Key() != available.Key() {
		t.Fatalf("finalize retry: %#v %v", again, err)
	}
	if registry.published != 1 {
		t.Fatalf("published %d times", registry.published)
	}
	if got := readAvailable(t, service, *again); got != original {
		t.Fatalf("published bytes %q", got)
	}
	items, total, err := service.ListByPrefix(t.Context(), object.Prefix, ListOptions{})
	if err != nil || total != 1 || len(items) != 1 {
		t.Fatalf("listing %v %d %v", items, total, err)
	}
}

func TestFileConcurrentFinalizersPublishOnce(t *testing.T) {
	const body = "initial"
	begin := func(t *testing.T) (*Service, *memoryRegistry, *Object) {
		t.Helper()
		service, registry := newFileService(t)
		object, action, err := service.BeginDirect(t.Context(), "documents/reports", "report.txt", declare(body))
		if err != nil {
			t.Fatal(err)
		}
		mustUpload(t, service, *action.Target, body)
		return service, registry, object
	}

	t.Run("in parallel", func(t *testing.T) {
		service, registry, object := begin(t)
		const finalizers = 12
		published := make(chan *Object, finalizers)
		var group sync.WaitGroup
		for range finalizers {
			group.Go(func() {
				available, err := service.Finalize(t.Context(), object.ID, object.Prefix)
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
			if !available.AvailableAt.Equal(*first.AvailableAt) || available.Key() != first.Key() {
				t.Fatalf("finalizers published different objects: %#v %#v", first, available)
			}
		}
		if registry.published != 1 || readAvailable(t, service, *first) != body {
			t.Fatalf("published %d times", registry.published)
		}
	})

	// A finalizer that loses the race after reading a pending object returns the
	// winner's publication.
	t.Run("published between lookup and refresh", func(t *testing.T) {
		service, registry, object := begin(t)
		var winner *Object
		registry.beforeRefresh = func() {
			registry.beforeRefresh = nil
			var err error
			if winner, err = service.Finalize(t.Context(), object.ID, object.Prefix); err != nil {
				t.Errorf("winning finalize: %v", err)
			}
		}
		loser, err := service.Finalize(t.Context(), object.ID, object.Prefix)
		if err != nil || winner == nil {
			t.Fatalf("losing finalize: %v", err)
		}
		if !loser.AvailableAt.Equal(*winner.AvailableAt) || registry.published != 1 {
			t.Fatalf("finalizers published different objects: %#v %#v", winner, loser)
		}
	})
}

func TestDeleteDoesNotRepublish(t *testing.T) {
	const body = "original"
	begin := func(t *testing.T) (*Service, *memoryRegistry, *Object, UploadTarget) {
		t.Helper()
		service, registry := newFileService(t)
		object, action, err := service.BeginDirect(t.Context(), "documents/reports", "report.txt", declare(body))
		if err != nil {
			t.Fatal(err)
		}
		mustUpload(t, service, *action.Target, body)
		return service, registry, object, *action.Target
	}

	t.Run("during finalize", func(t *testing.T) {
		service, registry, object, _ := begin(t)
		verified := make(chan struct{})
		resume := make(chan struct{})
		registry.beforeMark = func() error { close(verified); <-resume; return nil }
		result := make(chan error, 1)
		go func() { _, err := service.Finalize(t.Context(), object.ID, object.Prefix); result <- err }()
		<-verified
		if err := service.Delete(t.Context(), object.ID, object.Prefix); err != nil {
			t.Fatal(err)
		}
		close(resume)
		if err := <-result; !errors.Is(err, ErrNotFound) {
			t.Fatalf("finalize after delete: %v", err)
		}
		if keys := storedKeys(t, service); registry.published != 0 || len(keys) != 0 {
			t.Fatalf("deleted object survived: published=%d keys=%v", registry.published, keys)
		}
	})

	t.Run("after finalize", func(t *testing.T) {
		service, registry, object, target := begin(t)
		available, err := service.Finalize(t.Context(), object.ID, object.Prefix)
		if err != nil {
			t.Fatal(err)
		}
		if err := service.Delete(t.Context(), object.ID, "documents/other"); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("wrong prefix delete: %v", err)
		}
		if err := service.Delete(t.Context(), object.ID, object.Prefix); err != nil {
			t.Fatal(err)
		}
		if _, err := service.backend.Open(t.Context(), available.Key()); !errors.Is(err, ErrObjectNotFound) {
			t.Fatalf("deleted bytes: %v", err)
		}
		// The upload target outlives the object; bytes it lands cannot bring it back.
		mustUpload(t, service, target, body)
		if _, err := service.Finalize(t.Context(), object.ID, object.Prefix); !errors.Is(err, ErrNotFound) {
			t.Fatalf("finalize after delete: %v", err)
		}
		if _, err := service.GetByID(t.Context(), object.ID); !errors.Is(err, ErrNotFound) || registry.published != 1 {
			t.Fatalf("deleted object returned: %v, published=%d", err, registry.published)
		}
	})
}

func TestFinalizeDetectsContentTypeFromLeadingBytes(t *testing.T) {
	png := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 8*1024)
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{name: "document", body: "%PDF-1.7\nreport", want: "application/pdf"},
		{name: "longer than the detected head", body: png, want: "image/png"},
		{name: "binary only past the detected head", body: strings.Repeat("a", contentTypeHeadBytes) + "\x00\x01\x02", want: "text/plain; charset=utf-8"},
		{name: "empty", body: "", want: "text/plain"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, _ := newFileService(t)
			object, action, err := service.Begin(t.Context(), "documents/reports", "upload.bin", declare(test.body))
			if err != nil {
				t.Fatal(err)
			}
			mustUpload(t, service, *action.Target, test.body)
			available, err := service.Finalize(t.Context(), object.ID, object.Prefix)
			if err != nil {
				t.Fatal(err)
			}
			if available.ContentType != test.want {
				t.Fatalf("content type %q, want %q", available.ContentType, test.want)
			}
			if got := readAvailable(t, service, *available); got != test.body {
				t.Fatalf("published %d bytes, want %d", len(got), len(test.body))
			}
		})
	}
}

func TestFinalizeRetriesAfterRegistryFailure(t *testing.T) {
	const body = "committed bytes"
	for name, fail := range map[string]func(*memoryRegistry, func() error){
		"publish refused":       func(r *memoryRegistry, hook func() error) { r.beforeMark = hook },
		"publish response lost": func(r *memoryRegistry, hook func() error) { r.afterMark = hook },
	} {
		t.Run(name, func(t *testing.T) {
			service, registry := newFileService(t)
			object, action, err := service.BeginDirect(t.Context(), "documents/reports", "report.txt", declare(body))
			if err != nil {
				t.Fatal(err)
			}
			mustUpload(t, service, *action.Target, body)
			failed := false
			fail(registry, func() error {
				if failed {
					return nil
				}
				failed = true
				return errors.New("registry unavailable")
			})
			if _, err := service.Finalize(t.Context(), object.ID, object.Prefix); err == nil {
				t.Fatal("expected registry failure")
			}
			available, err := service.Finalize(t.Context(), object.ID, object.Prefix)
			if err != nil {
				t.Fatal(err)
			}
			if registry.published != 1 || readAvailable(t, service, *available) != body {
				t.Fatalf("retry published %d times", registry.published)
			}
		})
	}
}

func TestWritePublishesAndCleansUpOnFailure(t *testing.T) {
	service, registry := newFileService(t)
	const body = `{"name":"production"}`
	written, err := service.Write(t.Context(), "munki/catalogues", "production.json", "APPLICATION/JSON", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	content := declare(body)
	if written.ContentType != "application/json" || written.CRC64NVME == nil || *written.CRC64NVME != content.CRC64NVME {
		t.Fatalf("written object %#v", written)
	}
	if want := fmt.Sprintf("_objects/%d/production.json", written.ID); written.Key() != want {
		t.Fatalf("stored key %q, want %q", written.Key(), want)
	}
	if got := readAvailable(t, service, *written); got != body {
		t.Fatalf("written %q", got)
	}

	write := func() error {
		_, err := service.Write(t.Context(), "munki/catalogues", "failed.json", "application/json", []byte(`{"name":"testing"}`))
		return err
	}
	assertOnlyWrittenRemains := func(t *testing.T) {
		t.Helper()
		if keys := storedKeys(t, service); len(keys) != 1 || keys[0] != written.Key() {
			t.Fatalf("stored keys %v", keys)
		}
		if len(registry.objects) != 1 {
			t.Fatalf("registry holds %d objects", len(registry.objects))
		}
	}
	t.Run("invalid content type", func(t *testing.T) {
		if _, err := service.Write(t.Context(), "munki/catalogues", "failed.json", "not a content type", nil); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("write error %v", err)
		}
		assertOnlyWrittenRemains(t)
	})
	t.Run("publish refused", func(t *testing.T) {
		registry.beforeMark = func() error { return errors.New("registry unavailable") }
		defer func() { registry.beforeMark = nil }()
		if err := write(); err == nil {
			t.Fatal("expected registry failure")
		}
		assertOnlyWrittenRemains(t)
	})
	t.Run("publish response lost", func(t *testing.T) {
		registry.afterMark = func() error { return errors.New("commit response lost") }
		defer func() { registry.afterMark = nil }()
		if err := write(); err == nil {
			t.Fatal("expected registry failure")
		}
		assertOnlyWrittenRemains(t)
	})
	t.Run("storage refuses the bytes", func(t *testing.T) {
		file, ok := service.backend.(*fileStore)
		if !ok {
			t.Fatal("expected file backend")
		}
		// A file where the next object's directory belongs makes its write fail.
		blocked := filepath.Join(file.root, "_objects", fmt.Sprint(registry.nextID+1))
		if err := os.WriteFile(blocked, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Remove(blocked) }()
		if err := write(); err == nil {
			t.Fatal("expected storage failure")
		}
		if len(registry.objects) != 1 {
			t.Fatalf("registry holds %d objects", len(registry.objects))
		}
	})
}

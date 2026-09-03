package bloby

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newFileService(t *testing.T) (*Service, *memoryRegistry) {
	t.Helper()
	registry := newMemoryRegistry()
	service, err := New(t.Context(), registry, Config{Kind: KindFile, TransferTTL: time.Minute, File: FileConfig{Root: t.TempDir(), BaseURL: "https://storage.invalid", CapabilityKeyHex: testCapabilityKeyHex}}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	return service, registry
}

func putTarget(t *testing.T, handler http.Handler, target UploadTarget, body string) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), target.Method, target.URL, strings.NewReader(body))
	for name, value := range target.Headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("upload status %d: %s", rec.Code, rec.Body.String())
	}
}

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

func TestFileUploadReplayCannotChangePublishedObject(t *testing.T) {
	service, registry := newFileService(t)
	object, action, err := service.BeginDirect(t.Context(), "documents/reports", `folder\Report.pdf`)
	if err != nil {
		t.Fatal(err)
	}
	if object.Filename != "Report.pdf" || action.Strategy != StrategyDirectPut {
		t.Fatalf("begin: %#v %#v", object, action)
	}
	handler := service.TransferHandler()
	original := "%PDF-1.7\noriginal document"
	putTarget(t, handler, *action.Target, original)
	if _, err := service.DownloadURL(t.Context(), *object, 0, DeliveryOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pending URL error: %v", err)
	}
	if _, err := service.Finalize(t.Context(), object.ID, "documents/other"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("wrong prefix error: %v", err)
	}
	available, err := service.Finalize(t.Context(), object.ID, object.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	putTarget(t, handler, *action.Target, "replacement after publication")
	again, err := service.Finalize(t.Context(), object.ID, object.Prefix)
	if err != nil || !again.AvailableAt.Equal(*available.AvailableAt) {
		t.Fatalf("retry: %#v %v", again, err)
	}
	if got := readAvailable(t, service, *again); got != original {
		t.Fatalf("published bytes %q", got)
	}
	items, total, err := service.ListByPrefix(t.Context(), object.Prefix, ListOptions{})
	if err != nil || total != 1 || len(items) != 1 {
		t.Fatalf("listing %v %d %v", items, total, err)
	}
	if err := service.Delete(t.Context(), object.ID, object.Prefix); err != nil {
		t.Fatal(err)
	}
	putTarget(t, handler, *action.Target, "late write after deletion")
	if _, err := registry.GetByID(t.Context(), object.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted registry entry: %v", err)
	}
	if _, _, err := service.backend.Open(t.Context(), available.Key()); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("deleted final bytes: %v", err)
	}
	file, ok := service.backend.(*fileStore)
	if !ok {
		t.Fatal("expected file backend")
	}
	staging, err := file.resolve(object.stagingKey())
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(staging, old, old); err != nil {
		t.Fatal(err)
	}
	service.sweepExpiredUploads(t.Context())
	if _, _, err := file.Open(t.Context(), object.stagingKey()); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("orphan staging retained: %v", err)
	}
}

func TestFileFinalizeRetryPublishesSelectedCandidate(t *testing.T) {
	service, registry := newFileService(t)
	object, action, err := service.Begin(t.Context(), "documents/reports", "report.txt", 10)
	if err != nil {
		t.Fatal(err)
	}
	putTarget(t, service.TransferHandler(), *action.Target, "original")
	var attempts atomic.Int32
	registry.beforeMark = func() error {
		if attempts.Add(1) == 1 {
			return errors.New("temporary registry failure")
		}
		return nil
	}
	if _, err := service.Finalize(t.Context(), object.ID, object.Prefix); err == nil {
		t.Fatal("expected registry failure")
	}
	putTarget(t, service.TransferHandler(), *action.Target, "replayed")
	available, err := service.Finalize(t.Context(), object.ID, object.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAvailable(t, service, *available); got != "replayed" {
		t.Fatalf("retry did not publish its selected candidate: %q", got)
	}
}

func TestFileConcurrentFinalizersPublishOneRepresentation(t *testing.T) {
	service, _ := newFileService(t)
	object, action, err := service.BeginDirect(t.Context(), "documents/reports", "report.txt")
	if err != nil {
		t.Fatal(err)
	}
	putTarget(t, service.TransferHandler(), *action.Target, "initial")
	var group sync.WaitGroup
	for range 12 {
		group.Go(func() {
			available, err := service.Finalize(t.Context(), object.ID, object.Prefix)
			if err != nil {
				t.Errorf("finalize: %v", err)
				return
			}
			if got := readAvailable(t, service, *available); got != "initial" {
				t.Errorf("published %q", got)
			}
		})
	}
	group.Wait()
}

func TestDeletingDuringFinalizeDoesNotRepublishBytes(t *testing.T) {
	service, registry := newFileService(t)
	object, action, err := service.BeginDirect(t.Context(), "documents/reports", "report.txt")
	if err != nil {
		t.Fatal(err)
	}
	putTarget(t, service.TransferHandler(), *action.Target, "original")
	sealed := make(chan struct{})
	resume := make(chan struct{})
	registry.beforeMark = func() error { close(sealed); <-resume; return nil }
	result := make(chan error, 1)
	go func() { _, err := service.Finalize(t.Context(), object.ID, object.Prefix); result <- err }()
	<-sealed
	if err := service.Delete(t.Context(), object.ID, object.Prefix); err != nil {
		t.Fatal(err)
	}
	close(resume)
	if err := <-result; !errors.Is(err, ErrNotFound) {
		t.Fatalf("finalize after delete: %v", err)
	}
	keys, err := service.backend.expiredCandidates(t.Context(), time.Now().Add(time.Hour))
	if err != nil || len(keys) != 0 {
		t.Fatalf("candidate bytes after delete: %v %v", keys, err)
	}
}

func TestServiceWriteAndCleanup(t *testing.T) {
	service, registry := newFileService(t)
	body := `{"name":"production"}`
	written, err := service.Write(t.Context(), "munki/catalogues", "production.json", "APPLICATION/JSON", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if written.ContentType != "application/json" || readAvailable(t, service, *written) != body {
		t.Fatalf("write %#v", written)
	}
	abandoned, action, err := service.BeginDirect(t.Context(), "documents/reports", "abandoned.txt")
	if err != nil {
		t.Fatal(err)
	}
	putTarget(t, service.TransferHandler(), *action.Target, "abandoned")
	registry.mu.Lock()
	old := registry.objects[abandoned.ID]
	old.UpdatedAt = time.Now().Add(-48 * time.Hour)
	registry.objects[old.ID] = old
	registry.mu.Unlock()
	service.sweepExpiredUploads(t.Context())
	if _, err := registry.GetByID(t.Context(), abandoned.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("abandoned entry: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	service.DeleteUnreferenced(ctx, written.ID)
	if _, err := registry.GetByID(t.Context(), written.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unreferenced entry: %v", err)
	}
}

func TestFileAmbiguousPublishKeepsCommittedCandidate(t *testing.T) {
	service, registry := newFileService(t)
	object, action, err := service.BeginDirect(t.Context(), "documents/reports", "ambiguous.txt")
	if err != nil {
		t.Fatal(err)
	}
	putTarget(t, service.TransferHandler(), *action.Target, "committed bytes")
	registry.afterMark = func() error { return errors.New("commit response lost") }
	if _, err := service.Finalize(t.Context(), object.ID, object.Prefix); err == nil {
		t.Fatal("expected uncertain commit response")
	}
	putTarget(t, service.TransferHandler(), *action.Target, "late replay")
	available, err := service.Finalize(t.Context(), object.ID, object.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	if readAvailable(t, service, *available) != "committed bytes" {
		t.Fatal("retry replaced committed candidate")
	}
}

func TestCandidateCleanupResolvesRegistryOwnership(t *testing.T) {
	for _, kind := range []string{"file", "s3"} {
		t.Run(kind, func(t *testing.T) {
			var service *Service
			var ageCandidates func()
			old := time.Now().Add(-48 * time.Hour)
			if kind == "file" {
				service, _ = newFileService(t)
				ageCandidates = func() {
					keys, err := service.backend.expiredCandidates(t.Context(), time.Now().Add(time.Hour))
					if err != nil {
						t.Fatal(err)
					}
					file, ok := service.backend.(*fileStore)
					if !ok {
						t.Fatal("expected file backend")
					}
					for _, key := range keys {
						path, err := file.resolve(key)
						if err != nil {
							t.Fatal(err)
						}
						if err := os.Chtimes(path, old, old); err != nil {
							t.Fatal(err)
						}
					}
				}
			} else {
				var fixture *s3Fixture
				service, fixture = newS3Fixture(t)
				ageCandidates = func() {
					fixture.mu.Lock()
					defer fixture.mu.Unlock()
					for key, object := range fixture.objects {
						object.modified = old
						fixture.objects[key] = object
					}
				}
			}
			registry, ok := service.registry.(*memoryRegistry)
			if !ok {
				t.Fatal("expected memory registry")
			}
			available, err := service.Write(t.Context(), "documents/reports", "selected.txt", "text/plain", []byte("selected"))
			if err != nil {
				t.Fatal(err)
			}
			pending, _, err := service.BeginDirect(t.Context(), "documents/reports", "pending.txt")
			if err != nil {
				t.Fatal(err)
			}
			unused := candidateKey(available)
			inFlight := candidateKey(pending)
			orphan := candidateKey(&Object{ID: 9999, Filename: "orphan.txt"})
			for _, key := range []string{unused, inFlight, orphan} {
				if err := service.backend.Put(t.Context(), key, strings.NewReader("candidate"), putOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			ageCandidates()
			registry.getFailure = errors.New("registry unavailable")
			service.sweepExpiredUploads(t.Context())
			keys, err := service.backend.expiredCandidates(t.Context(), time.Now())
			if err != nil || len(keys) != 4 {
				t.Fatalf("cleanup removed bytes without resolving ownership: %v %v", keys, err)
			}
			registry.getFailure = nil
			service.sweepExpiredUploads(t.Context())
			keys, err = service.backend.expiredCandidates(t.Context(), time.Now())
			if err != nil || len(keys) != 2 {
				t.Fatalf("cleanup retained orphan candidates: %v %v", keys, err)
			}
			for _, key := range keys {
				if key != available.Key() && key != inFlight {
					t.Fatalf("unexpected retained candidate %q", key)
				}
			}
			if readAvailable(t, service, *available) != "selected" {
				t.Fatal("cleanup altered selected bytes")
			}
			registry.mu.Lock()
			object := registry.objects[pending.ID]
			object.UpdatedAt = old
			registry.objects[pending.ID] = object
			registry.mu.Unlock()
			service.sweepExpiredUploads(t.Context())
			keys, err = service.backend.expiredCandidates(t.Context(), time.Now())
			if err != nil || len(keys) != 1 || keys[0] != available.Key() {
				t.Fatalf("expired pending candidate remains: %v %v", keys, err)
			}
		})
	}
}

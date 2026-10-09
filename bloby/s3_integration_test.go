package bloby

import (
	"errors"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestS3ProviderInteroperability checks the guarantees Bloby takes from an S3
// provider against a real one. It runs when BLOBY_TEST_S3_ENDPOINT is set, with
// BLOBY_TEST_S3_REGION, BLOBY_TEST_S3_BUCKET, BLOBY_TEST_S3_ACCESS_KEY and
// BLOBY_TEST_S3_SECRET_KEY. BLOBY_TEST_S3_PATH_STYLE=false selects
// virtual-hosted bucket addressing.
//
// Cleanup sweeps are never run here: they assume a bucket dedicated to one
// registry, and this test may share its bucket.
func TestS3ProviderInteroperability(t *testing.T) {
	endpoint := os.Getenv("BLOBY_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("BLOBY_TEST_S3_ENDPOINT is not set")
	}
	registry := newMemoryRegistry()
	// Registry IDs lead object keys. Starting far above any database sequence
	// keeps this run's keys apart from other runs and from real objects.
	registry.nextID = 1<<62 + time.Now().UnixNano()
	service, err := New(t.Context(), registry, Config{Kind: KindS3, TransferTTL: time.Minute, S3: S3Config{
		Endpoint:  endpoint,
		Region:    os.Getenv("BLOBY_TEST_S3_REGION"),
		Bucket:    os.Getenv("BLOBY_TEST_S3_BUCKET"),
		AccessKey: os.Getenv("BLOBY_TEST_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("BLOBY_TEST_S3_SECRET_KEY"),
		PathStyle: os.Getenv("BLOBY_TEST_S3_PATH_STYLE") != "false",
	}}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	store, ok := service.backend.(*s3Store)
	if !ok {
		t.Fatal("expected S3 backend")
	}
	const prefix = "integration"
	var keys []string
	// track removes the object, its bytes and its upload when the subtest ends.
	track := func(t *testing.T, object *Object) {
		t.Helper()
		keys = append(keys, object.key())
		t.Cleanup(func() { service.DeleteUnreferenced(t.Context(), object.ID) })
	}
	refused := func(status int) bool { return status >= 400 && status < 500 }
	// swapped names the checksum of other bytes in place of the signed one.
	swapped := func(t *testing.T, target UploadTarget, name, checksum string) map[string]string {
		t.Helper()
		if _, signed := target.Headers[name]; !signed {
			t.Fatalf("target headers %v do not carry %s", target.Headers, name)
		}
		return map[string]string{name: checksum}
	}
	finalizeConcurrently := func(t *testing.T, object *Object) *Object {
		t.Helper()
		const finalizers = 4
		before := registry.published
		published := make(chan *Object, finalizers)
		var group sync.WaitGroup
		for range finalizers {
			group.Go(func() {
				available, err := service.Finalize(t.Context(), object.ID, prefix)
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
			t.FailNow()
		}
		first := <-published
		for available := range published {
			if !available.AvailableAt.Equal(*first.AvailableAt) {
				t.Fatalf("finalizers published different objects: %#v %#v", first, available)
			}
		}
		if registry.published != before+1 {
			t.Fatalf("published %d times", registry.published-before)
		}
		return first
	}

	t.Run("direct upload admits only the declared bytes", func(t *testing.T) {
		body := "%PDF-1.7\n" + strings.Repeat("declared direct bytes ", 512)
		other := strings.ToUpper(body)
		object, action, err := service.BeginDirect(t.Context(), prefix, "report.pdf", declare(body))
		if err != nil {
			t.Fatal(err)
		}
		track(t, object)
		target := *action.Target
		otherChecksum := swapped(t, target, "X-Amz-Checksum-Sha256", sha256Base64(other))

		for name, status := range map[string]int{
			"other bytes":                      upload(t, service, target, other),
			"declared bytes without a header":  sendS3Upload(t, target, nil, body),
			"other bytes under their checksum": sendS3Upload(t, target, otherChecksum, other),
			"a longer body":                    upload(t, service, target, body+"!"),
		} {
			if !refused(status) {
				t.Fatalf("%s: status %d, want a refusal", name, status)
			}
		}
		_, err = service.Finalize(t.Context(), object.ID, prefix)
		assertNotArrived(t, err)

		mustUpload(t, service, target, body)
		available := finalizeConcurrently(t, object)
		if available.ContentType != "application/pdf" || available.Key() != object.key() {
			t.Fatalf("published %#v", available)
		}

		for name, status := range map[string]int{
			"other bytes":                      upload(t, service, target, other),
			"other bytes under their checksum": sendS3Upload(t, target, otherChecksum, other),
		} {
			if !refused(status) {
				t.Fatalf("replay with %s: status %d, want a refusal", name, status)
			}
		}
		if got := readAvailable(t, service, *available); got != body {
			t.Fatal("replay changed the published bytes")
		}

		// A download URL works as a bare link.
		link, err := service.DownloadURL(t.Context(), *available, 0, DeliveryOptions{CacheControl: "private, max-age=60"})
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, link, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		downloaded, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != http.StatusOK || string(downloaded) != body {
			t.Fatalf("download: status %d, %d bytes, %v", response.StatusCode, len(downloaded), err)
		}
		if got := response.Header.Get("Content-Type"); got != "application/pdf" {
			t.Fatalf("download content type %q", got)
		}
	})

	t.Run("empty object", func(t *testing.T) {
		object, action, err := service.BeginDirect(t.Context(), prefix, "empty.txt", declare(""))
		if err != nil {
			t.Fatal(err)
		}
		track(t, object)
		if status := upload(t, service, *action.Target, "not empty"); !refused(status) {
			t.Fatalf("undeclared bytes: status %d, want a refusal", status)
		}
		mustUpload(t, service, *action.Target, "")
		available, err := service.Finalize(t.Context(), object.ID, prefix)
		if err != nil {
			t.Fatal(err)
		}
		if got := readAvailable(t, service, *available); got != "" {
			t.Fatalf("published %d bytes", len(got))
		}
	})

	t.Run("server write", func(t *testing.T) {
		const body = `{"catalog":"production"}`
		written, err := service.Write(t.Context(), prefix, "catalog.json", "application/json", []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		track(t, written)
		if got := readAvailable(t, service, *written); got != body {
			t.Fatalf("written %q", got)
		}
	})

	t.Run("multipart upload is assembled and verified by finalize", func(t *testing.T) {
		// Providers require every part but the last to be at least 5 MiB, and
		// some require those parts to share one size.
		const partSize = 5 << 20
		parts := []string{strings.Repeat("a", partSize), strings.Repeat("b", partSize), "tail of the upload"}
		whole := strings.Join(parts, "")
		object := beginMultipart(t, service, prefix, "installer.pkg", declare(whole))
		track(t, object)

		tail := partTarget(t, service, object, 3, parts[2])
		other := strings.ToUpper(parts[2])
		for name, status := range map[string]int{
			"other bytes":                      upload(t, service, tail, other),
			"declared bytes without a header":  sendS3Upload(t, tail, nil, parts[2]),
			"other bytes under their checksum": sendS3Upload(t, tail, swapped(t, tail, "X-Amz-Checksum-Crc64nvme", crc64Base64(other)), other),
		} {
			if !refused(status) {
				t.Fatalf("part with %s: status %d, want a refusal", name, status)
			}
		}
		_, err := service.Finalize(t.Context(), object.ID, prefix)
		assertNotArrived(t, err)

		// Parts arrive out of order, and the first is sent twice.
		mustUpload(t, service, tail, parts[2])
		first := mustUploadPart(t, service, object, 1, parts[0])
		mustUpload(t, service, first, parts[0])
		_, err = service.Finalize(t.Context(), object.ID, prefix)
		assertContentMismatch(t, err)
		mustUploadPart(t, service, object, 2, parts[1])

		available := finalizeConcurrently(t, object)
		if got := readAvailable(t, service, *available); got != whole {
			t.Fatal("assembled bytes differ from the uploaded parts")
		}
		if status := upload(t, service, tail, parts[2]); status != http.StatusNotFound {
			t.Fatalf("part replay after assembly: status %d, want %d", status, http.StatusNotFound)
		}
		again, err := service.Finalize(t.Context(), object.ID, prefix)
		if err != nil || !again.AvailableAt.Equal(*available.AvailableAt) {
			t.Fatalf("finalize retry: %#v %v", again, err)
		}
	})

	t.Run("multipart upload of undeclared content is refused", func(t *testing.T) {
		const declared, uploaded = "declared content", "uploaded content"
		object := beginMultipart(t, service, prefix, "installer.pkg", declare(declared))
		track(t, object)
		// The part matches its own signed checksum; only the whole differs.
		mustUploadPart(t, service, object, 1, uploaded)
		for range 2 {
			_, err := service.Finalize(t.Context(), object.ID, prefix)
			assertContentMismatch(t, err)
		}
		stored, err := registry.GetByID(t.Context(), object.ID)
		if err != nil || stored.Available() {
			t.Fatalf("undeclared content published: %#v %v", stored, err)
		}
	})

	t.Run("finalize after a completion whose response was lost", func(t *testing.T) {
		const body = "assembled before the response was lost"
		object := beginMultipart(t, service, prefix, "installer.pkg", declare(body))
		track(t, object)
		mustUploadPart(t, service, object, 1, body)
		stored, err := registry.GetByID(t.Context(), object.ID)
		if err != nil || stored.MultipartUploadID == nil {
			t.Fatalf("pending upload %#v: %v", stored, err)
		}
		// The provider assembles the upload while the registry still records it.
		if err := store.CompleteMultipartUpload(t.Context(), object.key(), *stored.MultipartUploadID, int64(len(body))); err != nil {
			t.Fatal(err)
		}
		available, err := service.Finalize(t.Context(), object.ID, prefix)
		if err != nil {
			t.Fatal(err)
		}
		if got := readAvailable(t, service, *available); got != body {
			t.Fatalf("assembled %q", got)
		}
	})

	t.Run("nothing is left behind", func(t *testing.T) {
		for _, key := range keys {
			if err := store.verify(t.Context(), key, Content{}); !errors.Is(err, ErrObjectNotFound) {
				t.Errorf("%s remains: %v", key, err)
			}
		}
		uploads, err := store.expiredUploads(t.Context(), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		for _, upload := range uploads {
			if slices.Contains(keys, upload.Key) {
				t.Errorf("upload %s for %s remains", upload.ID, upload.Key)
			}
		}
	})
}

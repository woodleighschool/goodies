package bloby

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// s3Fixture exercises SDK requests against the provider's HTTP contract. The
// object data remains small; advertisedSize models multipart copy ranges over
// the 5 GiB boundary without allocating a multi-gigabyte test object.
type s3FixtureObject struct {
	body           string
	etag           string
	modified       time.Time
	advertisedSize int64
}
type s3FixtureUpload struct {
	key       string
	parts     map[int]s3FixtureObject
	initiated time.Time
}
type s3Fixture struct {
	mu              sync.Mutex
	objects         map[string]s3FixtureObject
	uploads         map[string]*s3FixtureUpload
	nextID          int
	before          func(*http.Request)
	copyRanges      []string
	conditionalGets int
	completed       int
}

func newS3Fixture(t *testing.T) (*Service, *s3Fixture) {
	t.Helper()
	fixture := &s3Fixture{objects: make(map[string]s3FixtureObject), uploads: make(map[string]*s3FixtureUpload)}
	server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	t.Cleanup(server.Close)
	service, err := New(t.Context(), newMemoryRegistry(), Config{Kind: KindS3, TransferTTL: time.Minute, S3: S3Config{Bucket: "test", Region: "us-east-1", Endpoint: server.URL, PathStyle: true, AccessKey: "test", SecretKey: "test"}}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	return service, fixture
}

func (f *s3Fixture) set(key, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = s3FixtureObject{body: body, etag: `"` + hashString(body) + `"`, modified: time.Now()}
}

func s3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><Message>%s</Message></Error>", code, code)
}

func (f *s3Fixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if f.before != nil {
		f.before(r)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.TrimPrefix(r.URL.Path, "/test/")
	query := r.URL.Query()
	w.Header().Set("Content-Type", "application/xml")
	if r.Method == http.MethodGet && query.Get("list-type") == "2" {
		_, _ = io.WriteString(w, "<ListBucketResult><IsTruncated>false</IsTruncated>")
		for key, object := range f.objects {
			if strings.HasPrefix(key, query.Get("prefix")) {
				_, _ = fmt.Fprintf(w, "<Contents><Key>%s</Key><LastModified>%s</LastModified></Contents>", key, object.modified.UTC().Format(time.RFC3339))
			}
		}
		_, _ = io.WriteString(w, "</ListBucketResult>")
		return
	}
	if r.Method == http.MethodGet && query.Has("uploads") {
		_, _ = io.WriteString(w, "<ListMultipartUploadsResult><IsTruncated>false</IsTruncated>")
		for id, upload := range f.uploads {
			_, _ = fmt.Fprintf(w, "<Upload><Key>%s</Key><UploadId>%s</UploadId><Initiated>%s</Initiated></Upload>", upload.key, id, upload.initiated.UTC().Format(time.RFC3339))
		}
		_, _ = io.WriteString(w, "</ListMultipartUploadsResult>")
		return
	}
	if r.Method == http.MethodPost && query.Has("uploads") {
		f.nextID++
		id := strconv.Itoa(f.nextID)
		f.uploads[id] = &s3FixtureUpload{key: key, parts: make(map[int]s3FixtureObject), initiated: time.Now()}
		_, _ = fmt.Fprintf(w, "<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>", id)
		return
	}
	if id := query.Get("uploadId"); id != "" {
		upload, ok := f.uploads[id]
		if !ok || upload.key != key {
			s3Error(w, 404, "NoSuchUpload")
			return
		}
		switch r.Method {
		case http.MethodDelete:
			delete(f.uploads, id)
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPut:
			number, err := strconv.Atoi(query.Get("partNumber"))
			if err != nil {
				s3Error(w, 400, "InvalidArgument")
				return
			}
			var part s3FixtureObject
			if copySource := r.Header.Get("X-Amz-Copy-Source"); copySource != "" {
				source, err := url.PathUnescape(strings.TrimPrefix(copySource, "test/"))
				if err != nil {
					s3Error(w, 400, "InvalidArgument")
					return
				}
				original, exists := f.objects[source]
				if !exists {
					s3Error(w, 404, "NoSuchKey")
					return
				}
				if r.Header.Get("X-Amz-Copy-Source-If-Match") != original.etag {
					s3Error(w, 412, "PreconditionFailed")
					return
				}
				f.copyRanges = append(f.copyRanges, r.Header.Get("X-Amz-Copy-Source-Range"))
				part = original
				_, _ = fmt.Fprintf(w, "<CopyPartResult><ETag>%s</ETag></CopyPartResult>", part.etag)
			} else {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					s3Error(w, 500, "InternalError")
					return
				}
				part = s3FixtureObject{body: string(body), etag: `"` + hashString(string(body)) + `"`, modified: time.Now()}
				w.Header().Set("ETag", part.etag)
			}
			upload.parts[number] = part
		case http.MethodPost:
			var completed struct {
				Parts []struct {
					Number int    `xml:"PartNumber"`
					ETag   string `xml:"ETag"`
				} `xml:"Part"`
			}
			if err := xml.NewDecoder(r.Body).Decode(&completed); err != nil {
				s3Error(w, 400, "MalformedXML")
				return
			}
			var body strings.Builder
			for _, part := range completed.Parts {
				stored, exists := upload.parts[part.Number]
				if !exists || part.ETag != stored.etag {
					s3Error(w, 400, "InvalidPart")
					return
				}
				body.WriteString(stored.body)
			}
			object := s3FixtureObject{body: body.String(), etag: `"` + hashString(body.String()) + `"`, modified: time.Now()}
			f.objects[key] = object
			delete(f.uploads, id)
			f.completed++
			_, _ = fmt.Fprintf(w, "<CompleteMultipartUploadResult><ETag>%s</ETag></CompleteMultipartUploadResult>", object.etag)
		default:
			s3Error(w, 400, "InvalidRequest")
		}
		return
	}
	switch r.Method {
	case http.MethodHead, http.MethodGet:
		object, ok := f.objects[key]
		if !ok {
			s3Error(w, 404, "NoSuchKey")
			return
		}
		if match := r.Header.Get("If-Match"); match != "" {
			f.conditionalGets++
			if match != object.etag {
				s3Error(w, 412, "PreconditionFailed")
				return
			}
		}
		size := int64(len(object.body))
		if r.Method == http.MethodHead && object.advertisedSize > 0 {
			size = object.advertisedSize
		}
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.Header().Set("ETag", object.etag)
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, object.body)
		}
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			s3Error(w, 500, "InternalError")
			return
		}
		object := s3FixtureObject{body: string(body), etag: `"` + hashString(string(body)) + `"`, modified: time.Now()}
		f.objects[key] = object
		w.Header().Set("ETag", object.etag)
	case http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		s3Error(w, 400, "InvalidRequest")
	}
}

func uploadS3Target(t *testing.T, target UploadTarget, body string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), target.Method, target.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range target.Headers {
		req.Header.Set(key, value)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		contents, _ := io.ReadAll(response.Body)
		t.Fatalf("upload status %d: %s", response.StatusCode, contents)
	}
	return response.Header.Get("ETag")
}

func TestS3DirectUploadReplayCannotChangePublishedObject(t *testing.T) {
	service, fixture := newS3Fixture(t)
	object, action, err := service.BeginDirect(t.Context(), "documents/reports", "report.txt")
	if err != nil {
		t.Fatal(err)
	}
	uploadS3Target(t, *action.Target, "original")
	available, err := service.Finalize(t.Context(), object.ID, object.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	uploadS3Target(t, *action.Target, "replay after publication")
	if got := readAvailable(t, service, *available); got != "original" {
		t.Fatalf("published %q", got)
	}
	if err := service.Delete(t.Context(), object.ID, object.Prefix); err != nil {
		t.Fatal(err)
	}
	uploadS3Target(t, *action.Target, "replay after deletion")
	fixture.mu.Lock()
	orphan := fixture.objects[object.stagingKey()]
	orphan.modified = time.Now().Add(-48 * time.Hour)
	fixture.objects[object.stagingKey()] = orphan
	fixture.mu.Unlock()
	service.sweepExpiredUploads(t.Context())
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.objects) != 0 || len(fixture.uploads) != 0 {
		t.Fatalf("orphan bytes or copies retained: %#v %#v", fixture.objects, fixture.uploads)
	}
	if fixture.conditionalGets != 1 {
		t.Fatalf("pinned small-object reads=%d", fixture.conditionalGets)
	}
}

func TestS3BeginAbortsMultipartWhenRegistrationFails(t *testing.T) {
	service, fixture := newS3Fixture(t)
	failure := errors.New("registry unavailable")
	service.registry = multipartRegistrationFailure{Registry: service.registry, err: failure}
	if _, _, err := service.Begin(t.Context(), "documents/reports", "report.txt", s3MultipartThreshold+1); !errors.Is(err, failure) {
		t.Fatalf("begin error = %v, want registry failure", err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.uploads) != 0 {
		t.Fatalf("failed registration retained %d provider uploads", len(fixture.uploads))
	}
}

type multipartRegistrationFailure struct {
	Registry
	err error
}

func (r multipartRegistrationFailure) RecordMultipartUploadID(context.Context, int64, string) error {
	return r.err
}

func TestS3MultipartCompletionAndReplay(t *testing.T) {
	service, fixture := newS3Fixture(t)
	object, action, err := service.Begin(t.Context(), "documents/reports", "report.txt", s3MultipartThreshold+1)
	if err != nil {
		t.Fatal(err)
	}
	if action.Strategy != StrategyMultipart || action.Target != nil {
		t.Fatalf("multipart action %#v", action)
	}
	part, err := service.PresignMultipartPart(t.Context(), object.ID, object.Prefix, 1)
	if err != nil {
		t.Fatal(err)
	}
	etag := uploadS3Target(t, part, "multipart original")
	parts := []CompletedPart{{PartNumber: 1, ETag: etag}}
	if _, err := service.Finalize(t.Context(), object.ID, object.Prefix); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("incomplete multipart finalized: %v", err)
	}
	if err := service.CompleteMultipart(t.Context(), object.ID, object.Prefix, parts); err != nil {
		t.Fatal(err)
	}
	if err := service.CompleteMultipart(t.Context(), object.ID, object.Prefix, parts); err != nil {
		t.Fatalf("completion retry: %v", err)
	}
	available, err := service.Finalize(t.Context(), object.ID, object.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.CompleteMultipart(t.Context(), object.ID, object.Prefix, parts); err != nil {
		t.Fatalf("completion retry after finalize: %v", err)
	}
	request, err := http.NewRequestWithContext(t.Context(), part.Method, part.URL, strings.NewReader("replayed part"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("replayed part status %d", response.StatusCode)
	}
	if got := readAvailable(t, service, *available); got != "multipart original" {
		t.Fatalf("published %q", got)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.completed != 1 {
		t.Fatalf("client multipart completions %d", fixture.completed)
	}
}

func TestS3SealingPinsSourceAcrossLargeCopyParts(t *testing.T) {
	for _, changeSource := range []bool{false, true} {
		t.Run(fmt.Sprintf("source changes=%t", changeSource), func(t *testing.T) {
			service, fixture := newS3Fixture(t)
			staging, key := "_staging/documents/1/large.bin", "documents/1/large.bin"
			fixture.set(staging, "original")
			fixture.mu.Lock()
			object := fixture.objects[staging]
			object.advertisedSize = s3CopyPartSize + 1
			fixture.objects[staging] = object
			fixture.mu.Unlock()
			fixture.before = func(r *http.Request) {
				if changeSource && r.Header.Get("X-Amz-Copy-Source") != "" && r.URL.Query().Get("partNumber") == "2" {
					fixture.set(staging, "changed")
				}
			}
			err := service.backend.seal(t.Context(), staging, key)
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if changeSource {
				if err == nil {
					t.Fatal("changing source was published")
				}
				if _, ok := fixture.objects[key]; ok {
					t.Fatal("mixed source copy exists")
				}
				if len(fixture.uploads) != 0 {
					t.Fatal("failed copy multipart upload was retained")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if got := strings.Join(fixture.copyRanges, ","); got != "bytes=0-5368709119,bytes=5368709120-5368709120" {
					t.Fatalf("copy ranges %q", got)
				}
				if fixture.completed != 1 {
					t.Fatal("large candidate was not completed")
				}
			}
		})
	}
}

func TestS3ConcurrentFinalizersSelectOneImmutableCandidate(t *testing.T) {
	service, fixture := newS3Fixture(t)
	testCompetingFinalizers(t, service)
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.objects) != 1 || len(fixture.uploads) != 0 {
		t.Fatalf("unselected candidates retained: %d objects, %d uploads", len(fixture.objects), len(fixture.uploads))
	}
}

func testCompetingFinalizers(t *testing.T, service *Service) {
	t.Helper()
	object, action, err := service.BeginDirect(t.Context(), "documents/reports", "competing.txt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.DeleteUnreferenced(t.Context(), object.ID) })
	registry, ok := service.registry.(*memoryRegistry)
	if !ok {
		t.Fatal("expected memory registry")
	}
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	registry.beforeMark = func() error { arrived <- struct{}{}; <-release; return nil }
	uploadS3Target(t, *action.Target, "first complete snapshot")
	type result struct {
		object *Object
		err    error
	}
	results := make(chan result, 2)
	awaitCandidate := func() {
		select {
		case <-arrived:
		case result := <-results:
			t.Fatalf("finalize failed before candidate selection: %v", result.err)
		}
	}
	go func() {
		object, err := service.Finalize(t.Context(), object.ID, object.Prefix)
		results <- result{object, err}
	}()
	awaitCandidate()
	uploadS3Target(t, *action.Target, "second complete snapshot")
	go func() {
		object, err := service.Finalize(t.Context(), object.ID, object.Prefix)
		results <- result{object, err}
	}()
	awaitCandidate()
	releaseOnce.Do(func() { close(release) })
	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("competing finalizers: %v, %v", first.err, second.err)
	}
	if first.object.Key() != second.object.Key() || first.object.SHA256Value() != second.object.SHA256Value() {
		t.Fatal("finalizers returned different selected objects")
	}
	body := readAvailable(t, service, *first.object)
	if body != "first complete snapshot" && body != "second complete snapshot" {
		t.Fatalf("mixed snapshot %q", body)
	}
	uploadS3Target(t, *action.Target, "replay after both finalizers")
	if readAvailable(t, service, *second.object) != body {
		t.Fatal("late upload changed chosen candidate")
	}
	if err := service.backend.Delete(t.Context(), object.stagingKey()); err != nil {
		t.Fatal(err)
	}
}

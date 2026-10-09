package bloby

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// s3Fixture serves the part of the S3 HTTP contract Bloby uses, with the checks
// real providers apply to it: a body must match the checksum header sent with
// it, a presigned request must carry every header its signature names, and a
// stored checksum is reported only when the request asks for it. Signatures
// themselves are not verified.
type s3Fixture struct {
	mu      sync.Mutex
	objects map[string]s3FixtureObject
	uploads map[string]*s3FixtureUpload
	nextID  int
	// partsPerPage caps one ListParts page, as providers cap theirs at 1,000.
	partsPerPage int
	// afterComplete runs once an upload is assembled, before its response.
	afterComplete func()
	completed     int
	// served counts the object bytes returned to GET requests.
	served int64
}

// s3FixtureObject holds only the checksums its writer had verified.
type s3FixtureObject struct {
	body         string
	modified     time.Time
	sha256       string
	crc64nvme    string
	checksumType string
}

type s3FixturePart struct {
	body      string
	crc64nvme string
}

type s3FixtureUpload struct {
	key           string
	parts         map[int]s3FixturePart
	initiated     time.Time
	fullObjectCRC bool
}

func newS3Fixture(t *testing.T) (*Service, *s3Fixture) {
	t.Helper()
	fixture := &s3Fixture{objects: make(map[string]s3FixtureObject), uploads: make(map[string]*s3FixtureUpload), partsPerPage: 1000}
	server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	t.Cleanup(server.Close)
	service, err := New(t.Context(), newMemoryRegistry(), Config{Kind: KindS3, TransferTTL: time.Minute, S3: S3Config{Bucket: "test", Region: "us-east-1", Endpoint: server.URL, PathStyle: true, AccessKey: "test", SecretKey: "test"}}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	return service, fixture
}

// plant stores an object as a writer that sent no checksum would.
func (f *s3Fixture) plant(key string, object s3FixtureObject) {
	f.mu.Lock()
	defer f.mu.Unlock()
	object.modified = time.Now()
	f.objects[key] = object
}

// open starts an upload outside Bloby and returns its ID.
func (f *s3Fixture) open(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := strconv.Itoa(f.nextID)
	f.uploads[id] = &s3FixtureUpload{key: key, parts: make(map[int]s3FixturePart), initiated: time.Now()}
	return id
}

// age backdates every object and incomplete upload.
func (f *s3Fixture) age(by time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for key, object := range f.objects {
		object.modified = object.modified.Add(-by)
		f.objects[key] = object
	}
	for _, upload := range f.uploads {
		upload.initiated = upload.initiated.Add(-by)
	}
}

// uploadKeys lists the key of every incomplete upload.
func (f *s3Fixture) uploadKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]string, 0, len(f.uploads))
	for _, upload := range f.uploads {
		keys = append(keys, upload.key)
	}
	slices.Sort(keys)
	return keys
}

func (f *s3Fixture) counts() (completed int, served int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.completed, f.served
}

func s3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><Message>%s</Message></Error>", code, code)
}

func xmlText(value string) string {
	var escaped strings.Builder
	_ = xml.EscapeText(&escaped, []byte(value))
	return escaped.String()
}

func s3ETag(body string) string { return `"` + hashString(body)[:32] + `"` }

func sha256Base64(body string) string {
	sum := sha256.Sum256([]byte(body))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func crc64Base64(body string) string { return base64Digest(declare(body).CRC64NVME) }

// missingSignedHeader names a header a presigned request's signature covers
// but the request left out. HTTP itself supplies Host and Content-Length.
func missingSignedHeader(r *http.Request) string {
	for name := range strings.SplitSeq(r.URL.Query().Get("X-Amz-SignedHeaders"), ";") {
		if name == "" || name == "host" || name == "content-length" {
			continue
		}
		if r.Header.Get(name) == "" {
			return name
		}
	}
	return ""
}

func (f *s3Fixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.TrimPrefix(r.URL.Path, "/test/")
	query := r.URL.Query()
	w.Header().Set("Content-Type", "application/xml")
	switch {
	case missingSignedHeader(r) != "":
		s3Error(w, http.StatusBadRequest, "InvalidRequest")
	case r.Method == http.MethodGet && query.Get("list-type") == "2":
		_, _ = io.WriteString(w, "<ListBucketResult><IsTruncated>false</IsTruncated>")
		for _, key := range slices.Sorted(maps.Keys(f.objects)) {
			if strings.HasPrefix(key, query.Get("prefix")) {
				_, _ = fmt.Fprintf(w, "<Contents><Key>%s</Key><LastModified>%s</LastModified></Contents>", xmlText(key), f.objects[key].modified.UTC().Format(time.RFC3339))
			}
		}
		_, _ = io.WriteString(w, "</ListBucketResult>")
	case r.Method == http.MethodGet && query.Has("uploads"):
		_, _ = io.WriteString(w, "<ListMultipartUploadsResult><IsTruncated>false</IsTruncated>")
		for _, id := range slices.Sorted(maps.Keys(f.uploads)) {
			upload := f.uploads[id]
			_, _ = fmt.Fprintf(w, "<Upload><Key>%s</Key><UploadId>%s</UploadId><Initiated>%s</Initiated></Upload>", xmlText(upload.key), id, upload.initiated.UTC().Format(time.RFC3339))
		}
		_, _ = io.WriteString(w, "</ListMultipartUploadsResult>")
	case r.Method == http.MethodPost && query.Has("uploads"):
		f.nextID++
		id := strconv.Itoa(f.nextID)
		f.uploads[id] = &s3FixtureUpload{
			key: key, parts: make(map[int]s3FixturePart), initiated: time.Now(),
			fullObjectCRC: r.Header.Get("x-amz-checksum-algorithm") == "CRC64NVME" && r.Header.Get("x-amz-checksum-type") == "FULL_OBJECT",
		}
		_, _ = fmt.Fprintf(w, "<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>", id)
	case query.Has("uploadId"):
		f.serveUpload(w, r, key)
	default:
		f.serveObject(w, r, key)
	}
}

func (f *s3Fixture) serveUpload(w http.ResponseWriter, r *http.Request, key string) {
	query := r.URL.Query()
	id := query.Get("uploadId")
	upload, ok := f.uploads[id]
	if !ok || upload.key != key {
		s3Error(w, http.StatusNotFound, "NoSuchUpload")
		return
	}
	switch r.Method {
	case http.MethodGet:
		marker, _ := strconv.Atoi(query.Get("part-number-marker"))
		numbers := slices.DeleteFunc(slices.Sorted(maps.Keys(upload.parts)), func(number int) bool { return number <= marker })
		page := numbers[:min(len(numbers), f.partsPerPage)]
		_, _ = fmt.Fprintf(w, "<ListPartsResult><IsTruncated>%t</IsTruncated>", len(page) < len(numbers))
		if len(page) < len(numbers) {
			_, _ = fmt.Fprintf(w, "<NextPartNumberMarker>%d</NextPartNumberMarker>", page[len(page)-1])
		}
		for _, number := range page {
			part := upload.parts[number]
			_, _ = fmt.Fprintf(w, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag><Size>%d</Size><ChecksumCRC64NVME>%s</ChecksumCRC64NVME></Part>", number, xmlText(s3ETag(part.body)), len(part.body), part.crc64nvme)
		}
		_, _ = io.WriteString(w, "</ListPartsResult>")
	case http.MethodDelete:
		delete(f.uploads, id)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPut:
		number, err := strconv.Atoi(query.Get("partNumber"))
		if err != nil {
			s3Error(w, http.StatusBadRequest, "InvalidArgument")
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			s3Error(w, http.StatusInternalServerError, "InternalError")
			return
		}
		part := s3FixturePart{body: string(body), crc64nvme: r.Header.Get("x-amz-checksum-crc64nvme")}
		switch {
		// Garage refuses a part without a checksum on such an upload; R2 accepts it.
		case part.crc64nvme == "" && upload.fullObjectCRC:
			s3Error(w, http.StatusBadRequest, "InvalidRequest")
			return
		case part.crc64nvme != "" && part.crc64nvme != crc64Base64(part.body):
			s3Error(w, http.StatusBadRequest, "BadDigest")
			return
		}
		upload.parts[number] = part
		w.Header().Set("ETag", s3ETag(part.body))
	case http.MethodPost:
		var completion struct {
			Parts []struct {
				Number    int    `xml:"PartNumber"`
				ETag      string `xml:"ETag"`
				CRC64NVME string `xml:"ChecksumCRC64NVME"`
			} `xml:"Part"`
		}
		if err := xml.NewDecoder(r.Body).Decode(&completion); err != nil || len(completion.Parts) == 0 {
			s3Error(w, http.StatusBadRequest, "MalformedXML")
			return
		}
		var body strings.Builder
		previous := 0
		for _, part := range completion.Parts {
			stored, exists := upload.parts[part.Number]
			if part.Number <= previous {
				s3Error(w, http.StatusBadRequest, "InvalidPartOrder")
				return
			}
			if !exists || part.ETag != s3ETag(stored.body) || (part.CRC64NVME != "" && part.CRC64NVME != stored.crc64nvme) {
				s3Error(w, http.StatusBadRequest, "InvalidPart")
				return
			}
			previous = part.Number
			body.WriteString(stored.body)
		}
		object := s3FixtureObject{body: body.String(), modified: time.Now()}
		if upload.fullObjectCRC {
			object.crc64nvme = crc64Base64(object.body)
		}
		f.objects[key] = object
		delete(f.uploads, id)
		f.completed++
		if f.afterComplete != nil {
			f.afterComplete()
		}
		_, _ = fmt.Fprintf(w, "<CompleteMultipartUploadResult><ETag>%s</ETag></CompleteMultipartUploadResult>", xmlText(s3ETag(object.body)))
	default:
		s3Error(w, http.StatusBadRequest, "InvalidRequest")
	}
}

func (f *s3Fixture) serveObject(w http.ResponseWriter, r *http.Request, key string) {
	switch r.Method {
	case http.MethodHead, http.MethodGet:
		object, ok := f.objects[key]
		if !ok {
			s3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		body, status := object.body, http.StatusOK
		if requested := r.Header.Get("Range"); requested != "" {
			var first, last int
			if _, err := fmt.Sscanf(requested, "bytes=%d-%d", &first, &last); err != nil || first >= len(body) {
				s3Error(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange")
				return
			}
			last = min(last, len(body)-1)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, len(body)))
			body, status = body[first:last+1], http.StatusPartialContent
		}
		if r.Header.Get("x-amz-checksum-mode") == "ENABLED" && status == http.StatusOK {
			for name, value := range map[string]string{
				"x-amz-checksum-sha256":    object.sha256,
				"x-amz-checksum-crc64nvme": object.crc64nvme,
				"x-amz-checksum-type":      object.checksumType,
			} {
				if value != "" {
					w.Header().Set(name, value)
				}
			}
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("ETag", s3ETag(object.body))
		w.WriteHeader(status)
		if r.Method == http.MethodGet {
			f.served += int64(len(body))
			_, _ = io.WriteString(w, body)
		}
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			s3Error(w, http.StatusInternalServerError, "InternalError")
			return
		}
		object := s3FixtureObject{
			body: string(body), modified: time.Now(),
			sha256: r.Header.Get("x-amz-checksum-sha256"), crc64nvme: r.Header.Get("x-amz-checksum-crc64nvme"),
		}
		if (object.sha256 != "" && object.sha256 != sha256Base64(object.body)) || (object.crc64nvme != "" && object.crc64nvme != crc64Base64(object.body)) {
			s3Error(w, http.StatusBadRequest, "BadDigest")
			return
		}
		f.objects[key] = object
		w.Header().Set("ETag", s3ETag(object.body))
	case http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		s3Error(w, http.StatusBadRequest, "InvalidRequest")
	}
}

// sendS3Upload puts body to a presigned target with headers and returns the status.
func sendS3Upload(t *testing.T, target UploadTarget, headers map[string]string, body string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), target.Method, target.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

// beginMultipart reserves a multipart object as Begin does for content above
// the multipart threshold, so tests can assemble uploads from small parts.
func beginMultipart(t *testing.T, service *Service, prefix, filename string, content Content) *Object {
	t.Helper()
	object, err := service.createPending(t.Context(), prefix, filename, content)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.createMultipart(t.Context(), object); err != nil {
		t.Fatal(err)
	}
	return object
}

// partTarget signs the upload of body as one part.
func partTarget(t *testing.T, service *Service, object *Object, number int32, body string) UploadTarget {
	t.Helper()
	target, err := service.PresignMultipartPart(t.Context(), object.ID, object.Prefix, number, declare(body).CRC64NVME)
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func mustUploadPart(t *testing.T, service *Service, object *Object, number int32, body string) UploadTarget {
	t.Helper()
	target := partTarget(t, service, object, number, body)
	mustUpload(t, service, target, body)
	return target
}

func memoryRegistryOf(t *testing.T, service *Service) *memoryRegistry {
	t.Helper()
	registry, ok := service.registry.(*memoryRegistry)
	if !ok {
		t.Fatal("expected memory registry")
	}
	return registry
}

func TestS3DirectUploadReplayCannotChangePublishedObject(t *testing.T) {
	service, fixture := newS3Fixture(t)
	registry := memoryRegistryOf(t, service)
	original := "%PDF-1.7\n" + strings.Repeat("original document ", 1024)
	object, action, err := service.BeginDirect(t.Context(), "documents/reports", "report.pdf", declare(original))
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.Finalize(t.Context(), object.ID, object.Prefix)
	assertNotArrived(t, err)
	if status := upload(t, service, *action.Target, strings.ToUpper(original)); status != http.StatusBadRequest {
		t.Fatalf("undeclared bytes status %d", status)
	}
	if status := sendS3Upload(t, *action.Target, nil, original); status != http.StatusBadRequest {
		t.Fatalf("upload without its signed checksum status %d", status)
	}
	if keys := storedKeys(t, service); len(keys) != 0 {
		t.Fatalf("refused uploads stored %v", keys)
	}
	_, err = service.Finalize(t.Context(), object.ID, object.Prefix)
	assertNotArrived(t, err)

	mustUpload(t, service, *action.Target, original)
	_, servedBefore := fixture.counts()
	available, err := service.Finalize(t.Context(), object.ID, object.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	if _, served := fixture.counts(); served-servedBefore != contentTypeHeadBytes {
		t.Fatalf("publishing read %d bytes of a %d byte object, want only its %d byte head", served-servedBefore, len(original), contentTypeHeadBytes)
	}
	if want := fmt.Sprintf("_objects/%d/report.pdf", object.ID); available.Key() != want || available.ContentType != "application/pdf" {
		t.Fatalf("published %#v", available)
	}

	if status := upload(t, service, *action.Target, "replay after publication"); status != http.StatusBadRequest {
		t.Fatalf("replay with other bytes status %d", status)
	}
	mustUpload(t, service, *action.Target, original)
	again, err := service.Finalize(t.Context(), object.ID, object.Prefix)
	if err != nil || !again.AvailableAt.Equal(*available.AvailableAt) || registry.published != 1 {
		t.Fatalf("finalize retry: %#v %v, published %d times", again, err, registry.published)
	}
	if got := readAvailable(t, service, *again); got != original {
		t.Fatal("replay changed the published bytes")
	}
}

func TestS3FinalizePublishesEmptyObject(t *testing.T) {
	service, fixture := newS3Fixture(t)
	object, action, err := service.Begin(t.Context(), "documents/reports", "empty.txt", declare(""))
	if err != nil {
		t.Fatal(err)
	}
	mustUpload(t, service, *action.Target, "")
	available, err := service.Finalize(t.Context(), object.ID, object.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	if _, served := fixture.counts(); served != 0 || available.SizeBytesValue() != 0 {
		t.Fatalf("empty object %#v read %d bytes", available, served)
	}
	if got := readAvailable(t, service, *available); got != "" {
		t.Fatalf("published %q", got)
	}
}

func TestS3FinalizeTrustsOnlyAMatchingProviderChecksum(t *testing.T) {
	const declared = "declared content"
	for _, test := range []struct {
		name     string
		stored   s3FixtureObject
		mismatch bool
	}{
		{name: "other bytes of the declared size", stored: s3FixtureObject{body: "uploaded content", sha256: sha256Base64("uploaded content")}, mismatch: true},
		{name: "another size", stored: s3FixtureObject{body: declared + "!", sha256: sha256Base64(declared)}, mismatch: true},
		{name: "full-object CRC of other bytes", stored: s3FixtureObject{body: "uploaded content", crc64nvme: crc64Base64("uploaded content")}, mismatch: true},
		{name: "no checksum", stored: s3FixtureObject{body: declared}},
		{name: "composite checksum", stored: s3FixtureObject{body: declared, crc64nvme: crc64Base64(declared), checksumType: "COMPOSITE"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, fixture := newS3Fixture(t)
			registry := memoryRegistryOf(t, service)
			object, _, err := service.BeginDirect(t.Context(), "documents/reports", "report.txt", declare(declared))
			if err != nil {
				t.Fatal(err)
			}
			fixture.plant(object.key(), test.stored)
			_, err = service.Finalize(t.Context(), object.ID, object.Prefix)
			switch {
			case test.mismatch:
				assertContentMismatch(t, err)
			case err == nil || errors.Is(err, ErrInvalidInput):
				// The uploader did nothing wrong when storage cannot vouch for the bytes.
				t.Fatalf("finalize error = %v, want a storage failure", err)
			}
			if _, served := fixture.counts(); registry.published != 0 || served != 0 {
				t.Fatalf("unverified object: published=%d, read %d bytes", registry.published, served)
			}
		})
	}
}

func TestS3BeginCreatesFullObjectChecksumUpload(t *testing.T) {
	service, fixture := newS3Fixture(t)
	registry := memoryRegistryOf(t, service)
	content := declare("installer")
	content.SizeBytes = s3MultipartThreshold + 1
	object, action, err := service.Begin(t.Context(), "munki/packages", "Installer.pkg", content)
	if err != nil {
		t.Fatal(err)
	}
	if action.Strategy != StrategyMultipart || action.Target != nil {
		t.Fatalf("action %#v", action)
	}
	stored, err := registry.GetByID(t.Context(), object.ID)
	if err != nil || stored.MultipartUploadID == nil {
		t.Fatalf("stored object %#v, %v", stored, err)
	}
	fixture.mu.Lock()
	upload := fixture.uploads[*stored.MultipartUploadID]
	fixture.mu.Unlock()
	if upload == nil || upload.key != object.key() || !upload.fullObjectCRC {
		t.Fatalf("provider upload %#v", upload)
	}
}

func TestS3BeginAbortsMultipartWhenRegistrationFails(t *testing.T) {
	service, fixture := newS3Fixture(t)
	registry := memoryRegistryOf(t, service)
	failure := errors.New("registry unavailable")
	service.registry = multipartRegistrationFailure{Registry: registry, err: failure}
	content := declare("installer")
	content.SizeBytes = s3MultipartThreshold + 1
	if _, _, err := service.Begin(t.Context(), "documents/reports", "report.txt", content); !errors.Is(err, failure) {
		t.Fatalf("begin error = %v, want registry failure", err)
	}
	if uploads := fixture.uploadKeys(); len(uploads) != 0 || len(registry.objects) != 0 {
		t.Fatalf("failed registration retained uploads %v and %d objects", uploads, len(registry.objects))
	}
}

type multipartRegistrationFailure struct {
	Registry
	err error
}

func (r multipartRegistrationFailure) RecordMultipartUploadID(context.Context, int64, string) error {
	return r.err
}

func TestS3FinalizeAssemblesMultipartUpload(t *testing.T) {
	service, fixture := newS3Fixture(t)
	registry := memoryRegistryOf(t, service)
	fixture.partsPerPage = 2
	parts := []string{"first part, ", "second part, ", "third part, ", "fourth part, ", "tail"}
	whole := strings.Join(parts, "")
	object := beginMultipart(t, service, "documents/reports", "report.txt", declare(whole))

	// Parts arrive out of order, and the second is replaced after a first attempt.
	mustUploadPart(t, service, object, 2, "SECOND PART, ")
	var first UploadTarget
	for _, number := range []int32{4, 2, 5, 1, 3} {
		target := mustUploadPart(t, service, object, number, parts[number-1])
		if number == 1 {
			first = target
		}
	}
	available, err := service.Finalize(t.Context(), object.ID, object.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAvailable(t, service, *available); got != whole {
		t.Fatalf("assembled %q", got)
	}
	if available.MultipartUploadID != nil || available.Key() != object.key() {
		t.Fatalf("published %#v", available)
	}

	if status := upload(t, service, first, parts[0]); status != http.StatusNotFound {
		t.Fatalf("part replay after assembly status %d", status)
	}
	if _, err := service.PresignMultipartPart(t.Context(), object.ID, object.Prefix, 1, declare(parts[0]).CRC64NVME); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("part signed after publication: %v", err)
	}
	again, err := service.Finalize(t.Context(), object.ID, object.Prefix)
	if err != nil || !again.AvailableAt.Equal(*available.AvailableAt) {
		t.Fatalf("finalize retry: %#v %v", again, err)
	}
	if completed, _ := fixture.counts(); completed != 1 || registry.published != 1 || len(fixture.uploadKeys()) != 0 {
		t.Fatalf("completed %d uploads, published %d times, open uploads %v", completed, registry.published, fixture.uploadKeys())
	}
}

func TestS3PartMustMatchItsSignedChecksum(t *testing.T) {
	service, _ := newS3Fixture(t)
	const body = "declared part"
	object := beginMultipart(t, service, "documents/reports", "report.txt", declare(body))
	target := partTarget(t, service, object, 1, body)
	if status := upload(t, service, target, "another part!"); status != http.StatusBadRequest {
		t.Fatalf("undeclared part status %d", status)
	}
	if status := sendS3Upload(t, target, nil, body); status != http.StatusBadRequest {
		t.Fatalf("part without its signed checksum status %d", status)
	}
	_, err := service.Finalize(t.Context(), object.ID, object.Prefix)
	assertNotArrived(t, err)

	mustUpload(t, service, target, body)
	available, err := service.Finalize(t.Context(), object.ID, object.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAvailable(t, service, *available); got != body {
		t.Fatalf("assembled %q", got)
	}
}

func TestS3FinalizeRefusesPartsOfTheWrongTotalSize(t *testing.T) {
	service, fixture := newS3Fixture(t)
	parts := []string{"first part, ", "second part"}
	whole := strings.Join(parts, "")
	object := beginMultipart(t, service, "documents/reports", "report.txt", declare(whole))
	mustUploadPart(t, service, object, 1, parts[0])

	_, err := service.Finalize(t.Context(), object.ID, object.Prefix)
	assertContentMismatch(t, err)
	if completed, _ := fixture.counts(); completed != 0 || len(fixture.uploadKeys()) != 1 || len(storedKeys(t, service)) != 0 {
		t.Fatalf("short upload was assembled: completed=%d uploads=%v", completed, fixture.uploadKeys())
	}

	// The upload stays open, so the uploader can send what is missing.
	mustUploadPart(t, service, object, 2, parts[1])
	available, err := service.Finalize(t.Context(), object.ID, object.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAvailable(t, service, *available); got != whole {
		t.Fatalf("assembled %q", got)
	}
}

func TestS3FinalizeRefusesAssembledObjectWithUndeclaredContent(t *testing.T) {
	service, _ := newS3Fixture(t)
	registry := memoryRegistryOf(t, service)
	const declared, uploaded = "declared content", "uploaded content"
	object := beginMultipart(t, service, "documents/reports", "report.txt", declare(declared))
	// Each part matches its own signed checksum; only the whole differs.
	mustUploadPart(t, service, object, 1, uploaded[:8])
	mustUploadPart(t, service, object, 2, uploaded[8:])

	for range 2 {
		_, err := service.Finalize(t.Context(), object.ID, object.Prefix)
		assertContentMismatch(t, err)
	}
	stored, err := registry.GetByID(t.Context(), object.ID)
	if err != nil || stored.Available() || registry.published != 0 {
		t.Fatalf("undeclared content published: %#v %v", stored, err)
	}
	if _, err := service.DownloadURL(t.Context(), *stored, 0, DeliveryOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("undeclared content download URL: %v", err)
	}
}

func TestS3FinalizeRetriesAfterLostCompletionResponse(t *testing.T) {
	service, fixture := newS3Fixture(t)
	registry := memoryRegistryOf(t, service)
	parts := []string{"first part, ", "second part"}
	whole := strings.Join(parts, "")
	object := beginMultipart(t, service, "documents/reports", "report.txt", declare(whole))
	for i, part := range parts {
		mustUploadPart(t, service, object, int32(i+1), part)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fixture.afterComplete = cancel
	if _, err := service.Finalize(ctx, object.ID, object.Prefix); !errors.Is(err, context.Canceled) {
		t.Fatalf("finalize error = %v, want a canceled completion", err)
	}
	fixture.afterComplete = nil
	stored, err := registry.GetByID(t.Context(), object.ID)
	if completed, _ := fixture.counts(); err != nil || stored.Available() || stored.MultipartUploadID == nil || completed != 1 {
		t.Fatalf("after lost response: %#v %v, completed=%d", stored, err, completed)
	}

	available, err := service.Finalize(t.Context(), object.ID, object.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAvailable(t, service, *available); got != whole {
		t.Fatalf("assembled %q", got)
	}
	if completed, _ := fixture.counts(); completed != 1 || available.MultipartUploadID != nil {
		t.Fatalf("completed %d uploads, published %#v", completed, available)
	}
}

func TestS3ConcurrentFinalizersAssembleOnce(t *testing.T) {
	service, fixture := newS3Fixture(t)
	registry := memoryRegistryOf(t, service)
	parts := []string{"first part, ", "second part"}
	whole := strings.Join(parts, "")
	object := beginMultipart(t, service, "documents/reports", "report.txt", declare(whole))
	for i, part := range parts {
		mustUploadPart(t, service, object, int32(i+1), part)
	}
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
	if completed, _ := fixture.counts(); completed != 1 || registry.published != 1 {
		t.Fatalf("completed %d uploads, published %d times", completed, registry.published)
	}
	if got := readAvailable(t, service, *first); got != whole {
		t.Fatalf("assembled %q", got)
	}
}

func TestS3DeleteAbortsPendingMultipartUpload(t *testing.T) {
	service, fixture := newS3Fixture(t)
	const body = "only part"
	object := beginMultipart(t, service, "documents/reports", "report.txt", declare(body))
	target := mustUploadPart(t, service, object, 1, body)
	if err := service.Delete(t.Context(), object.ID, object.Prefix); err != nil {
		t.Fatal(err)
	}
	if uploads := fixture.uploadKeys(); len(uploads) != 0 {
		t.Fatalf("deleted object kept uploads %v", uploads)
	}
	if status := upload(t, service, target, body); status != http.StatusNotFound {
		t.Fatalf("part upload after delete status %d", status)
	}
	if _, err := service.Finalize(t.Context(), object.ID, object.Prefix); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finalize after delete: %v", err)
	}
}

func TestPresignMultipartPartRejectsInvalidRequests(t *testing.T) {
	service, _ := newS3Fixture(t)
	const body = "only part"
	crc := declare(body).CRC64NVME
	multipart := beginMultipart(t, service, "documents/reports", "multipart.txt", declare(body))
	direct, _, err := service.BeginDirect(t.Context(), "documents/reports", "direct.txt", declare(body))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		object *Object
		prefix string
		number int32
		crc    string
		want   error
	}{
		{name: "part zero", object: multipart, prefix: multipart.Prefix, number: 0, crc: crc, want: ErrInvalidInput},
		{name: "part past the provider limit", object: multipart, prefix: multipart.Prefix, number: 10_001, crc: crc, want: ErrInvalidInput},
		{name: "no checksum", object: multipart, prefix: multipart.Prefix, number: 1, want: ErrInvalidInput},
		{name: "base64 checksum", object: multipart, prefix: multipart.Prefix, number: 1, crc: base64Digest(crc), want: ErrInvalidInput},
		{name: "wrong prefix", object: multipart, prefix: "documents/other", number: 1, crc: crc, want: ErrInvalidInput},
		{name: "direct upload", object: direct, prefix: direct.Prefix, number: 1, crc: crc, want: ErrInvalidInput},
		{name: "missing object", object: &Object{ID: 404}, prefix: multipart.Prefix, number: 1, crc: crc, want: ErrNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := service.PresignMultipartPart(t.Context(), test.object.ID, test.prefix, test.number, test.crc); !errors.Is(err, test.want) {
				t.Fatalf("presign error = %v, want %v", err, test.want)
			}
		})
	}
	t.Run("file backend", func(t *testing.T) {
		file, _ := newFileService(t)
		object, _, err := file.Begin(t.Context(), "documents/reports", "report.txt", declare(body))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.PresignMultipartPart(t.Context(), object.ID, object.Prefix, 1, crc); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("presign error = %v, want ErrInvalidInput", err)
		}
	})
}

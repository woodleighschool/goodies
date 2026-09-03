package bloby

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/woodleighschool/goodies/bloby/internal/capability"
)

func TestBlobGetServesBytesAndRanges(t *testing.T) {
	t.Parallel()
	store := newTransferTestFileStore(t)
	const key = "munki/packages/1/Installer.pkg"
	if err := store.Put(
		t.Context(),
		key,
		strings.NewReader("0123456789"),
		putOptions{},
	); err != nil {
		t.Fatalf("Put: %v", err)
	}
	router := newBlobTestRouter(store)
	token := signBlobCapability(t, blobCapabilityClaims{
		Op:          capability.OpGet,
		Key:         key,
		Exp:         time.Now().Add(time.Minute).Unix(),
		ContentType: "application/octet-stream",
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/storage/munki/packages/1/Installer.pkg?cap="+token, nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, http.StatusOK, rec.Body.String())
	}
	if rec.Body.String() != "0123456789" {
		t.Fatalf("body = %q, want full object", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("Content-Type = %q, want application/octet-stream", got)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/storage/munki/packages/1/Installer.pkg?cap="+token, nil)
	req.Header.Set("Range", "bytes=2-5")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("range status = %d, want %d; body = %q", rec.Code, http.StatusPartialContent, rec.Body.String())
	}
	if rec.Body.String() != "2345" {
		t.Fatalf("range body = %q, want 2345", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 2-5/10" {
		t.Fatalf("Content-Range = %q, want bytes 2-5/10", got)
	}
}

func TestBlobGetAcceptsEquivalentEscapingForSignedKey(t *testing.T) {
	t.Parallel()

	store := newTransferTestFileStore(t)
	const key = "munki/packages/38/Zoom-7.1.5 (84650).pkg"
	if err := store.Put(t.Context(), key, strings.NewReader("zoom"), putOptions{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	token := signBlobCapability(t, blobCapabilityClaims{
		Op:  capability.OpGet,
		Key: key,
		Exp: time.Now().Add(time.Minute).Unix(),
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		"/storage/munki/packages/38/Zoom-7.1.5%20(84650).pkg?cap="+token,
		nil,
	)
	newBlobTestRouter(store).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, http.StatusOK, rec.Body.String())
	}
	if rec.Body.String() != "zoom" {
		t.Fatalf("body = %q, want zoom", rec.Body.String())
	}
}

func TestBlobGetRejectsInvalidExpiredAndMissingObjects(t *testing.T) {
	t.Parallel()
	store := newTransferTestFileStore(t)
	router := newBlobTestRouter(store)
	expired := signBlobCapability(t, blobCapabilityClaims{
		Op:  capability.OpGet,
		Key: "munki/icons/1/icon.png",
		Exp: time.Now().Add(-time.Minute).Unix(),
	})
	missing := signBlobCapability(t, blobCapabilityClaims{
		Op:  capability.OpGet,
		Key: "munki/icons/1/icon.png",
		Exp: time.Now().Add(time.Minute).Unix(),
	})

	cases := []struct {
		name string
		cap  string
		want int
	}{
		{name: "invalid", cap: "invalid", want: http.StatusUnauthorized},
		{name: "expired", cap: expired, want: http.StatusGone},
		{name: "missing", cap: missing, want: http.StatusNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/storage/munki/icons/1/icon.png?cap="+tc.cap, nil)
			router.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %q", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestBlobPutWritesAndRejectsWrongOperation(t *testing.T) {
	t.Parallel()
	store := newTransferTestFileStore(t)
	router := newBlobTestRouter(store)
	key := "munki/icons/7/icon.png"
	putToken := signBlobCapability(t, blobCapabilityClaims{
		Op:  capability.OpPut,
		Key: key,
		Exp: time.Now().Add(time.Minute).Unix(),
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(),
		http.MethodPut,
		"/storage/munki/icons/7/icon.png?cap="+putToken,
		bytes.NewReader([]byte("png bytes")))

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	reader, _, err := store.Open(t.Context(), key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "png bytes" {
		t.Fatalf("stored bytes = %q, want png bytes", got)
	}

	getToken := signBlobCapability(t, blobCapabilityClaims{
		Op:  capability.OpGet,
		Key: key,
		Exp: time.Now().Add(time.Minute).Unix(),
	})
	rec = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(t.Context(),
		http.MethodPut,
		"/storage/munki/icons/7/icon.png?cap="+getToken,
		strings.NewReader("wrong"))

	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong op status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestBlobRejectsMismatchedPathAndSignedKey(t *testing.T) {
	t.Parallel()
	store := newTransferTestFileStore(t)
	router := newBlobTestRouter(store)
	token := signBlobCapability(t, blobCapabilityClaims{
		Op:  capability.OpGet,
		Key: "munki/icons/7/icon.png",
		Exp: time.Now().Add(time.Minute).Unix(),
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/storage/munki/icons/8/icon.png?cap="+token, nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestBlobGetLogsOpenFailures(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	h := transferHandler{
		store:  failingOpenStore{},
		key:    testCapabilityKey,
		logger: logger,
	}
	token := signBlobCapability(t, blobCapabilityClaims{
		Op:  capability.OpGet,
		Key: "munki/packages/1/Installer.pkg",
		Exp: time.Now().Add(time.Minute).Unix(),
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/storage/munki/packages/1/Installer.pkg?cap="+token, nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	line := logs.String()
	for _, want := range []string{
		`"msg":"storage blob handler failed"`,
		`"operation":"get-storage-object"`,
		`"status":500`,
		`"key":"munki/packages/1/Installer.pkg"`,
		`"err":"open object: backend unavailable"`,
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line %q does not contain %s", line, want)
		}
	}
}

func TestTransferRoutesAreNotMountedForS3(t *testing.T) {
	t.Parallel()
	backend, err := newBackend(t.Context(), Config{
		Kind:        KindS3,
		TransferTTL: time.Minute,
		S3: S3Config{
			Bucket:    "woodstar",
			Region:    "ap-southeast-2",
			Endpoint:  "https://uploads.example",
			AccessKey: "test-access-key",
			SecretKey: "test-secret-key",
			PathStyle: true,
		},
	})
	if err != nil {
		t.Fatalf("New S3 backend: %v", err)
	}
	handler := (&Service{backend: backend, logger: testLogger()}).TransferHandler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/storage/munki/packages/1/Installer.pkg", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func newBlobTestRouter(store backend) http.Handler {
	return (&Service{backend: store, logger: testLogger()}).TransferHandler()
}

func signBlobCapability(t *testing.T, claims blobCapabilityClaims) string {
	t.Helper()
	token, err := capability.Sign(testCapabilityKey, claims)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return token
}

func newTransferTestFileStore(t *testing.T) backend {
	t.Helper()
	store, err := newBackend(t.Context(), Config{
		Kind:        KindFile,
		TransferTTL: time.Minute,
		File: FileConfig{
			Root:             t.TempDir(),
			BaseURL:          "https://woodstar.example",
			CapabilityKeyHex: testCapabilityKeyHex,
		},
	})
	if err != nil {
		t.Fatalf("new storage backend: %v", err)
	}
	return store
}

type failingOpenStore struct{}

func (failingOpenStore) Open(_ context.Context, _ string) (io.ReadSeekCloser, objectInfo, error) {
	return nil, objectInfo{}, errors.New("backend unavailable")
}

func (failingOpenStore) Put(context.Context, string, io.Reader, putOptions) error {
	return errors.New("unexpected put")
}

func (failingOpenStore) Delete(context.Context, string) error {
	return nil
}

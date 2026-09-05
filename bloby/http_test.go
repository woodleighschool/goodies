package bloby

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBlobGetServesBytesAndRanges(t *testing.T) {
	t.Parallel()
	store := newTransferTestFileStore(t)
	const key = "munki/packages/1/Installer.pkg"
	if err := store.Put(t.Context(), key, strings.NewReader("0123456789"), putOptions{}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(newBlobTestRouter(store))
	t.Cleanup(server.Close)
	token := signBlobCapability(t, capabilityClaims{
		Op:          capabilityGet,
		Key:         key,
		Exp:         time.Now().Add(time.Minute).Unix(),
		ContentType: "application/octet-stream",
	})
	for _, test := range []struct {
		name         string
		rangeHeader  string
		status       int
		body         string
		contentRange string
	}{
		{name: "full object", status: http.StatusOK, body: "0123456789"},
		{name: "byte range", rangeHeader: "bytes=2-5", status: http.StatusPartialContent, body: "2345", contentRange: "bytes 2-5/10"},
	} {
		t.Run(test.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/storage/"+key+"?cap="+token, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Range", test.rangeHeader)
			response, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != test.status || string(body) != test.body {
				t.Fatalf("response = %d/%q, want %d/%q", response.StatusCode, body, test.status, test.body)
			}
			if got := response.Header.Get("Content-Range"); got != test.contentRange {
				t.Fatalf("Content-Range = %q, want %q", got, test.contentRange)
			}
			if got := response.Header.Get("Content-Type"); got != "application/octet-stream" {
				t.Fatalf("Content-Type = %q", got)
			}
		})
	}
}

func TestBlobGetAcceptsEquivalentEscapingForSignedKey(t *testing.T) {
	t.Parallel()

	store := newTransferTestFileStore(t)
	const key = "munki/packages/38/Zoom-7.1.5 (84650).pkg"
	if err := store.Put(t.Context(), key, strings.NewReader("zoom"), putOptions{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	token := signBlobCapability(t, capabilityClaims{
		Op:  capabilityGet,
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
	expired := signBlobCapability(t, capabilityClaims{
		Op:  capabilityGet,
		Key: "munki/icons/1/icon.png",
		Exp: time.Now().Add(-time.Minute).Unix(),
	})
	missing := signBlobCapability(t, capabilityClaims{
		Op:  capabilityGet,
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
	putToken := signBlobCapability(t, capabilityClaims{
		Op:  capabilityPut,
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

	getToken := signBlobCapability(t, capabilityClaims{
		Op:  capabilityGet,
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
	token := signBlobCapability(t, capabilityClaims{
		Op:  capabilityGet,
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
	store := newTestFileStore(t)
	if err := os.WriteFile(filepath.Join(store.root, "munki"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	h := transferHandler{
		store:  store,
		logger: logger,
	}
	token := signBlobCapability(t, capabilityClaims{
		Op:  capabilityGet,
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
		`"err":"open object: open`,
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

func signBlobCapability(t *testing.T, claims capabilityClaims) string {
	t.Helper()
	return signCapability(testCapabilityKey, claims)
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

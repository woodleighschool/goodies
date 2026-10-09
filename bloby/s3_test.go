package bloby

import (
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"
)

const testS3TransferTTL = 17 * time.Minute

func TestS3StoreSelectsUploadActionBySize(t *testing.T) {
	t.Parallel()
	store := newTestS3Store(t, time.Minute)

	for _, tt := range []struct {
		name          string
		sizeBytes     int64
		wantMultipart bool
	}{
		{name: "empty", sizeBytes: 0},
		{name: "threshold", sizeBytes: 100 * 1024 * 1024},
		{name: "above threshold", sizeBytes: 100*1024*1024 + 1, wantMultipart: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			content := declare("")
			content.SizeBytes = tt.sizeBytes
			action, err := store.beginUpload(t.Context(), "munki/packages/42/Installer.pkg", content)
			if err != nil {
				t.Fatalf("begin upload: %v", err)
			}
			multipart := action.Strategy == StrategyMultipart
			if multipart != tt.wantMultipart || multipart == (action.Target != nil) {
				t.Fatalf("action = %+v, want multipart %t", action, tt.wantMultipart)
			}
		})
	}
}

func TestS3StorePresignedPutBindsDeclaredContent(t *testing.T) {
	t.Parallel()
	store := newTestS3Store(t, testS3TransferTTL)
	for _, test := range []struct {
		name          string
		body          string
		signedHeaders string
	}{
		{name: "content", body: "installer bytes", signedHeaders: "content-length;host;x-amz-checksum-sha256"},
		// A zero length is not signed; only the empty body has the signed SHA-256.
		{name: "empty", body: "", signedHeaders: "host;x-amz-checksum-sha256"},
	} {
		t.Run(test.name, func(t *testing.T) {
			content := declare(test.body)
			target, err := store.PresignPut(t.Context(), "_objects/42/Installer.pkg", content, 0)
			if err != nil {
				t.Fatalf("PresignPut: %v", err)
			}
			if target.Method != http.MethodPut {
				t.Fatalf("method = %q, want %q", target.Method, http.MethodPut)
			}
			// A browser cannot set Host or Content-Length; it sends the rest unchanged.
			if want := map[string]string{"X-Amz-Checksum-Sha256": base64Digest(content.SHA256)}; !maps.Equal(target.Headers, want) {
				t.Fatalf("headers = %v, want %v", target.Headers, want)
			}
			parsed, err := url.Parse(target.URL)
			if err != nil {
				t.Fatalf("parse URL: %v", err)
			}
			if got := parsed.Query().Get("X-Amz-SignedHeaders"); got != test.signedHeaders {
				t.Fatalf("X-Amz-SignedHeaders = %q, want %q", got, test.signedHeaders)
			}
		})
	}
}

func TestS3StorePresignedPartBindsItsChecksum(t *testing.T) {
	t.Parallel()
	store := newTestS3Store(t, testS3TransferTTL)
	crc := declare("part bytes").CRC64NVME

	target, err := store.PresignMultipartPart(
		t.Context(),
		"_objects/42/Installer.pkg",
		"upload-id",
		7,
		crc,
		0,
	)
	if err != nil {
		t.Fatalf("presign multipart part: %v", err)
	}
	if target.Method != http.MethodPut {
		t.Fatalf("method = %q, want %q", target.Method, http.MethodPut)
	}
	if want := map[string]string{"X-Amz-Checksum-Crc64nvme": base64Digest(crc)}; !maps.Equal(target.Headers, want) {
		t.Fatalf("headers = %v, want %v", target.Headers, want)
	}
	parsed, err := url.Parse(target.URL)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	query := parsed.Query()
	if got := query.Get("uploadId"); got != "upload-id" {
		t.Fatalf("uploadId = %q, want upload-id", got)
	}
	if got := query.Get("partNumber"); got != "7" {
		t.Fatalf("partNumber = %q, want 7", got)
	}
	if got := query.Get("X-Amz-SignedHeaders"); got != "host;x-amz-checksum-crc64nvme" {
		t.Fatalf("X-Amz-SignedHeaders = %q, want the host and part checksum", got)
	}
}

func TestS3StoreTransferOriginMatchesPresignedPart(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  S3Config
	}{
		{
			name: "custom endpoint",
			cfg: S3Config{
				Bucket:    "woodstar",
				Region:    "ap-southeast-2",
				Endpoint:  "https://uploads.example",
				AccessKey: "test-access-key",
				SecretKey: "test-secret-key",
				PathStyle: true,
			},
		},
		{
			name: "AWS endpoint",
			cfg: S3Config{
				Bucket:    "woodstar",
				Region:    "ap-southeast-2",
				AccessKey: "test-access-key",
				SecretKey: "test-secret-key",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, err := newS3Store(t.Context(), tt.cfg, time.Minute)
			if err != nil {
				t.Fatalf("newS3Store: %v", err)
			}
			target, err := store.PresignMultipartPart(
				t.Context(),
				"munki/packages/42/Installer.pkg",
				"upload-id",
				1,
				declare("part bytes").CRC64NVME,
				time.Minute,
			)
			if err != nil {
				t.Fatalf("PresignMultipartPart: %v", err)
			}
			parsed, err := url.Parse(target.URL)
			if err != nil {
				t.Fatalf("parse target URL: %v", err)
			}
			if got, want := store.TransferOrigin(), parsed.Scheme+"://"+parsed.Host; got != want {
				t.Fatalf("transfer origin = %q, want presigned target origin %q", got, want)
			}
		})
	}
}

func TestS3StoreUsesConfiguredTransferTTL(t *testing.T) {
	t.Parallel()
	store := newTestS3Store(t, testS3TransferTTL)

	getURL, err := store.PresignGet(t.Context(), "munki/icons/7/icon.png", 0, getOptions{})
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	putTarget, err := store.PresignPut(t.Context(), "munki/packages/42/upload", declare("upload"), 0)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	partTarget, err := store.PresignMultipartPart(
		t.Context(),
		"munki/packages/42/Installer.pkg",
		"upload-id",
		1,
		declare("part bytes").CRC64NVME,
		0,
	)
	if err != nil {
		t.Fatalf("PresignMultipartPart: %v", err)
	}

	for name, rawURL := range map[string]string{
		"get":            getURL,
		"put":            putTarget.URL,
		"multipart part": partTarget.URL,
	} {
		t.Run(name, func(t *testing.T) {
			parsed, err := url.Parse(rawURL)
			if err != nil {
				t.Fatalf("parse URL: %v", err)
			}
			want := strconv.FormatInt(int64(testS3TransferTTL/time.Second), 10)
			if got := parsed.Query().Get("X-Amz-Expires"); got != want {
				t.Fatalf("X-Amz-Expires = %q, want %q", got, want)
			}
		})
	}
}

func TestS3StorePresignedGetRequiresNoHeaders(t *testing.T) {
	t.Parallel()
	store := newTestS3Store(t, time.Minute)

	rawURL, err := store.PresignGet(t.Context(), "munki/packages/42/Installer.pkg", 0, getOptions{ContentType: "application/octet-stream", CacheControl: "private"})
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse presigned URL: %v", err)
	}
	if got := parsed.Query().Get("X-Amz-SignedHeaders"); got != "host" {
		t.Fatalf("X-Amz-SignedHeaders = %q, want host", got)
	}
}

func newTestS3Store(t *testing.T, transferTTL time.Duration) *s3Store {
	t.Helper()
	store, err := newS3Store(t.Context(), S3Config{
		Bucket:    "woodstar",
		Region:    "ap-southeast-2",
		Endpoint:  "https://uploads.example",
		AccessKey: "test-access-key",
		SecretKey: "test-secret-key",
		PathStyle: true,
	}, transferTTL)
	if err != nil {
		t.Fatalf("newS3Store: %v", err)
	}
	return store
}

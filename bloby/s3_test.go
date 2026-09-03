package bloby

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"
)

const testS3TransferTTL = 17 * time.Minute

func TestS3StoreSelectsUploadActionBySize(t *testing.T) {
	t.Parallel()
	store, err := newS3Store(t.Context(), S3Config{
		Bucket:    "woodstar",
		Region:    "ap-southeast-2",
		Endpoint:  "https://uploads.example",
		AccessKey: "test-access-key",
		SecretKey: "test-secret-key",
		PathStyle: true,
	}, time.Minute)
	if err != nil {
		t.Fatalf("newS3Store: %v", err)
	}

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
			action, err := store.beginUpload(t.Context(), "munki/packages/42/Installer.pkg", tt.sizeBytes)
			if err != nil {
				t.Fatalf("begin upload: %v", err)
			}
			multipart := action.Strategy == StrategyMultipart
			if multipart != tt.wantMultipart {
				t.Fatalf("multipart action = %t, want %t", multipart, tt.wantMultipart)
			}
		})
	}
}

func TestS3StorePresignsMultipartPart(t *testing.T) {
	t.Parallel()
	store, err := newS3Store(t.Context(), S3Config{
		Bucket:    "woodstar",
		Region:    "ap-southeast-2",
		Endpoint:  "https://uploads.example",
		AccessKey: "test-access-key",
		SecretKey: "test-secret-key",
		PathStyle: true,
	}, testS3TransferTTL)
	if err != nil {
		t.Fatalf("newS3Store: %v", err)
	}

	target, err := store.PresignMultipartPart(
		t.Context(),
		"munki/packages/42/Installer.pkg",
		"upload-id",
		7,
		0,
	)
	if err != nil {
		t.Fatalf("presign multipart part: %v", err)
	}
	if target.Method != http.MethodPut {
		t.Fatalf("method = %q, want %q", target.Method, http.MethodPut)
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
	if got := query.Get("X-Amz-Expires"); got != strconv.FormatInt(int64(testS3TransferTTL/time.Second), 10) {
		t.Fatalf("X-Amz-Expires = %q, want configured TTL", got)
	}
	if got := query.Get("X-Amz-SignedHeaders"); got != "host" {
		t.Fatalf("X-Amz-SignedHeaders = %q, want host", got)
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
	store, err := newS3Store(t.Context(), S3Config{
		Bucket:    "woodstar",
		Region:    "ap-southeast-2",
		Endpoint:  "https://uploads.example",
		AccessKey: "test-access-key",
		SecretKey: "test-secret-key",
		PathStyle: true,
	}, testS3TransferTTL)
	if err != nil {
		t.Fatalf("newS3Store: %v", err)
	}

	getURL, err := store.PresignGet(t.Context(), "munki/icons/7/icon.png", 0, getOptions{})
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	putTarget, err := store.PresignPut(t.Context(), "munki/packages/42/upload", 0)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	partTarget, err := store.PresignMultipartPart(
		t.Context(),
		"munki/packages/42/Installer.pkg",
		"upload-id",
		1,
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
	store, err := newS3Store(t.Context(), S3Config{
		Bucket:    "woodstar",
		Region:    "ap-southeast-2",
		Endpoint:  "https://downloads.example",
		AccessKey: "test-access-key",
		SecretKey: "test-secret-key",
		PathStyle: true,
	}, time.Minute)
	if err != nil {
		t.Fatalf("newS3Store: %v", err)
	}

	rawURL, err := store.PresignGet(t.Context(), "munki/packages/42/Installer.pkg", 0, getOptions{})
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

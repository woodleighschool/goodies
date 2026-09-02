package bloby

import (
	"errors"
	"testing"
)

func TestNormalizeContentType(t *testing.T) {
	t.Parallel()

	got, err := normalizeContentType(`IMAGE/PNG; profile="screen"`)
	if err != nil {
		t.Fatalf("normalize valid content type: %v", err)
	}
	if got != "image/png; profile=screen" {
		t.Fatalf("content type = %q, want normalized media type", got)
	}

	if _, err := normalizeContentType("not a content type"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("normalize invalid content type error = %v, want ErrInvalidInput", err)
	}
}

func TestNormalizeMultipartUploadID(t *testing.T) {
	t.Parallel()

	got, err := normalizeMultipartUploadID("  upload-1  ")
	if err != nil {
		t.Fatalf("normalize multipart upload ID: %v", err)
	}
	if got != "upload-1" {
		t.Fatalf("multipart upload ID = %q, want upload-1", got)
	}

	if _, err := normalizeMultipartUploadID("  "); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("normalize blank multipart upload ID error = %v, want ErrInvalidInput", err)
	}
}

func TestValidateAvailableObjectMetadata(t *testing.T) {
	t.Parallel()

	if err := validateAvailableObjectMetadata(0, "sha256"); err != nil {
		t.Fatalf("validate complete metadata: %v", err)
	}
	for name, input := range map[string]struct {
		sizeBytes int64
		sha256sum string
	}{
		"negative size": {sizeBytes: -1, sha256sum: "sha256"},
		"blank hash":    {sizeBytes: 1, sha256sum: "  "},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := validateAvailableObjectMetadata(input.sizeBytes, input.sha256sum); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("validate metadata error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

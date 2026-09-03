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

func TestValidateAvailableObjectMetadata(t *testing.T) {
	t.Parallel()

	if err := validateAvailableObjectMetadata(0, hashString("example")); err != nil {
		t.Fatalf("validate complete metadata: %v", err)
	}
	for name, input := range map[string]struct {
		sizeBytes int64
		sha256sum string
	}{
		"negative size": {sizeBytes: -1, sha256sum: hashString("example")},
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

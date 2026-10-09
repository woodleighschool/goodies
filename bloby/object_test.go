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

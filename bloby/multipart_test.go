package bloby

import (
	"errors"
	"testing"
)

func TestValidateCompletedPartsRequiresStrictAscendingNonemptyParts(t *testing.T) {
	t.Parallel()
	valid := []CompletedPart{
		{PartNumber: 1, ETag: `"first"`},
		{PartNumber: 10_000, ETag: `"last"`},
	}
	if err := validateCompletedParts(valid); err != nil {
		t.Fatalf("validate ascending parts: %v", err)
	}
	for name, parts := range map[string][]CompletedPart{
		"empty":      nil,
		"zero":       {{PartNumber: 0, ETag: `"etag"`}},
		"too large":  {{PartNumber: 10_001, ETag: `"etag"`}},
		"blank etag": {{PartNumber: 1, ETag: "  "}},
		"duplicate":  {{PartNumber: 1, ETag: `"one"`}, {PartNumber: 1, ETag: `"again"`}},
		"descending": {{PartNumber: 2, ETag: `"two"`}, {PartNumber: 1, ETag: `"one"`}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateCompletedParts(parts); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("validate parts error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

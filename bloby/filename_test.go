package bloby

import (
	"errors"
	"testing"
)

func TestNormalizeUploadFilename(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"Firefox-120.0.dmg":   "Firefox-120.0.dmg",
		"/tmp/Firefox.dmg":    "Firefox.dmg",
		`C:\Users\me\App.pkg`: "App.pkg",
		"  Spaced.icns  ":     "Spaced.icns",
		"Acmé Café.png":       "Acmé Café.png",
		"sub/dir/file.pkg":    "file.pkg",
		"../../etc/passwd":    "passwd",
	}
	for in, want := range cases {
		got := normalizeUploadFilename(in)
		if got != want {
			t.Errorf("normalizeUploadFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateUploadFilenameRejects(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"",
		"   ",
		".",
		"..",
		"/",
		"a/b/..",
		"with\x00null.pkg",
		"invalid-\xff.pkg",
	} {
		name := normalizeUploadFilename(in)
		if err := validateUploadFilename(name); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("validateUploadFilename(%q) error = %v, want ErrInvalidInput", name, err)
		}
	}
}

func TestValidateUploadFilenameAcceptsURLSignificantCharacters(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"Zoom-7.1.5 (84650).pkg",
		"What?#100%.pkg",
		"Café (Français) [arm64]+.pkg",
	} {
		if err := validateUploadFilename(name); err != nil {
			t.Errorf("validateUploadFilename(%q) error = %v", name, err)
		}
	}
}

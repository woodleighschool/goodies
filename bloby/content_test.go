package bloby

import (
	"errors"
	"strings"
	"testing"
)

func TestDigestMatchesPublishedCheckValues(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		body string
		want Content
	}{
		{
			// The check input and CRC-64/NVME value from the CRC catalogue.
			name: "check string",
			body: "123456789",
			want: Content{
				SizeBytes: 9,
				SHA256:    "15e2b0d3c33891ebb0f1ef609ec419420c20e320ce94c65fbc8c3312448eb225",
				CRC64NVME: "ae8b14860a799888",
			},
		},
		{
			name: "empty",
			want: Content{
				SHA256:    "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
				CRC64NVME: "0000000000000000",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := Digest(strings.NewReader(test.body))
			if err != nil || got != test.want {
				t.Fatalf("Digest = %+v, %v; want %+v", got, err, test.want)
			}
			if err := got.validate(); err != nil {
				t.Fatalf("digest is not a valid declaration: %v", err)
			}
			crc, err := CRC64NVME(strings.NewReader(test.body))
			if err != nil || crc != test.want.CRC64NVME {
				t.Fatalf("CRC64NVME = %q, %v; want %q", crc, err, test.want.CRC64NVME)
			}
		})
	}
}

func TestBeginRejectsMalformedDeclarations(t *testing.T) {
	t.Parallel()
	valid := declare("payload")
	for name, change := range map[string]func(*Content){
		"negative size":     func(c *Content) { c.SizeBytes = -1 },
		"missing sha256":    func(c *Content) { c.SHA256 = "" },
		"uppercase sha256":  func(c *Content) { c.SHA256 = strings.ToUpper(c.SHA256) },
		"truncated sha256":  func(c *Content) { c.SHA256 = c.SHA256[:63] },
		"missing crc64nvme": func(c *Content) { c.CRC64NVME = "" },
		"base64 crc64nvme":  func(c *Content) { c.CRC64NVME = base64Digest(c.CRC64NVME) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			service, registry := newFileService(t)
			content := valid
			change(&content)
			if _, _, err := service.Begin(t.Context(), "documents", "report.txt", content); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("Begin error = %v, want ErrInvalidInput", err)
			}
			if _, _, err := service.BeginDirect(t.Context(), "documents", "report.txt", content); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("BeginDirect error = %v, want ErrInvalidInput", err)
			}
			if len(registry.objects) != 0 {
				t.Fatalf("malformed declaration reserved %d objects", len(registry.objects))
			}
		})
	}
}

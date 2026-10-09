package bloby

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"

	"github.com/minio/crc64nvme"
)

// ErrContentMismatch reports stored bytes that differ from their declaration.
var ErrContentMismatch = errors.New("stored content does not match its declaration")

// Content is an uploader's declaration of the bytes it is about to send.
// Storage checks what it receives against the declaration, so publishing an
// object never reads it back. SHA256 and CRC64NVME are lowercase hexadecimal.
type Content struct {
	SizeBytes int64  `json:"size_bytes" minimum:"0"`
	SHA256    string `json:"sha256"     pattern:"^[0-9a-f]{64}$"`
	CRC64NVME string `json:"crc64nvme"  pattern:"^[0-9a-f]{16}$"`
}

// Digest reads r to its end and returns the declaration for its bytes.
func Digest(r io.Reader) (Content, error) {
	sum := sha256.New()
	crc := crc64nvme.New()
	size, err := io.Copy(io.MultiWriter(sum, crc), r)
	if err != nil {
		return Content{}, err
	}
	return Content{
		SizeBytes: size,
		SHA256:    hex.EncodeToString(sum.Sum(nil)),
		CRC64NVME: hex.EncodeToString(crc.Sum(nil)),
	}, nil
}

// CRC64NVME reads r to its end and returns its checksum, as a multipart
// uploader sends with each part.
func CRC64NVME(r io.Reader) (string, error) {
	crc := crc64nvme.New()
	if _, err := io.Copy(crc, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(crc.Sum(nil)), nil
}

var (
	sha256Pattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	crc64NVMEPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

func (c Content) validate() error {
	if c.SizeBytes < 0 || !sha256Pattern.MatchString(c.SHA256) || !crc64NVMEPattern.MatchString(c.CRC64NVME) {
		return fmt.Errorf("%w: content needs a size, SHA-256 and CRC64NVME", ErrInvalidInput)
	}
	return nil
}

func contentMismatch(format string, args ...any) error {
	return fmt.Errorf("%w: %w: %s", ErrInvalidInput, ErrContentMismatch, fmt.Sprintf(format, args...))
}

// base64Digest converts a validated hexadecimal digest to S3's header encoding.
func base64Digest(hexDigest string) string {
	raw, _ := hex.DecodeString(hexDigest)
	return base64.StdEncoding.EncodeToString(raw)
}

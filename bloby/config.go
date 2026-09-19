package bloby

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Kind selects a storage backend.
type Kind string

// Supported backends.
const (
	KindFile Kind = "file"
	KindS3   Kind = "s3"
)

// Config selects and configures a backend.
type Config struct {
	Kind        Kind
	TransferTTL time.Duration
	File        FileConfig
	S3          S3Config
	// ReferencedPrefixes lists the prefixes whose objects only have a purpose once
	// a foreign key references them, such as an installer awaiting its package.
	// Cleanup removes available objects under them that stayed unreferenced past
	// the abandoned-upload age. Objects under other prefixes are kept, because a
	// library users browse holds unreferenced objects on purpose.
	ReferencedPrefixes []string
}

// FileConfig holds the settings for server-hosted storage transfers.
// Root must be a directory dedicated to Bloby.
type FileConfig struct {
	Root             string
	BaseURL          string
	CapabilityKeyHex string
}

// S3Config holds the settings for the S3 backend. Bucket must be dedicated to
// Bloby; cleanup expires all abandoned multipart uploads in it.
type S3Config struct {
	Bucket    string
	Region    string
	Endpoint  string
	AccessKey string
	SecretKey string
	PathStyle bool
}

// New returns the complete blob lifecycle for a registry and configured backend.
func New(ctx context.Context, registry Registry, cfg Config, logger *slog.Logger) (*Service, error) {
	if registry == nil {
		return nil, errors.New("storage registry is required")
	}
	backend, err := newBackend(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	for _, prefix := range cfg.ReferencedPrefixes {
		if !prefixPattern.MatchString(prefix) {
			return nil, fmt.Errorf("%w: invalid referenced prefix %q", ErrInvalidInput, prefix)
		}
	}
	return &Service{registry: registry, backend: backend, logger: logger, transferTTL: cfg.TransferTTL, referencedPrefixes: cfg.ReferencedPrefixes}, nil
}

func newBackend(ctx context.Context, cfg Config) (backend, error) {
	if cfg.TransferTTL <= 0 {
		return nil, errors.New("storage transfer TTL must be positive")
	}
	switch cfg.Kind {
	case KindFile:
		return newFileStore(
			cfg.File.Root,
			cfg.File.BaseURL,
			cfg.File.CapabilityKeyHex,
			cfg.TransferTTL,
		)
	case KindS3:
		return newS3Store(ctx, cfg.S3, cfg.TransferTTL)
	default:
		return nil, fmt.Errorf("unknown storage kind %q", cfg.Kind)
	}
}

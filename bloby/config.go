package bloby

import (
	"context"
	"errors"
	"fmt"
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
}

// FileConfig holds the settings for server-hosted storage transfers.
type FileConfig struct {
	Root             string
	BaseURL          string
	CapabilityKeyHex string
}

// S3Config holds the settings for the S3 backend.
type S3Config struct {
	Bucket    string
	Region    string
	Endpoint  string
	AccessKey string
	SecretKey string
	PathStyle bool
}

// New builds the configured backend.
func New(ctx context.Context, cfg Config) (Backend, error) {
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

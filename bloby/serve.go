package bloby

import (
	"errors"
	"fmt"
	"net/http"
	"path"
	"time"
)

// serveOptions carries HTTP metadata for a caller-authorized key read.
type serveOptions struct {
	ContentType  string
	Filename     string
	CacheControl string
	ETag         string
}

func serveKey(w http.ResponseWriter, r *http.Request, store blobStore, key string, opts serveOptions) error {
	reader, _, err := store.Open(r.Context(), key)
	if errors.Is(err, ErrObjectNotFound) {
		w.WriteHeader(http.StatusNotFound)
		return nil
	}
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return fmt.Errorf("open object: %w", err)
	}
	defer func() { _ = reader.Close() }()

	if opts.ContentType != "" {
		w.Header().Set("Content-Type", opts.ContentType)
	}
	if opts.CacheControl != "" {
		w.Header().Set("Cache-Control", opts.CacheControl)
	}
	if opts.ETag != "" {
		w.Header().Set("ETag", opts.ETag)
	}
	filename := opts.Filename
	if filename == "" {
		filename = path.Base(key)
	}
	http.ServeContent(w, r, filename, time.Time{}, reader)
	return nil
}

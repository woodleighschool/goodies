package bloby

import (
	"context"
	"io"
	"net/http"
	"time"
)

type deliveryMode uint8

const (
	deliveryStream deliveryMode = iota
	deliveryRedirect
)

// Deliverer sends an authorized storage object to an HTTP client.
type Deliverer interface {
	Deliver(w http.ResponseWriter, r *http.Request, object Object, opts DeliveryOptions) error
}

// DeliveryOptions carries response policy owned by the resource exposing an
// object. Object identity and representation metadata come from Object.
type DeliveryOptions struct {
	CacheControl string
}

// Deliver streams file-backed content and redirects S3-backed content to a
// signed provider URL.
func (d *Service) Deliver(
	w http.ResponseWriter,
	r *http.Request,
	object Object,
	opts DeliveryOptions,
) error {
	if !object.Available() {
		w.WriteHeader(http.StatusNotFound)
		return ErrNotFound
	}
	if d.backend.deliveryMode() == deliveryRedirect {
		url, err := d.DownloadURL(r.Context(), object, 0, opts)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return err
		}
		http.Redirect(w, r, url, http.StatusFound)
		return nil
	}
	return serveKey(w, r, d.backend, object.Key(), serveOptions{
		ContentType:  object.ContentType,
		Filename:     object.Filename,
		CacheControl: opts.CacheControl,
		ETag:         object.ETag(),
	})
}

// DownloadURL mints a backend-appropriate direct read URL for object.
func (d *Service) DownloadURL(
	ctx context.Context,
	object Object,
	ttl time.Duration,
	opts DeliveryOptions,
) (string, error) {
	if !object.Available() {
		return "", ErrNotFound
	}
	return d.backend.PresignGet(ctx, object.Key(), ttl, getOptions{
		ContentType:  object.ContentType,
		CacheControl: opts.CacheControl,
	})
}

// Open reads the sealed bytes of an authorized available object.
func (s *Service) Open(ctx context.Context, object Object) (io.ReadSeekCloser, error) {
	if !object.Available() {
		return nil, ErrNotFound
	}
	reader, _, err := s.backend.Open(ctx, object.Key())
	return reader, err
}

// TransferOrigin returns the origin used by direct browser transfers.
func (s *Service) TransferOrigin() string { return s.backend.TransferOrigin() }

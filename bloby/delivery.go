package bloby

import (
	"context"
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

// Delivery applies backend-specific HTTP transfer policy to storage objects.
type Delivery struct {
	backend Backend
}

// NewDelivery returns the HTTP delivery boundary for backend.
func NewDelivery(backend Backend) *Delivery {
	return &Delivery{backend: backend}
}

// Deliver streams file-backed content and redirects S3-backed content to a
// signed provider URL.
func (d *Delivery) Deliver(
	w http.ResponseWriter,
	r *http.Request,
	object Object,
	opts DeliveryOptions,
) error {
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
func (d *Delivery) DownloadURL(
	ctx context.Context,
	object Object,
	ttl time.Duration,
	opts DeliveryOptions,
) (string, error) {
	return d.backend.PresignGet(ctx, object.Key(), ttl, GetOptions{
		ContentType:  object.ContentType,
		CacheControl: opts.CacheControl,
	})
}

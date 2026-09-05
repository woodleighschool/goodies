package bloby

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// TransferHandler returns the server-hosted transfer endpoint for backend.
// S3 transfers use provider-signed URLs, so its handler always returns 404.
func (s *Service) TransferHandler() http.Handler {
	file, ok := s.backend.(*fileStore)
	if !ok {
		return http.NotFoundHandler()
	}
	return transferHandler{
		store:  file,
		logger: s.logger,
	}
}

type transferHandler struct {
	store  *fileStore
	logger *slog.Logger
}

func (h transferHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.get(w, r)
	case http.MethodPut:
		h.put(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h transferHandler) get(w http.ResponseWriter, r *http.Request) {
	claims, ok := h.verify(w, r, capabilityGet)
	if !ok {
		return
	}
	if err := h.store.serveKey(
		w,
		r,
		claims.Key,
		serveOptions{ContentType: claims.ContentType},
	); err != nil {
		h.logError(r, "get-storage-object", err, "key", claims.Key)
	}
}

func (h transferHandler) put(w http.ResponseWriter, r *http.Request) {
	claims, ok := h.verify(w, r, capabilityPut)
	if !ok {
		return
	}
	if err := h.store.Put(
		r.Context(),
		claims.Key,
		r.Body,
		putOptions{},
	); err != nil {
		h.logError(r, "put-storage-object", err, "key", claims.Key)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h transferHandler) logError(r *http.Request, operation string, err error, attrs ...any) {
	args := make([]any, 0, 6+len(attrs))
	args = append(args, "operation", operation, "status", http.StatusInternalServerError)
	args = append(args, attrs...)
	args = append(args, "err", err)
	h.logger.ErrorContext(r.Context(), "storage blob handler failed", args...)
}

func (h transferHandler) verify(
	w http.ResponseWriter,
	r *http.Request,
	op string,
) (capabilityClaims, bool) {
	claims, err := verifyCapability(h.store.capabilityKey, r.URL.Query().Get("cap"), op, time.Now())
	requestKey := strings.TrimPrefix(r.URL.Path, "/storage/")
	switch {
	case errors.Is(err, errExpiredCapability):
		w.WriteHeader(http.StatusGone)
		return capabilityClaims{}, false
	case err != nil || claims.Key == "" || requestKey != claims.Key:
		w.WriteHeader(http.StatusUnauthorized)
		return capabilityClaims{}, false
	}
	return claims, true
}

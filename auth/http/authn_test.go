package authhttp

import (
	"bytes"
	"context"
	"errors"
	"github.com/woodleighschool/goodies/auth/authn"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeAuthenticator struct {
	principal *authn.Principal
	err       error
	got       string
}

func (f *fakeAuthenticator) Authenticate(_ context.Context, authHeader string) (*authn.Principal, error) {
	f.got = authHeader
	if f.err != nil {
		return nil, f.err
	}
	return f.principal, nil
}

func TestRequireHTTPAuthAttachesUser(t *testing.T) {
	authenticator := &fakeAuthenticator{principal: &authn.Principal{ID: 42}}
	handler := RequireAuth(authenticator, slog.New(slog.DiscardHandler))(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		user, ok := authn.PrincipalFromContext(req.Context())
		if !ok {
			t.Fatal("missing user in context")
		}
		if user.ID != 42 {
			t.Fatalf("user = %+v, want user ID 42", user)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if authenticator.got != "Bearer secret" {
		t.Fatalf("auth header = %q, want Bearer secret", authenticator.got)
	}
}

func TestRequireHTTPAuthRejectsMissingCredentials(t *testing.T) {
	authenticator := &fakeAuthenticator{err: authn.ErrNotAuthenticated}
	handler := RequireAuth(authenticator, slog.New(slog.DiscardHandler))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler should not run")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/protected", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestRequireHTTPAuthTreatsLookupFailureAsServerError(t *testing.T) {
	logs, logger := captureAuthLogs()
	handler := RequireAuth(&fakeAuthenticator{err: errors.New("database unavailable")}, logger)(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler ran") }),
	)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/protected", nil))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", recorder.Code)
	}
	if !strings.Contains(logs.String(), "database unavailable") || strings.Contains(recorder.Body.String(), "database unavailable") {
		t.Fatalf("unexpected-error boundary: body=%s logs=%s", recorder.Body.String(), logs.String())
	}
}

func captureAuthLogs() (*bytes.Buffer, *slog.Logger) {
	var logs bytes.Buffer
	return &logs, slog.New(slog.NewJSONHandler(&logs, nil))
}

package authhttp

import (
	"context"
	"errors"
	"github.com/woodleighschool/goodies/auth/authn"
	"github.com/woodleighschool/goodies/auth/authz"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

type permissionCheck struct {
	allowed bool
	err     error
}

func (p permissionCheck) CanAll(context.Context, int64, ...authz.Requirement) (bool, error) {
	return p.allowed, p.err
}
func TestPermissionHTTPBoundary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		principal bool
		check     permissionCheck
		status    int
	}{
		{"anonymous", false, permissionCheck{}, 401}, {"denied", true, permissionCheck{}, 403},
		{"allowed", true, permissionCheck{allowed: true}, 204}, {"failed", true, permissionCheck{err: errors.New("store unavailable")}, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			if tc.principal {
				ctx = authn.WithPrincipal(ctx, &authn.Principal{ID: 42})
			}
			handler := RequirePermission(tc.check, slog.New(slog.DiscardHandler), "users", authz.View)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/users", nil))
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

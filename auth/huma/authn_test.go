package authhuma

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/woodleighschool/goodies/auth/authn"
)

func TestProtectedHumaAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name          string
		authenticator *fakeAuthenticator
		status        int
	}{
		{"authenticated", &fakeAuthenticator{principal: &authn.Principal{ID: 42}}, http.StatusNoContent},
		{"anonymous", &fakeAuthenticator{err: authn.ErrNotAuthenticated}, http.StatusUnauthorized},
		{"unavailable", &fakeAuthenticator{err: errors.New("store unavailable")}, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs, logger := captureAuthLogs()
			router := http.NewServeMux()
			api := humago.New(router, huma.DefaultConfig("test", "test"))
			group := huma.NewGroup(api)
			group.UseMiddleware(RequireAuth(api, tc.authenticator, logger))
			group.UseModifier(ProtectedOperation(api))
			huma.Register(group, huma.Operation{OperationID: "protected", Method: http.MethodGet, Path: "/protected", DefaultStatus: http.StatusNoContent}, func(ctx context.Context, _ *struct{}) (*struct{}, error) {
				principal, err := authn.RequirePrincipal(ctx)
				if err != nil || principal.ID != 42 {
					t.Fatalf("principal=%v error=%v", principal, err)
				}
				return &struct{}{}, nil
			})
			recorder := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/protected", nil)
			req.Header.Set("Authorization", "Bearer synthetic-key")
			router.ServeHTTP(recorder, req)
			if recorder.Code != tc.status {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if tc.status == http.StatusInternalServerError {
				if !strings.Contains(logs.String(), tc.authenticator.err.Error()) || strings.Contains(recorder.Body.String(), tc.authenticator.err.Error()) {
					t.Fatalf("unexpected-error boundary: body=%s logs=%s", recorder.Body.String(), logs.String())
				}
			} else if logs.Len() != 0 {
				t.Fatalf("ordinary authentication logged an error: %s", logs.String())
			}
			if strings.Contains(logs.String(), "synthetic-key") {
				t.Fatal("authentication logged the bearer credential")
			}
			if tc.authenticator.got != "Bearer synthetic-key" {
				t.Fatalf("header=%q", tc.authenticator.got)
			}
			op := api.OpenAPI().Paths["/protected"].Get
			if !reflect.DeepEqual(op.Security, []map[string][]string{{"cookieAuth": {}}, {"bearerAuth": {}}}) {
				t.Fatalf("security=%v", op.Security)
			}
			if response := op.Responses["401"]; response == nil || response.Content["application/problem+json"] == nil {
				t.Fatal("missing unauthorized problem response")
			}
		})
	}
}

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

func TestOptionalHumaAuthAllowsAnonymousAndRejectsBrokenLookup(t *testing.T) {
	type output struct {
		Body struct {
			UserID int64 `json:"user_id"`
		}
	}

	register := func(authenticator *fakeAuthenticator, logger *slog.Logger) http.Handler {
		router := http.NewServeMux()
		humaAPI := humago.New(router, huma.DefaultConfig("test", "test"))
		group := huma.NewGroup(humaAPI)
		group.UseMiddleware(OptionalAuth(humaAPI, authenticator, logger))
		huma.Register(group, huma.Operation{
			OperationID: "optional-auth", Method: http.MethodGet, Path: "/session",
		}, func(ctx context.Context, _ *struct{}) (*output, error) {
			out := &output{}
			if user, ok := authn.PrincipalFromContext(ctx); ok {
				out.Body.UserID = user.ID
			}
			return out, nil
		})
		return router
	}

	for _, tc := range []struct {
		name          string
		authenticator *fakeAuthenticator
		wantStatus    int
		wantUserID    int64
	}{
		{
			name:          "anonymous allowed",
			authenticator: &fakeAuthenticator{err: authn.ErrNotAuthenticated},
			wantStatus:    http.StatusOK,
		},
		{
			name:          "user attached",
			authenticator: &fakeAuthenticator{principal: &authn.Principal{ID: 7}},
			wantStatus:    http.StatusOK,
			wantUserID:    7,
		},
		{
			name:          "broken auth lookup fails",
			authenticator: &fakeAuthenticator{err: errors.New("db down")},
			wantStatus:    http.StatusInternalServerError,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs, logger := captureAuthLogs()
			rec := httptest.NewRecorder()
			register(tc.authenticator, logger).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/session", nil))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %q", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantStatus == http.StatusInternalServerError && (!strings.Contains(logs.String(), "db down") || strings.Contains(rec.Body.String(), "db down")) {
				t.Fatalf("unexpected-error boundary: body=%s logs=%s", rec.Body.String(), logs.String())
			}
			if tc.wantStatus == http.StatusOK {
				var body struct {
					UserID int64 `json:"user_id"`
				}
				if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				if body.UserID != tc.wantUserID {
					t.Fatalf("user_id = %d, want %d", body.UserID, tc.wantUserID)
				}
			}
		})
	}
}

func captureAuthLogs() (*bytes.Buffer, *slog.Logger) {
	var logs bytes.Buffer
	return &logs, slog.New(slog.NewJSONHandler(&logs, nil))
}

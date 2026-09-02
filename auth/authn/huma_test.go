package authn

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

func TestProtectedHumaAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name          string
		authenticator *fakeAuthenticator
		status        int
	}{
		{"authenticated", &fakeAuthenticator{principal: &Principal{ID: 42}}, http.StatusNoContent},
		{"anonymous", &fakeAuthenticator{err: ErrNotAuthenticated}, http.StatusUnauthorized},
		{"unavailable", &fakeAuthenticator{err: errors.New("store unavailable")}, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := http.NewServeMux()
			api := humago.New(router, huma.DefaultConfig("test", "test"))
			group := huma.NewGroup(api)
			group.UseMiddleware(RequireHumaAuth(api, tc.authenticator))
			group.UseModifier(ProtectedOperation(api))
			huma.Register(group, huma.Operation{OperationID: "protected", Method: http.MethodGet, Path: "/protected", DefaultStatus: http.StatusNoContent}, func(ctx context.Context, _ *struct{}) (*struct{}, error) {
				principal, err := RequirePrincipal(ctx)
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
	principal *Principal
	err       error
	got       string
}

func (f *fakeAuthenticator) Authenticate(_ context.Context, authHeader string) (*Principal, error) {
	f.got = authHeader
	if f.err != nil {
		return nil, f.err
	}
	return f.principal, nil
}

func TestRequireHTTPAuthAttachesUser(t *testing.T) {
	authenticator := &fakeAuthenticator{principal: &Principal{ID: 42}}
	handler := RequireHTTPAuth(authenticator)(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		user, ok := PrincipalFromContext(req.Context())
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
	authenticator := &fakeAuthenticator{err: ErrNotAuthenticated}
	handler := RequireHTTPAuth(authenticator)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler should not run")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/protected", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestRequireHTTPAuthTreatsLookupFailureAsServerError(t *testing.T) {
	handler := RequireHTTPAuth(&fakeAuthenticator{err: errors.New("database unavailable")})(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler ran") }),
	)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/protected", nil))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestOptionalHumaAuthAllowsAnonymousAndRejectsBrokenLookup(t *testing.T) {
	type output struct {
		Body struct {
			UserID int64 `json:"user_id"`
		}
	}

	register := func(authenticator *fakeAuthenticator) http.Handler {
		router := http.NewServeMux()
		humaAPI := humago.New(router, huma.DefaultConfig("test", "test"))
		group := huma.NewGroup(humaAPI)
		group.UseMiddleware(OptionalHumaAuth(humaAPI, authenticator))
		huma.Register(group, huma.Operation{
			OperationID: "optional-auth", Method: http.MethodGet, Path: "/session",
		}, func(ctx context.Context, _ *struct{}) (*output, error) {
			out := &output{}
			if user, ok := PrincipalFromContext(ctx); ok {
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
			authenticator: &fakeAuthenticator{err: ErrNotAuthenticated},
			wantStatus:    http.StatusOK,
		},
		{
			name:          "user attached",
			authenticator: &fakeAuthenticator{principal: &Principal{ID: 7}},
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
			rec := httptest.NewRecorder()
			register(tc.authenticator).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/session", nil))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %q", rec.Code, tc.wantStatus, rec.Body.String())
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

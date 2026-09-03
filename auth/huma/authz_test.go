package authhuma

import (
	"bytes"
	"context"
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
	"github.com/woodleighschool/goodies/auth/authz"
	"github.com/woodleighschool/goodies/auth/browser"
)

func TestAuthorizationHTTPBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		anonymous bool
		store     grantStore
		status    int
	}{
		{"anonymous", true, grantStore{}, http.StatusUnauthorized},
		{"denied", false, grantStore{}, http.StatusForbidden},
		{"allowed", false, grantStore{grants: []authz.Grant{{Resource: "users", Access: authz.Edit}}}, http.StatusNoContent},
		{"failed", false, grantStore{err: errors.New("store unavailable")}, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newAuthorization(t, &tc.store)
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			raw := browser.RequirePermission(s, logger, "users", authz.View)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				p, err := authn.RequirePrincipal(r.Context())
				if err != nil || p.ID != 42 {
					t.Fatalf("principal=%v error=%v", p, err)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			router := http.NewServeMux()
			api := humago.New(router, huma.DefaultConfig("test", "test"))
			huma.Register(api, Require(api, s, logger, "users", authz.View, huma.Operation{OperationID: "users", Method: http.MethodGet, Path: "/users", DefaultStatus: http.StatusNoContent}), func(ctx context.Context, _ *struct{}) (*struct{}, error) {
				p, err := authn.RequirePrincipal(ctx)
				if err != nil || p.ID != 42 {
					t.Fatalf("principal=%v error=%v", p, err)
				}
				return &struct{}{}, nil
			})
			for name, handler := range map[string]http.Handler{"raw": raw, "huma": router} {
				t.Run(name, func(t *testing.T) {
					logs.Reset()
					ctx := t.Context()
					if !tc.anonymous {
						ctx = authn.WithPrincipal(ctx, &authn.Principal{ID: 42})
					}
					recorder := httptest.NewRecorder()
					handler.ServeHTTP(recorder, httptest.NewRequestWithContext(ctx, http.MethodGet, "/users", nil))
					if recorder.Code != tc.status {
						t.Fatalf("status=%d want=%d body=%s", recorder.Code, tc.status, recorder.Body.String())
					}
					if tc.status == http.StatusInternalServerError {
						if !strings.Contains(logs.String(), tc.store.err.Error()) || strings.Contains(recorder.Body.String(), tc.store.err.Error()) {
							t.Fatalf("unexpected-error boundary: body=%s logs=%s", recorder.Body.String(), logs.String())
						}
					} else if logs.Len() != 0 {
						t.Fatalf("ordinary authorization logged an error: %s", logs.String())
					}
				})
			}
		})
	}
}

func TestOperationMetadataAndResourcePolicy(t *testing.T) {
	router := http.NewServeMux()
	api := humago.New(router, huma.DefaultConfig("test", "test"))
	s := newAuthorization(t, &grantStore{})
	group := ResourceAPI(api, s, slog.New(slog.DiscardHandler), "users")
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		path := "/" + method
		huma.Register(group, huma.Operation{OperationID: method, Method: method, Path: path}, func(context.Context, *struct{}) (*struct{}, error) { return &struct{}{}, nil })
		op := api.OpenAPI().Paths[path].Get
		switch method {
		case http.MethodHead:
			op = api.OpenAPI().Paths[path].Head
		case http.MethodPost:
			op = api.OpenAPI().Paths[path].Post
		case http.MethodPut:
			op = api.OpenAPI().Paths[path].Put
		case http.MethodPatch:
			op = api.OpenAPI().Paths[path].Patch
		case http.MethodDelete:
			op = api.OpenAPI().Paths[path].Delete
		}
		want := authz.View
		if method != http.MethodGet && method != http.MethodHead {
			want = authz.Edit
		}
		if !reflect.DeepEqual(op.Extensions["x-authz"], map[string]any{"resource": "users", "access": string(want)}) {
			t.Fatalf("%s metadata=%v", method, op.Extensions)
		}
		if response := op.Responses["403"]; response == nil || response.Content["application/problem+json"] == nil {
			t.Fatalf("%s missing problem response", method)
		}
	}
	group = RequireAPI(api, s, slog.New(slog.DiscardHandler), authz.Requirement{Resource: "users", Access: authz.View}, authz.Requirement{Resource: "reports", Access: authz.Edit})
	huma.Register(group, huma.Operation{OperationID: "combined", Method: http.MethodGet, Path: "/combined", Extensions: map[string]any{"x-existing": "preserved"}}, func(context.Context, *struct{}) (*struct{}, error) { return &struct{}{}, nil })
	op := api.OpenAPI().Paths["/combined"].Get
	want := map[string]any{"all": []map[string]any{{"resource": "users", "access": "view"}, {"resource": "reports", "access": "edit"}}}
	if !reflect.DeepEqual(op.Extensions["x-authz"], want) || op.Extensions["x-existing"] != "preserved" {
		t.Fatalf("combined metadata=%v", op.Extensions)
	}
	registry := api.OpenAPI().Components.Schemas
	resourceSchema := registry.Schema(reflect.TypeFor[authz.Resource](), false, "")
	if resourceSchema.Type != "string" || len(resourceSchema.Enum) != 0 {
		t.Fatalf("resource leaked app catalogue: %+v", resourceSchema)
	}
	RegisterSchemas(registry)
	accessSchema := registry.Schema(reflect.TypeFor[authz.Access](), false, "")
	if !reflect.DeepEqual(accessSchema.Enum, []any{"none", "view", "edit"}) {
		t.Fatalf("access enum=%v", accessSchema.Enum)
	}
}

type grantStore struct {
	grants []authz.Grant
	err    error
}

func (s *grantStore) Grants(context.Context, int64) ([]authz.Grant, error) { return s.grants, s.err }
func newAuthorization(t *testing.T, store *grantStore) *authz.Service {
	t.Helper()
	service, err := authz.NewService(store, []authz.Resource{"users", "reports", "settings"})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

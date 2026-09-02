package authz

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/woodleighschool/goodies/auth/authn"
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
		{"allowed", false, grantStore{grants: []Grant{{"users", Edit}}}, http.StatusNoContent},
		{"failed", false, grantStore{err: errors.New("store unavailable")}, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newService(t, &tc.store)
			raw := RequireHTTP(s, "users", View)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				p, err := authn.RequirePrincipal(r.Context())
				if err != nil || p.ID != 42 {
					t.Fatalf("principal=%v error=%v", p, err)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			router := http.NewServeMux()
			api := humago.New(router, huma.DefaultConfig("test", "test"))
			huma.Register(api, Require(api, s, "users", View, huma.Operation{OperationID: "users", Method: http.MethodGet, Path: "/users", DefaultStatus: http.StatusNoContent}), func(ctx context.Context, _ *struct{}) (*struct{}, error) {
				p, err := authn.RequirePrincipal(ctx)
				if err != nil || p.ID != 42 {
					t.Fatalf("principal=%v error=%v", p, err)
				}
				return &struct{}{}, nil
			})
			for name, handler := range map[string]http.Handler{"raw": raw, "huma": router} {
				t.Run(name, func(t *testing.T) {
					ctx := t.Context()
					if !tc.anonymous {
						ctx = authn.WithPrincipal(ctx, &authn.Principal{ID: 42})
					}
					recorder := httptest.NewRecorder()
					handler.ServeHTTP(recorder, httptest.NewRequestWithContext(ctx, http.MethodGet, "/users", nil))
					if recorder.Code != tc.status {
						t.Fatalf("status=%d want=%d body=%s", recorder.Code, tc.status, recorder.Body.String())
					}
				})
			}
		})
	}
}

func TestOperationMetadataAndResourcePolicy(t *testing.T) {
	router := http.NewServeMux()
	api := humago.New(router, huma.DefaultConfig("test", "test"))
	s := newService(t, &grantStore{})
	group := ResourceAPI(api, s, "users")
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
		want := View
		if method != http.MethodGet && method != http.MethodHead {
			want = Edit
		}
		if !reflect.DeepEqual(op.Extensions["x-authz"], map[string]any{"resource": "users", "access": string(want)}) {
			t.Fatalf("%s metadata=%v", method, op.Extensions)
		}
		if response := op.Responses["403"]; response == nil || response.Content["application/problem+json"] == nil {
			t.Fatalf("%s missing problem response", method)
		}
	}
	group = RequireAPI(api, s, Requirement{"users", View}, Requirement{"reports", Edit})
	huma.Register(group, huma.Operation{OperationID: "combined", Method: http.MethodGet, Path: "/combined", Extensions: map[string]any{"x-existing": "preserved"}}, func(context.Context, *struct{}) (*struct{}, error) { return &struct{}{}, nil })
	op := api.OpenAPI().Paths["/combined"].Get
	want := map[string]any{"all": []map[string]any{{"resource": "users", "access": "view"}, {"resource": "reports", "access": "edit"}}}
	if !reflect.DeepEqual(op.Extensions["x-authz"], want) || op.Extensions["x-existing"] != "preserved" {
		t.Fatalf("combined metadata=%v", op.Extensions)
	}
	registry := api.OpenAPI().Components.Schemas
	resourceSchema := registry.Schema(reflect.TypeFor[Resource](), false, "")
	if resourceSchema.Type != "string" || len(resourceSchema.Enum) != 0 {
		t.Fatalf("resource leaked app catalogue: %+v", resourceSchema)
	}
	accessSchema := Access("").Schema(registry)
	if !reflect.DeepEqual(accessSchema.Enum, []any{"none", "view", "edit"}) {
		t.Fatalf("access enum=%v", accessSchema.Enum)
	}
}

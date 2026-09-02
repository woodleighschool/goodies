package authz

import (
	"context"
	"maps"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/woodleighschool/goodies/auth/authn"
	"github.com/woodleighschool/goodies/auth/internal/httpauth"
)

// Authorizer is the consumer contract shared by HTTP boundaries.
type Authorizer interface {
	Can(ctx context.Context, userID int64, resource Resource, required Access) (bool, error)
	CanAll(ctx context.Context, userID int64, requirements ...Requirement) (bool, error)
}

// ResourceAPI applies the conventional view-for-reads, edit-for-writes policy
// for one resource. Callers use [RequireAPI] for exceptions.
func ResourceAPI(api huma.API, service Authorizer, resource Resource) *huma.Group {
	group := huma.NewGroup(api)
	group.UseModifier(func(op *huma.Operation, next func(*huma.Operation)) {
		required := View
		switch op.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			required = Edit
		}
		*op = Require(api, service, resource, required, *op)
		next(op)
	})
	return group
}

// RequireAPI applies fixed requirements to every operation registered on the group.
func RequireAPI(api huma.API, service Authorizer, requirements ...Requirement) *huma.Group {
	group := huma.NewGroup(api)
	group.UseModifier(func(op *huma.Operation, next func(*huma.Operation)) {
		*op = RequireAll(api, service, *op, requirements...)
		next(op)
	})
	return group
}

// Require decorates one protected operation with its explicit authz requirement.
func Require(
	api huma.API,
	service Authorizer,
	resource Resource,
	required Access,
	op huma.Operation,
) huma.Operation {
	return require(api, service, op, Requirement{Resource: resource, Access: required})
}

// RequireAll decorates one protected operation with every required permission.
func RequireAll(
	api huma.API,
	service Authorizer,
	op huma.Operation,
	requirements ...Requirement,
) huma.Operation {
	return require(api, service, op, requirements...)
}

func require(
	api huma.API,
	service Authorizer,
	op huma.Operation,
	requirements ...Requirement,
) huma.Operation {
	op.Extensions = mergeExtensions(op.Extensions, map[string]any{
		"x-authz": requirementExtension(requirements),
	})
	op.Middlewares = append(op.Middlewares, func(ctx huma.Context, next func(huma.Context)) {
		principal, err := authn.RequirePrincipal(ctx.Context())
		if err != nil {
			_ = huma.WriteErr(api, ctx, http.StatusUnauthorized, "not authenticated")
			return
		}
		allowed, err := service.CanAll(ctx.Context(), principal.ID, requirements...)
		if err != nil {
			_ = huma.WriteErr(api, ctx, http.StatusInternalServerError, "authorization failed")
			return
		}
		if !allowed {
			_ = huma.WriteErr(api, ctx, http.StatusForbidden, "forbidden")
			return
		}
		next(ctx)
	})
	httpauth.DeclareErrorResponse(api, &op, http.StatusForbidden)
	return op
}

func requirementExtension(requirements []Requirement) map[string]any {
	values := make([]map[string]any, len(requirements))
	for i, requirement := range requirements {
		values[i] = map[string]any{
			"resource": string(requirement.Resource),
			"access":   string(requirement.Access),
		}
	}
	if len(values) == 1 {
		return values[0]
	}
	return map[string]any{"all": values}
}

// RequireHTTP enforces one authz requirement on a non-Huma HTTP route.
func RequireHTTP(service Authorizer, resource Resource, required Access) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal, err := authn.RequirePrincipal(r.Context())
			if err != nil {
				http.Error(w, "not authenticated", http.StatusUnauthorized)
				return
			}
			allowed, err := service.Can(r.Context(), principal.ID, resource, required)
			if err != nil {
				http.Error(w, "authorization failed", http.StatusInternalServerError)
				return
			}
			if !allowed {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func mergeExtensions(current map[string]any, extra map[string]any) map[string]any {
	if current == nil {
		current = map[string]any{}
	}
	maps.Copy(current, extra)
	return current
}

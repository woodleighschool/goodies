package authhuma

import (
	"log/slog"
	"maps"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/woodleighschool/goodies/auth/authn"
	"github.com/woodleighschool/goodies/auth/authz"
	"github.com/woodleighschool/goodies/auth/internal/httpauth"
)

// ResourceAPI applies the conventional view-for-reads, edit-for-writes policy
// for one resource. Callers use [RequireAPI] for exceptions.
func ResourceAPI(api huma.API, service authz.Authorizer, logger *slog.Logger, resource authz.Resource) *huma.Group {
	group := huma.NewGroup(api)
	group.UseModifier(func(op *huma.Operation, next func(*huma.Operation)) {
		required := authz.View
		switch op.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			required = authz.Edit
		}
		*op = Require(api, service, logger, resource, required, *op)
		next(op)
	})
	return group
}

// RequireAPI applies fixed requirements to every operation registered on the group.
func RequireAPI(api huma.API, service authz.Authorizer, logger *slog.Logger, requirements ...authz.Requirement) *huma.Group {
	group := huma.NewGroup(api)
	group.UseModifier(func(op *huma.Operation, next func(*huma.Operation)) {
		*op = RequireAll(api, service, logger, *op, requirements...)
		next(op)
	})
	return group
}

// Require decorates one protected operation with its explicit authz requirement.
func Require(
	api huma.API,
	service authz.Authorizer, logger *slog.Logger,
	resource authz.Resource,
	required authz.Access,
	op huma.Operation,
) huma.Operation {
	return require(api, service, logger, op, authz.Requirement{Resource: resource, Access: required})
}

// RequireAll decorates one protected operation with every required permission.
func RequireAll(
	api huma.API,
	service authz.Authorizer, logger *slog.Logger,
	op huma.Operation,
	requirements ...authz.Requirement,
) huma.Operation {
	return require(api, service, logger, op, requirements...)
}

func require(
	api huma.API,
	service authz.Authorizer, logger *slog.Logger,
	op huma.Operation,
	requirements ...authz.Requirement,
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
			logger.ErrorContext(ctx.Context(), "authorization failed", "operation", ctx.Operation().OperationID, "err", err)
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

func requirementExtension(requirements []authz.Requirement) map[string]any {
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

func mergeExtensions(current map[string]any, extra map[string]any) map[string]any {
	if current == nil {
		current = map[string]any{}
	}
	maps.Copy(current, extra)
	return current
}

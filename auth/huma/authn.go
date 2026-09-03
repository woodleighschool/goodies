// Package authhuma adapts authentication and authorization to Huma.
package authhuma

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/woodleighschool/goodies/auth/authn"
	"github.com/woodleighschool/goodies/auth/browser"
	"github.com/woodleighschool/goodies/auth/internal/httpauth"
)

// OptionalAuth attaches a user to the Huma context when credentials are
// present and valid. Missing credentials are allowed; other failures reject
// the request.
func OptionalAuth(api huma.API, authenticator browser.Authenticator, logger *slog.Logger) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		principal, err := authenticator.Authenticate(ctx.Context(), ctx.Header("Authorization"))
		if err == nil {
			next(huma.WithContext(ctx, authn.WithPrincipal(ctx.Context(), principal)))
			return
		}
		if errors.Is(err, authn.ErrNotAuthenticated) {
			next(ctx)
			return
		}
		logger.ErrorContext(ctx.Context(), "authentication failed", "operation", ctx.Operation().OperationID, "err", err)
		_ = huma.WriteErr(api, ctx, http.StatusInternalServerError, "request failed")
	}
}

// RequireAuth attaches the authenticated user to protected Huma operations.
func RequireAuth(api huma.API, authenticator browser.Authenticator, logger *slog.Logger) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		principal, err := authenticator.Authenticate(ctx.Context(), ctx.Header("Authorization"))
		if err != nil {
			if errors.Is(err, authn.ErrNotAuthenticated) {
				_ = huma.WriteErr(api, ctx, http.StatusUnauthorized, "not authenticated")
				return
			}
			logger.ErrorContext(ctx.Context(), "authentication failed", "operation", ctx.Operation().OperationID, "err", err)
			_ = huma.WriteErr(api, ctx, http.StatusInternalServerError, "request failed")
			return
		}

		next(huma.WithContext(ctx, authn.WithPrincipal(ctx.Context(), principal)))
	}
}

// ProtectedOperation declares the authentication contract shared by protected
// Huma operations. Runtime authentication is applied separately by
// RequireAuth.
func ProtectedOperation(api huma.API) func(*huma.Operation, func(*huma.Operation)) {
	return func(op *huma.Operation, next func(*huma.Operation)) {
		op.Security = []map[string][]string{
			{"cookieAuth": {}},
			{"bearerAuth": {}},
		}
		httpauth.DeclareErrorResponse(api, op, http.StatusUnauthorized)
		next(op)
	}
}

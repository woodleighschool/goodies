package authn

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/woodleighschool/goodies/auth/internal/httpauth"
)

// Authenticator resolves a browser session or API key into an authenticated principal.
type Authenticator interface {
	Authenticate(ctx context.Context, authHeader string) (*Principal, error)
}

// OptionalHumaAuth attaches a user to the Huma context when credentials are
// present and valid. Missing credentials are allowed; other failures reject
// the request.
func OptionalHumaAuth(api huma.API, authenticator Authenticator) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		principal, err := authenticator.Authenticate(ctx.Context(), ctx.Header("Authorization"))
		if err == nil {
			next(huma.WithContext(ctx, WithPrincipal(ctx.Context(), principal)))
			return
		}
		if errors.Is(err, ErrNotAuthenticated) {
			next(ctx)
			return
		}
		_ = huma.WriteErr(api, ctx, http.StatusInternalServerError, "request failed")
	}
}

// RequireHumaAuth attaches the authenticated user to protected Huma operations.
func RequireHumaAuth(api huma.API, authenticator Authenticator) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		principal, err := authenticator.Authenticate(ctx.Context(), ctx.Header("Authorization"))
		if err != nil {
			if errors.Is(err, ErrNotAuthenticated) {
				_ = huma.WriteErr(api, ctx, http.StatusUnauthorized, "not authenticated")
				return
			}
			_ = huma.WriteErr(api, ctx, http.StatusInternalServerError, "request failed")
			return
		}

		next(huma.WithContext(ctx, WithPrincipal(ctx.Context(), principal)))
	}
}

// ProtectedOperation declares the authentication contract shared by protected
// Huma operations. Runtime authentication is applied separately by
// RequireHumaAuth.
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

// RequireHTTPAuth attaches the authenticated user to raw HTTP routes.
func RequireHTTPAuth(authenticator Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			principal, err := authenticator.Authenticate(req.Context(), req.Header.Get("Authorization"))
			if err != nil {
				status := http.StatusInternalServerError
				if errors.Is(err, ErrNotAuthenticated) {
					status = http.StatusUnauthorized
				}
				http.Error(w, http.StatusText(status), status)
				return
			}
			next.ServeHTTP(w, req.WithContext(WithPrincipal(req.Context(), principal)))
		})
	}
}

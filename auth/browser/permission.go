package browser

import (
	"log/slog"
	"net/http"

	"github.com/woodleighschool/goodies/auth/authn"
	"github.com/woodleighschool/goodies/auth/authz"
)

// RequirePermission enforces one authz requirement on a non-Huma HTTP route.
func RequirePermission(service authz.Authorizer, logger *slog.Logger, resource authz.Resource, required authz.Access) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal, err := authn.RequirePrincipal(r.Context())
			if err != nil {
				http.Error(w, "not authenticated", http.StatusUnauthorized)
				return
			}
			allowed, err := service.Can(r.Context(), principal.ID, resource, required)
			if err != nil {
				logger.ErrorContext(r.Context(), "authorization failed", "operation", "authorize", "resource", resource, "access", required, "err", err)
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

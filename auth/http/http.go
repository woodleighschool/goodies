// Package authhttp adapts authentication and authorization to net/http.
package authhttp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/woodleighschool/goodies/auth/authn"
)

// RequireAuth attaches the authenticated user to raw HTTP routes.
func RequireAuth(authenticator Authenticator, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			principal, err := authenticator.Authenticate(req.Context(), req.Header.Get("Authorization"))
			if err != nil {
				status := http.StatusInternalServerError
				if errors.Is(err, authn.ErrNotAuthenticated) {
					status = http.StatusUnauthorized
				} else {
					logger.ErrorContext(req.Context(), "authentication failed", "operation", "authenticate", "err", err)
				}
				http.Error(w, http.StatusText(status), status)
				return
			}
			next.ServeHTTP(w, req.WithContext(authn.WithPrincipal(req.Context(), principal)))
		})
	}
}

// Authenticator resolves request credentials into an admitted principal.
type Authenticator interface {
	Authenticate(context.Context, string) (*authn.Principal, error)
}

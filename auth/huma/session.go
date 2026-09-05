package authhuma

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/woodleighschool/goodies/auth/authn"
)

const sessionPath = "/api/session"

type sessionOutput struct {
	Body sessionBody
}

type sessionBody struct {
	SSOEnabled bool             `json:"sso_enabled"`
	User       *authn.Principal `json:"user,omitempty"`
}

type sessionUserOutput struct {
	Body authn.Principal
}

type sessionCreateInput struct {
	Body struct {
		Email    string `json:"email" format:"email"`
		Password string `json:"password"`
	}
}

// RegisterSessions mounts GET, POST, and DELETE /api/session. The caller supplies
// APIs with optional authentication, password throttling, and unauthenticated
// logout respectively. Each surface must load sessions and protect unsafe methods
// against cross-origin requests; logout must not depend on current admission.
// A nil authentication service may register schema-only APIs whose handlers are never called.
func RegisterSessions(session, password, logout huma.API, b *authn.Service, logger *slog.Logger, tag string) {
	huma.Register(session, huma.Operation{
		OperationID: "get-session", Method: http.MethodGet, Path: sessionPath,
		Tags: []string{tag}, Summary: "Get session",
	}, func(ctx context.Context, _ *struct{}) (*sessionOutput, error) {
		out := &sessionOutput{Body: sessionBody{SSOEnabled: b.SSOEnabled()}}
		if principal, ok := authn.PrincipalFromContext(ctx); ok {
			out.Body.User = principal
		}
		return out, nil
	})
	huma.Register(password, huma.Operation{
		OperationID: "create-session", Method: http.MethodPost, Path: sessionPath,
		Tags: []string{tag}, Summary: "Create a session",
		Errors: []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusTooManyRequests},
	}, func(ctx context.Context, input *sessionCreateInput) (*sessionUserOutput, error) {
		principal, err := b.Login(ctx, authn.LoginParams{
			Email: input.Body.Email, Password: input.Body.Password,
		})
		if err != nil {
			return nil, sessionError(ctx, logger, "create-session", err)
		}
		return &sessionUserOutput{Body: *principal}, nil
	})
	password.OpenAPI().Paths[sessionPath].Post.Responses["429"].Headers = map[string]*huma.Param{
		"Retry-After": {
			Description: "Seconds before another login attempt", Required: true,
			Schema: &huma.Schema{Type: "integer"},
		},
	}
	huma.Register(logout, huma.Operation{
		OperationID: "delete-session", Method: http.MethodDelete, Path: sessionPath,
		Tags: []string{tag}, Summary: "Delete session", DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, _ *struct{}) (*struct{}, error) {
		if err := b.Logout(ctx); err != nil {
			return nil, sessionError(ctx, logger, "delete-session", err)
		}
		return &struct{}{}, nil
	})
}

func sessionError(ctx context.Context, logger *slog.Logger, operation string, err error) error {
	switch {
	case errors.Is(err, authn.ErrInvalidCredentials):
		return huma.Error401Unauthorized("invalid email or password")
	case errors.Is(err, authn.ErrNotAuthenticated):
		return huma.Error401Unauthorized("not authenticated")
	default:
		logger.ErrorContext(ctx, "authentication failed", "operation", operation, "err", err)
		return huma.Error500InternalServerError("request failed")
	}
}

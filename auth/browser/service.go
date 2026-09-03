// Package browser coordinates browser authentication and application admission.
package browser

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/woodleighschool/goodies/auth/authn"
	"golang.org/x/time/rate"
)

// Config supplies application admission and browser destinations.
type Config struct {
	// Admit checks current application access. A nil predicate denies access.
	Admit func(context.Context, int64) (bool, error)
	// SuccessRedirect defaults to "/"; FailureRedirect defaults to "/login".
	SuccessRedirect string
	FailureRedirect string
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// Service owns browser login, admission, and the process-local password throttle.
// Use it as the Authenticator for browser and API-key protected routes.
type Service struct {
	service         *authn.Service
	admit           func(context.Context, int64) (bool, error)
	successRedirect string
	failureRedirect string
	logger          *slog.Logger
	loginLimiter    *rate.Limiter
}

// New applies one admission rule before login and on authenticated requests.
func New(service *authn.Service, cfg Config) *Service {
	if cfg.SuccessRedirect == "" {
		cfg.SuccessRedirect = "/"
	}
	if cfg.FailureRedirect == "" {
		cfg.FailureRedirect = "/login"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Service{
		service: service, admit: cfg.Admit,
		successRedirect: cfg.SuccessRedirect, failureRedirect: cfg.FailureRedirect,
		logger: cfg.Logger, loginLimiter: rate.NewLimiter(rate.Every(time.Minute/10), 4),
	}
}

// Authenticate resolves credentials and requires current application admission.
func (b *Service) Authenticate(ctx context.Context, authHeader string) (*authn.Principal, error) {
	var principal *authn.Principal
	var err error
	if token, ok := bearerToken(authHeader); ok {
		principal, err = b.service.AuthenticateAPIKey(ctx, token)
	} else {
		principal, err = b.service.CurrentPrincipal(ctx)
	}
	if err != nil {
		return nil, err
	}
	if err := b.admitPrincipal(ctx, principal.ID); err != nil {
		return nil, err
	}
	return principal, nil
}

func (b *Service) startSession(ctx context.Context, principal *authn.Principal) error {
	if err := b.admitPrincipal(ctx, principal.ID); err != nil {
		return err
	}
	return b.service.StartSession(ctx, principal.ID)
}

func (b *Service) admitPrincipal(ctx context.Context, principalID int64) error {
	if b.admit == nil {
		return authn.ErrNotAuthenticated
	}
	allowed, err := b.admit(ctx, principalID)
	if err != nil {
		return fmt.Errorf("check application access: %w", err)
	}
	if !allowed {
		return authn.ErrNotAuthenticated
	}
	return nil
}

// Authenticator resolves a browser session or API key into a principal.
type Authenticator interface {
	Authenticate(context.Context, string) (*authn.Principal, error)
}

// Login verifies a password and admits the principal before creating a session.
func (b *Service) Login(ctx context.Context, params authn.LoginParams) (*authn.Principal, error) {
	principal, err := b.service.AuthenticatePassword(ctx, params)
	if err != nil {
		return nil, err
	}
	if err := b.startSession(ctx, principal); err != nil {
		return nil, err
	}
	return principal, nil
}

// Logout revokes the loaded browser session.
func (b *Service) Logout(ctx context.Context) error { return b.service.Logout(ctx) }

// SSOEnabled reports whether an identity provider is configured.
func (b *Service) SSOEnabled() bool { return b.service.SSOEnabled() }

func bearerToken(authorization string) (string, bool) {
	scheme, value, ok := strings.Cut(authorization, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, " \t\r\n") {
		return "", false
	}
	return value, true
}

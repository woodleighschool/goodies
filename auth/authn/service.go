// Package authn authenticates principals and manages their credentials and sessions.
package authn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/alexedwards/scs/v2"
	"golang.org/x/time/rate"
)

const sessionUserIDKey = "user_id"

// Authentication errors distinguish missing identities from invalid credentials.
var (
	ErrPrincipalNotFound  = errors.New("principal not found")
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrNotAuthenticated   = errors.New("not authenticated")
)

// Principal is the authenticated identity exposed to the application.
type Principal struct {
	ID    int64  `json:"id"`
	Email string `json:"email" format:"email"`
	Name  string `json:"name"`
}

// PasswordIdentity contains the principal and password hash needed for local login.
type PasswordIdentity struct {
	Principal
	PasswordHash string
}

// Store supplies eligible application identities and API-key persistence.
// Missing or ineligible identities return ErrPrincipalNotFound.
type Store interface {
	GetPrincipal(context.Context, int64) (*Principal, error)
	GetPasswordIdentityByEmail(context.Context, string) (*PasswordIdentity, error)
	GetSSOPrincipalByEmail(context.Context, string) (*Principal, error)
	GetPrincipalByAPIKey(context.Context, string) (*Principal, error)
	SetAPIKey(context.Context, int64, string) error
	ClearAPIKey(context.Context, int64) error
}

// Config supplies application admission and browser sign-in configuration.
type Config struct {
	// Admit checks additional application-wide access on every authentication.
	// Nil relies on the store's identity eligibility rules.
	Admit func(context.Context, int64) (bool, error)
	// OIDC enables SSO when non-nil. Discovery completes before New returns.
	OIDC *OIDCConfig
	// SuccessRedirect defaults to "/"; FailureRedirect defaults to "/login".
	SuccessRedirect string
	FailureRedirect string
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// Service authenticates and admits principals before creating or accepting sessions.
// Its configuration is immutable after construction.
type Service struct {
	principals      Store
	sessions        *scs.SessionManager
	dummyHash       string
	oidc            *oidcProvider
	admit           func(context.Context, int64) (bool, error)
	successRedirect string
	failureRedirect string
	logger          *slog.Logger
	loginLimiter    *rate.Limiter
}

// New configures authentication, admission, sessions and optional OIDC discovery.
func New(ctx context.Context, principals Store, sessions *scs.SessionManager, cfg Config) (*Service, error) {
	if principals == nil || sessions == nil {
		return nil, errors.New("authentication requires an identity store and sessions")
	}
	dummyHash, err := HashPassword("authentication-dummy-password")
	if err != nil {
		return nil, fmt.Errorf("hash dummy password: %w", err)
	}
	if cfg.SuccessRedirect == "" {
		cfg.SuccessRedirect = "/"
	}
	if cfg.FailureRedirect == "" {
		cfg.FailureRedirect = "/login"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	service := &Service{
		principals: principals, sessions: sessions, dummyHash: dummyHash,
		admit: cfg.Admit, successRedirect: cfg.SuccessRedirect, failureRedirect: cfg.FailureRedirect,
		logger: cfg.Logger, loginLimiter: rate.NewLimiter(rate.Every(time.Minute/10), 4),
	}
	if cfg.OIDC != nil {
		service.oidc, err = configureOIDC(ctx, *cfg.OIDC)
		if err != nil {
			return nil, err
		}
	}
	return service, nil
}

// Authenticate resolves a browser session or bearer API key and checks current admission.
func (s *Service) Authenticate(ctx context.Context, authHeader string) (*Principal, error) {
	var principal *Principal
	var err error
	if token, ok := bearerToken(authHeader); ok {
		principal, err = s.authenticateAPIKey(ctx, token)
	} else {
		principal, err = s.currentPrincipal(ctx)
	}
	if err != nil {
		return nil, err
	}
	if err := s.admitPrincipal(ctx, principal.ID); err != nil {
		return nil, err
	}
	return principal, nil
}

// Login verifies local credentials and admits the principal before creating a session.
func (s *Service) Login(ctx context.Context, params LoginParams) (*Principal, error) {
	principal, err := s.authenticatePassword(ctx, params)
	if err != nil {
		return nil, err
	}
	if err := s.startSession(ctx, principal.ID); err != nil {
		return nil, err
	}
	return principal, nil
}

func (s *Service) admitPrincipal(ctx context.Context, principalID int64) error {
	if s.admit == nil {
		return nil
	}
	allowed, err := s.admit(ctx, principalID)
	if err != nil {
		return fmt.Errorf("check application access: %w", err)
	}
	if !allowed {
		return ErrNotAuthenticated
	}
	return nil
}

func (s *Service) currentPrincipal(ctx context.Context) (*Principal, error) {
	userID := s.sessions.GetInt64(ctx, sessionUserIDKey)
	if userID == 0 {
		return nil, ErrNotAuthenticated
	}
	principal, err := s.principals.GetPrincipal(ctx, userID)
	if errors.Is(err, ErrPrincipalNotFound) {
		if err := s.sessions.Destroy(ctx); err != nil {
			return nil, fmt.Errorf("destroy invalid session: %w", err)
		}
		return nil, ErrNotAuthenticated
	}
	if err != nil {
		return nil, fmt.Errorf("get session principal: %w", err)
	}
	return principal, nil
}

func (s *Service) startSession(ctx context.Context, principalID int64) error {
	if err := s.admitPrincipal(ctx, principalID); err != nil {
		return err
	}
	if err := s.sessions.RenewToken(ctx); err != nil {
		return fmt.Errorf("renew session: %w", err)
	}
	s.sessions.Put(ctx, sessionUserIDKey, principalID)
	return nil
}

// Logout revokes the active browser session.
func (s *Service) Logout(ctx context.Context) error {
	if err := s.sessions.Destroy(ctx); err != nil {
		return fmt.Errorf("destroy session: %w", err)
	}
	return nil
}

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

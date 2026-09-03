// Package authn authenticates principals and manages their credentials and sessions.
package authn

import (
	"context"
	"errors"
	"fmt"

	"github.com/alexedwards/scs/v2"
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

// PrincipalStore resolves session identities, returning ErrPrincipalNotFound when absent.
type PrincipalStore interface {
	GetPrincipal(context.Context, int64) (*Principal, error)
}

// PasswordStore resolves local credentials, returning ErrPrincipalNotFound when absent.
type PasswordStore interface {
	GetPasswordIdentityByEmail(context.Context, string) (*PasswordIdentity, error)
}

// OIDCStore resolves provisioned SSO identities, returning ErrPrincipalNotFound when absent.
type OIDCStore interface {
	GetSSOPrincipalByEmail(context.Context, string) (*Principal, error)
}

// APIKeyStore resolves and persists API-key credentials.
// Missing credentials return ErrPrincipalNotFound.
type APIKeyStore interface {
	GetPrincipalByAPIKey(context.Context, string) (*Principal, error)
	SetAPIKey(context.Context, int64, string) error
	ClearAPIKey(context.Context, int64) error
}

// Store supplies the identity and credential persistence used by Service.
type Store interface {
	PrincipalStore
	PasswordStore
	OIDCStore
	APIKeyStore
}

// Service authenticates principals and manages sessions and API keys.
// Configure OIDC before serving concurrent requests.
type Service struct {
	principals Store
	sessions   *scs.SessionManager
	dummyHash  string
	oidc       *oidcProvider
}

// NewService wires authentication to principal persistence and sessions.
func NewService(principals Store, sessions *scs.SessionManager) (*Service, error) {
	dummyHash, err := HashPassword("authentication-dummy-password")
	if err != nil {
		return nil, fmt.Errorf("hash dummy password: %w", err)
	}
	return &Service{principals: principals, sessions: sessions, dummyHash: dummyHash}, nil
}

// ConfigureOIDC performs OIDC issuer discovery and enables the SSO flow.
// A discovery failure leaves the existing configuration unchanged.
func (s *Service) ConfigureOIDC(ctx context.Context, cfg OIDCConfig) error {
	provider, err := configureOIDC(ctx, cfg)
	if err != nil {
		return err
	}
	s.oidc = provider
	return nil
}

// CurrentPrincipal resolves the principal stored in the session loaded into ctx.
func (s *Service) CurrentPrincipal(ctx context.Context) (*Principal, error) {
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

// StartSession renews the loaded browser session token and authenticates principalID.
func (s *Service) StartSession(ctx context.Context, principalID int64) error {
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

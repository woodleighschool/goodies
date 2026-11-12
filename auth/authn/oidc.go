package authn

import (
	"context"
	"errors"
	"fmt"
)

// SSO errors describe expected callback failures.
var (
	ErrSSOUnknownUser     = errors.New("no account for this identity")
	ErrSSOEmailClaimEmpty = errors.New("identity provider returned no email claim")
)

// SSOEnabled reports whether the service has a configured OIDC provider.
func (s *Service) SSOEnabled() bool {
	return s.oidc != nil
}

// completeSSO verifies the identity before resolving its provisioned account.
func (s *Service) completeSSO(ctx context.Context, state, code string) (*Principal, error) {
	identity, err := s.oidc.Complete(ctx, state, code)
	if err != nil {
		return nil, err
	}
	if identity.Email == "" {
		return nil, ErrSSOEmailClaimEmpty
	}
	return s.completeSSOLogin(ctx, identity.Email)
}

func (s *Service) completeSSOLogin(ctx context.Context, email string) (*Principal, error) {
	principal, err := s.principals.GetSSOPrincipalByEmail(ctx, email)
	if errors.Is(err, ErrPrincipalNotFound) {
		return nil, ErrSSOUnknownUser
	}
	if err != nil {
		return nil, fmt.Errorf("lookup sso user: %w", err)
	}
	return principal, nil
}

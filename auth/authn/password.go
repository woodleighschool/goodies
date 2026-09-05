package authn

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// LoginParams contains a local-login attempt.
type LoginParams struct {
	Email    string
	Password string
}

// authenticatePassword verifies local credentials without creating a session.
func (s *Service) authenticatePassword(ctx context.Context, params LoginParams) (*Principal, error) {
	identity, passwordHash, err := s.passwordLoginCandidate(ctx, strings.TrimSpace(params.Email))
	if err != nil {
		return nil, err
	}
	valid, err := VerifyPassword(params.Password, passwordHash)
	if err != nil {
		return nil, fmt.Errorf("verify password: %w", err)
	}
	if identity == nil || !valid {
		return nil, ErrInvalidCredentials
	}
	return &identity.Principal, nil
}

func (s *Service) passwordLoginCandidate(ctx context.Context, email string) (*PasswordIdentity, string, error) {
	identity, err := s.principals.GetPasswordIdentityByEmail(ctx, email)
	if errors.Is(err, ErrPrincipalNotFound) {
		return nil, s.dummyHash, nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("get password identity: %w", err)
	}
	return identity, identity.PasswordHash, nil
}

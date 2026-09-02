package authn

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const minimumCredentialFailureDuration = time.Second

// LoginParams contains a local-login attempt.
type LoginParams struct {
	Email    string
	Password string
}

// AuthenticatePassword verifies local credentials without creating a session.
func (s *Service) AuthenticatePassword(ctx context.Context, params LoginParams) (*Principal, error) {
	started := time.Now()
	identity, passwordHash, err := s.passwordLoginCandidate(ctx, strings.TrimSpace(params.Email))
	if err != nil {
		return nil, err
	}
	valid, err := VerifyPassword(params.Password, passwordHash)
	if err != nil {
		return nil, fmt.Errorf("verify password: %w", err)
	}
	if identity == nil || !valid {
		// Padding only credential failures avoids leaking whether the identity exists.
		time.Sleep(time.Until(started.Add(minimumCredentialFailureDuration)))
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

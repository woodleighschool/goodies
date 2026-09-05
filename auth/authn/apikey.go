package authn

import (
	"context"
	"errors"
	"fmt"
)

// apiKeyByteLen provides 192 bits of entropy and encodes to 32 base64url characters.
const apiKeyByteLen = 24

// RotateAPIKey generates and persists a replacement API-key credential.
func (s *Service) RotateAPIKey(ctx context.Context, principalID int64) error {
	key, err := randomToken(apiKeyByteLen)
	if err != nil {
		return err
	}
	if err := s.principals.SetAPIKey(ctx, principalID, key); err != nil {
		return fmt.Errorf("set API key: %w", err)
	}
	return nil
}

// RevokeAPIKey removes a principal's API-key credential.
func (s *Service) RevokeAPIKey(ctx context.Context, principalID int64) error {
	if err := s.principals.ClearAPIKey(ctx, principalID); err != nil {
		return fmt.Errorf("clear API key: %w", err)
	}
	return nil
}

// authenticateAPIKey resolves one API key without consulting a browser session.
func (s *Service) authenticateAPIKey(ctx context.Context, token string) (*Principal, error) {
	if token == "" {
		return nil, ErrNotAuthenticated
	}
	principal, err := s.principals.GetPrincipalByAPIKey(ctx, token)
	if errors.Is(err, ErrPrincipalNotFound) {
		return nil, ErrNotAuthenticated
	}
	if err != nil {
		return nil, fmt.Errorf("get principal by API key: %w", err)
	}
	return principal, nil
}

package authn

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	ssoStateSessionKey = "sso_state"
	ssoNonceSessionKey = "sso_nonce"
	oidcTokenByteLen   = 24
)

// SSO errors describe expected callback failures.
var (
	ErrSSOStateMismatch   = errors.New("sso state mismatch")
	ErrSSONonceMismatch   = errors.New("sso nonce mismatch")
	ErrSSONotConfigured   = errors.New("sso is not configured")
	ErrSSOUnknownUser     = errors.New("no account for this identity")
	ErrSSOEmailClaimEmpty = errors.New("identity provider returned no email claim")
)

// OIDCConfig contains everything needed to talk to an OIDC issuer.
type OIDCConfig struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       []string
	EmailClaim   string
}

// oidcProvider holds the configured oauth2 client and ID-token verifier.
type oidcProvider struct {
	oauth2     *oauth2.Config
	verifier   *oidc.IDTokenVerifier
	emailClaim string
}

// configureOIDC discovers the OIDC issuer and returns a provider.
func configureOIDC(ctx context.Context, cfg OIDCConfig) (*oidcProvider, error) {
	if cfg.IssuerURL == "" {
		return nil, ErrSSONotConfigured
	}
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("discover oidc issuer: %w", err)
	}
	scopes := append([]string(nil), cfg.Scopes...)
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "email", "profile"}
	}
	emailClaim := cfg.EmailClaim
	if emailClaim == "" {
		emailClaim = "email"
	}
	return &oidcProvider{
		oauth2: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  cfg.RedirectURL,
			Scopes:       scopes,
		},
		verifier:   provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		emailClaim: emailClaim,
	}, nil
}

// SSOEnabled reports whether the service has a configured OIDC provider.
func (s *Service) SSOEnabled() bool {
	return s.oidc != nil
}

// beginSSO generates a state and nonce, stores them on the session, and
// returns the provider authorization URL the caller should redirect to.
func (s *Service) beginSSO(ctx context.Context) (string, error) {
	if s.oidc == nil {
		return "", ErrSSONotConfigured
	}
	state, err := randomToken(oidcTokenByteLen)
	if err != nil {
		return "", err
	}
	nonce, err := randomToken(oidcTokenByteLen)
	if err != nil {
		return "", err
	}
	s.sessions.Put(ctx, ssoStateSessionKey, state)
	s.sessions.Put(ctx, ssoNonceSessionKey, nonce)
	return s.oidc.oauth2.AuthCodeURL(state, oidc.Nonce(nonce)), nil
}

// completeSSO verifies a single-use callback and resolves its provisioned principal.
// It does not create a browser session or apply application admission policy.
func (s *Service) completeSSO(ctx context.Context, state, code string) (*Principal, error) {
	if s.oidc == nil {
		return nil, ErrSSONotConfigured
	}
	expectedState := s.sessions.PopString(ctx, ssoStateSessionKey)
	expectedNonce := s.sessions.PopString(ctx, ssoNonceSessionKey)
	if expectedState == "" || state == "" || state != expectedState {
		return nil, ErrSSOStateMismatch
	}
	if expectedNonce == "" {
		return nil, ErrSSONonceMismatch
	}

	token, err := s.oidc.oauth2.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("oidc token exchange: %w", err)
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, errors.New("oidc token response missing id_token")
	}
	idToken, err := s.oidc.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("verify id token: %w", err)
	}
	if idToken.Nonce != expectedNonce {
		return nil, ErrSSONonceMismatch
	}

	claims := map[string]any{}
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("decode id token claims: %w", err)
	}
	email, _ := claims[s.oidc.emailClaim].(string)
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, ErrSSOEmailClaimEmpty
	}
	return s.completeSSOLogin(ctx, email)
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

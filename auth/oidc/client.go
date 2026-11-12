// Package oidc verifies browser sign-ins without requiring application accounts.
package oidc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/alexedwards/scs/v2"
	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	stateKey    = "sso_state"
	nonceKey    = "sso_nonce"
	verifierKey = "sso_verifier"
)

// Callback errors distinguish configuration and browser transaction failures.
var (
	ErrNotConfigured   = errors.New("sso is not configured")
	ErrStateMismatch   = errors.New("sso state mismatch")
	ErrNonceMismatch   = errors.New("sso nonce mismatch")
	ErrVerifierMissing = errors.New("sso verifier missing")
)

// Config supplies the issuer, OAuth client and requested identity claims.
type Config struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       []string
	EmailClaim   string
}

// Identity contains verified identity claims. Name and email are display values;
// issuer and subject identify the account independently of those editable values.
type Identity struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
	Name    string `json:"name"`
	Email   string `json:"email"`
}

// Client binds authorization requests to an SCS browser session.
type Client struct {
	sessions   *scs.SessionManager
	oauth2     *oauth2.Config
	verifier   *oidc.IDTokenVerifier
	emailClaim string
}

// New discovers the issuer and configures ID-token verification before returning.
func New(ctx context.Context, sessions *scs.SessionManager, cfg Config) (*Client, error) {
	if cfg.IssuerURL == "" {
		return nil, ErrNotConfigured
	}
	if sessions == nil {
		return nil, errors.New("oidc requires browser sessions")
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
	return &Client{
		sessions: sessions,
		oauth2: &oauth2.Config{
			ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret,
			Endpoint: provider.Endpoint(), RedirectURL: cfg.RedirectURL, Scopes: scopes,
		},
		verifier:   provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		emailClaim: emailClaim,
	}, nil
}

// Begin records state, nonce and PKCE verifier and returns the authorization URL.
func (c *Client) Begin(ctx context.Context) (string, error) {
	if c == nil {
		return "", ErrNotConfigured
	}
	state, nonce, verifier := oauth2.GenerateVerifier(), oauth2.GenerateVerifier(), oauth2.GenerateVerifier()
	c.sessions.Put(ctx, stateKey, state)
	c.sessions.Put(ctx, nonceKey, nonce)
	c.sessions.Put(ctx, verifierKey, verifier)
	return c.oauth2.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), nil
}

// Complete consumes a single-use callback and verifies its ID token. Missing
// display claims are allowed; the caller owns admission and session creation.
func (c *Client) Complete(ctx context.Context, state, code string) (*Identity, error) {
	if c == nil {
		return nil, ErrNotConfigured
	}
	expectedState := c.sessions.PopString(ctx, stateKey)
	expectedNonce := c.sessions.PopString(ctx, nonceKey)
	verifier := c.sessions.PopString(ctx, verifierKey)
	if expectedState == "" || state == "" || state != expectedState {
		return nil, ErrStateMismatch
	}
	if expectedNonce == "" {
		return nil, ErrNonceMismatch
	}
	if verifier == "" {
		return nil, ErrVerifierMissing
	}
	token, err := c.oauth2.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, fmt.Errorf("oidc token exchange: %w", err)
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, errors.New("oidc token response missing id_token")
	}
	idToken, err := c.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("verify id token: %w", err)
	}
	if idToken.Nonce != expectedNonce {
		return nil, ErrNonceMismatch
	}
	if idToken.Subject == "" {
		return nil, errors.New("identity provider returned no subject")
	}
	claims := map[string]any{}
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("decode id token claims: %w", err)
	}
	name, _ := claims["name"].(string)
	email, _ := claims[c.emailClaim].(string)
	return &Identity{Issuer: idToken.Issuer, Subject: idToken.Subject, Name: strings.TrimSpace(name), Email: strings.TrimSpace(email)}, nil
}

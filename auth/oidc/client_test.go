package oidc

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/url"
	"testing"

	"github.com/alexedwards/scs/v2"
	"github.com/woodleighschool/goodies/auth/internal/authtest"
	"golang.org/x/oauth2"
)

func TestVerifiedIdentity(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		alter     func(map[string]any)
		wantEmail string
		wantError bool
	}{
		{name: "identity", wantEmail: "person@example.invalid"},
		{name: "missing display claims", alter: func(c map[string]any) { delete(c, "email"); delete(c, "name") }},
		{name: "missing subject", alter: func(c map[string]any) { delete(c, "sub") }, wantError: true},
		{name: "wrong nonce", alter: func(c map[string]any) { c["nonce"] = "wrong" }, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := authtest.OIDCProvider(t, key, "email", tc.alter)
			sessions := scs.New()
			ctx, err := sessions.Load(t.Context(), "")
			if err != nil {
				t.Fatal(err)
			}
			client, err := New(ctx, sessions, Config{IssuerURL: provider.URL, ClientID: "client-id", ClientSecret: "synthetic-secret", RedirectURL: "https://app.example.invalid/callback"})
			if err != nil {
				t.Fatal(err)
			}
			authorize, err := client.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := url.Parse(authorize)
			if err != nil {
				t.Fatal(err)
			}
			q := parsed.Query()
			if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") != oauth2.S256ChallengeFromVerifier(sessions.GetString(ctx, verifierKey)) {
				t.Fatal("authorization request is missing PKCE")
			}
			identity, err := client.Complete(ctx, q.Get("state"), q.Get("nonce"))
			if (err != nil) != tc.wantError {
				t.Fatalf("identity=%+v error=%v", identity, err)
			}
			if !tc.wantError && (identity.Issuer != provider.URL || identity.Subject != "synthetic-user" || identity.Email != tc.wantEmail) {
				t.Fatalf("identity=%+v", identity)
			}
			if _, err := client.Complete(ctx, q.Get("state"), q.Get("nonce")); !errors.Is(err, ErrStateMismatch) {
				t.Fatalf("replayed callback: %v", err)
			}
		})
	}
}

func TestCallbackConsumesBrowserTransaction(t *testing.T) {
	for _, tc := range []struct {
		name, state, nonce, verifier string
		want                         error
	}{
		{"wrong state", "wrong", "nonce", "verifier", ErrStateMismatch},
		{"missing nonce", "expected", "", "verifier", ErrNonceMismatch},
		{"missing verifier", "expected", "nonce", "", ErrVerifierMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions := scs.New()
			ctx, err := sessions.Load(t.Context(), "")
			if err != nil {
				t.Fatal(err)
			}
			sessions.Put(ctx, stateKey, "expected")
			sessions.Put(ctx, nonceKey, tc.nonce)
			sessions.Put(ctx, verifierKey, tc.verifier)
			client := &Client{sessions: sessions}
			if _, err := client.Complete(ctx, tc.state, "code"); !errors.Is(err, tc.want) {
				t.Fatalf("callback error=%v", err)
			}
			for _, key := range []string{stateKey, nonceKey, verifierKey} {
				if sessions.Exists(ctx, key) {
					t.Fatalf("callback retained %s", key)
				}
			}
		})
	}
}

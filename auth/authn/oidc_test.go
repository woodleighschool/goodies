package authn

import (
	"errors"
	"net/url"
	"testing"

	"golang.org/x/oauth2"
)

func TestBeginSSOBindsStateAndNonceToSession(t *testing.T) {
	sessions, ctx := loadedSession(t)
	service := &Service{
		sessions: sessions,
		oidc: &oidcProvider{oauth2: &oauth2.Config{
			ClientID: "client-id",
			Endpoint: oauth2.Endpoint{AuthURL: "https://identity.example.invalid/authorize"},
		}},
	}

	authURL, err := service.BeginSSO(ctx)
	if err != nil {
		t.Fatalf("BeginSSO() error = %v", err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse authorization URL: %v", err)
	}
	state := sessions.GetString(ctx, ssoStateSessionKey)
	nonce := sessions.GetString(ctx, ssoNonceSessionKey)
	if state == "" || parsed.Query().Get("state") != state {
		t.Fatalf("authorization state = %q, session state = %q", parsed.Query().Get("state"), state)
	}
	if nonce == "" || parsed.Query().Get("nonce") != nonce {
		t.Fatalf("authorization nonce = %q, session nonce = %q", parsed.Query().Get("nonce"), nonce)
	}
}

func TestCompleteSSORejectsMissingSessionNonce(t *testing.T) {
	sessions, ctx := loadedSession(t)
	sessions.Put(ctx, ssoStateSessionKey, "expected-state")

	service := &Service{
		sessions: sessions,
		oidc:     &oidcProvider{},
	}
	if _, err := service.CompleteSSO(ctx, "expected-state", "code"); !errors.Is(err, ErrSSONonceMismatch) {
		t.Fatalf("CompleteSSO error = %v, want %v", err, ErrSSONonceMismatch)
	}
}

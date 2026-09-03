package authn

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/woodleighschool/goodies/auth/internal/authtest"
)

func TestOIDCDiscoveryAndCallback(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	broken := errors.New("store unavailable")
	for _, tc := range []struct {
		name              string
		claim             string
		alter             func(map[string]any)
		storeErr, errorIs error
		wantError         bool
	}{
		{name: "valid"},
		{name: "custom claim", claim: "preferred_username"},
		{name: "nonce mismatch", alter: func(c map[string]any) { c["nonce"] = "wrong" }, errorIs: ErrSSONonceMismatch, wantError: true},
		{name: "missing email", alter: func(c map[string]any) { delete(c, "email") }, errorIs: ErrSSOEmailClaimEmpty, wantError: true},
		{name: "wrong audience", alter: func(c map[string]any) { c["aud"] = "another-client" }, wantError: true},
		{name: "wrong issuer", alter: func(c map[string]any) { c["iss"] = "https://other.example.invalid" }, wantError: true},
		{name: "expired", alter: func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }, wantError: true},
		{name: "unprovisioned", storeErr: ErrPrincipalNotFound, errorIs: ErrSSOUnknownUser, wantError: true},
		{name: "lookup failure", storeErr: broken, errorIs: broken, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claim := tc.claim
			if claim == "" {
				claim = "email"
			}
			server := authtest.OIDCProvider(t, key, claim, tc.alter)
			issuer := server.URL
			sessions, ctx := loadedSession(t)
			store := &principalStore{principal: &Principal{ID: 42}, err: tc.storeErr}
			service := &Service{principals: store, sessions: sessions}
			if err := service.ConfigureOIDC(ctx, OIDCConfig{IssuerURL: issuer, ClientID: "client-id", ClientSecret: "synthetic-secret", RedirectURL: "https://app.example.invalid/callback", EmailClaim: tc.claim}); err != nil {
				t.Fatal(err)
			}
			if !service.SSOEnabled() {
				t.Fatal("SSO not enabled after discovery")
			}
			authURL, err := service.BeginSSO(ctx)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := url.Parse(authURL)
			if err != nil {
				t.Fatal(err)
			}
			query := parsed.Query()
			if query.Get("client_id") != "client-id" || query.Get("scope") != "openid email profile" || query.Get("response_type") != "code" {
				t.Fatalf("authorization query=%v", query)
			}
			principal, err := service.CompleteSSO(ctx, query.Get("state"), query.Get("nonce"))
			if (err != nil) != tc.wantError {
				t.Fatalf("principal=%v error=%v", principal, err)
			}
			if tc.errorIs != nil && !errors.Is(err, tc.errorIs) {
				t.Fatalf("error=%v want=%v", err, tc.errorIs)
			}
			if !tc.wantError && (principal.ID != 42 || store.email != "person@example.invalid") {
				t.Fatalf("principal=%v email=%q", principal, store.email)
			}
			if sessions.GetInt64(ctx, sessionUserIDKey) != 0 {
				t.Fatal("SSO started session before admission")
			}
			if _, err := service.CompleteSSO(ctx, query.Get("state"), query.Get("nonce")); !errors.Is(err, ErrSSOStateMismatch) {
				t.Fatalf("replayed callback: %v", err)
			}
		})
	}
}

func TestSSOConfigurationAndStateErrors(t *testing.T) {
	sessions, ctx := loadedSession(t)
	service := &Service{sessions: sessions}
	if service.SSOEnabled() {
		t.Fatal("unconfigured SSO enabled")
	}
	if err := service.ConfigureOIDC(ctx, OIDCConfig{}); !errors.Is(err, ErrSSONotConfigured) {
		t.Fatalf("empty configuration: %v", err)
	}
	if _, err := service.BeginSSO(ctx); !errors.Is(err, ErrSSONotConfigured) {
		t.Fatalf("unconfigured BeginSSO: %v", err)
	}
	if _, err := service.CompleteSSO(ctx, "state", "code"); !errors.Is(err, ErrSSONotConfigured) {
		t.Fatalf("unconfigured CompleteSSO: %v", err)
	}
	service.oidc = &oidcProvider{}
	sessions.Put(ctx, ssoStateSessionKey, "expected-state")
	sessions.Put(ctx, ssoNonceSessionKey, "expected-nonce")
	if _, err := service.CompleteSSO(ctx, "wrong-state", "code"); !errors.Is(err, ErrSSOStateMismatch) {
		t.Fatalf("wrong state: %v", err)
	}
	if sessions.GetString(ctx, ssoStateSessionKey) != "" || sessions.GetString(ctx, ssoNonceSessionKey) != "" {
		t.Fatal("failed callback did not consume state and nonce")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	if err := service.ConfigureOIDC(ctx, OIDCConfig{IssuerURL: server.URL}); err == nil || !strings.Contains(err.Error(), "discover oidc issuer") {
		t.Fatalf("discovery failure: %v", err)
	}
}

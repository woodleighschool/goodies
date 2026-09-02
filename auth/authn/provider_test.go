package authn

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
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
			var issuer string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/.well-known/openid-configuration":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token",
						"jwks_uri": issuer + "/keys", "response_types_supported": []string{"code"},
						"subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"},
					})
				case "/keys":
					_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{"kty": "RSA", "kid": "test-key", "alg": "RS256", "use": "sig",
						"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
					}}})
				case "/token":
					if err := r.ParseForm(); err != nil {
						t.Error(err)
						http.Error(w, "invalid form", 400)
						return
					}
					if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("redirect_uri") != "https://app.example.invalid/callback" {
						t.Errorf("token exchange form=%v", r.Form)
					}
					client, secret, ok := r.BasicAuth()
					if !ok || client != "client-id" || secret != "synthetic-secret" {
						t.Error("missing token client authentication")
					}
					claims := map[string]any{"iss": issuer, "sub": "synthetic-user", "aud": "client-id", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": r.Form.Get("code"), claim: " person@example.invalid "}
					if tc.alter != nil {
						tc.alter(claims)
					}
					payload, err := json.Marshal(claims)
					if err != nil {
						t.Error(err)
						return
					}
					header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test-key"}`))
					unsigned := header + "." + base64.RawURLEncoding.EncodeToString(payload)
					digest := sha256.Sum256([]byte(unsigned))
					signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
					if err != nil {
						t.Error(err)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "synthetic-access-token", "token_type": "Bearer", "id_token": unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)})
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			issuer = server.URL
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

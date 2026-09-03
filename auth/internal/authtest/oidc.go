// Package authtest supplies a local OIDC provider for authentication contract tests.
package authtest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// OIDCProvider returns a local issuer that signs ID tokens using the nonce as its authorization code.
func OIDCProvider(t *testing.T, key *rsa.PrivateKey, claim string, alter func(map[string]any)) *httptest.Server {
	t.Helper()
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
			if alter != nil {
				alter(claims)
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
	return server
}

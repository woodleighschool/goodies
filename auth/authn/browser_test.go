package authn

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"github.com/alexedwards/scs/v2"
	"github.com/woodleighschool/goodies/auth/internal/authtest"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestBrowserSSOAdmissionAndRedirects(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	provider := authtest.OIDCProvider(t, key, "email", nil)
	for _, tc := range []struct {
		name, destination string
		admitted          bool
		admissionErr      error
	}{
		{name: "allowed", admitted: true, destination: "/app/home"},
		{name: "denied", destination: "/sign-in?from=app&sso_error=no+account+for+this+identity"},
		{name: "failed", admissionErr: errors.New("synthetic admission failure"), destination: "/sign-in?from=app&sso_error=sso+sign-in+failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions, _ := loadedSession(t)
			var logs bytes.Buffer
			admitted, admissionErr := tc.admitted, tc.admissionErr
			browser, err := New(t.Context(), &principalStore{principal: &Principal{ID: 42}}, sessions, Config{
				OIDC:            &OIDCConfig{IssuerURL: provider.URL, ClientID: "client-id", ClientSecret: "synthetic-secret", RedirectURL: "https://app.example.invalid/callback"},
				Admit:           func(context.Context, int64) (bool, error) { return admitted, admissionErr },
				SuccessRedirect: "/app/home", FailureRedirect: "/sign-in?from=app",
				Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
			})
			if err != nil {
				t.Fatal(err)
			}
			handler := ssoTestHandler(browser, sessions)
			start := ssoRequest(t, handler, http.MethodGet, "/api/auth/sso/start", "", nil, "")
			authorize, err := url.Parse(start.Header().Get("Location"))
			if err != nil || start.Code != http.StatusFound {
				t.Fatalf("SSO start = %d %s error=%v", start.Code, start.Body.String(), err)
			}
			cookie := start.Result().Cookies()[0]
			query := authorize.Query()
			path := "/api/auth/sso/callback?" + url.Values{"state": {query.Get("state")}, "code": {query.Get("nonce")}}.Encode()
			callback := ssoRequest(t, handler, http.MethodGet, path, "", cookie, "")
			if callback.Code != http.StatusFound || callback.Header().Get("Location") != tc.destination {
				t.Fatalf("callback = %d %q", callback.Code, callback.Header().Get("Location"))
			}
			if tc.admissionErr != nil && !strings.Contains(logs.String(), tc.admissionErr.Error()) {
				t.Fatalf("missing operational error: %s", logs.String())
			}
			if cookies := callback.Result().Cookies(); len(cookies) > 0 {
				cookie = cookies[0]
			}
			// Check session creation separately from current admission: denying the
			// next request would otherwise conceal an incorrectly created session.
			admitted, admissionErr = true, nil
			session := ssoRequest(t, handler, http.MethodGet, "/session", "", cookie, "")
			var body ssoSessionBody
			if err := json.Unmarshal(session.Body.Bytes(), &body); err != nil || (body.User != nil) != tc.admitted {
				t.Fatalf("SSO session = %s error=%v", session.Body.String(), err)
			}
		})
	}
}

func TestBrowserSSOCallbackErrors(t *testing.T) {
	browser := &Service{failureRedirect: "/login"}
	for path, message := range map[string]string{
		"?error=access_denied": "access_denied",
		"?state=state-only":    "missing state or code",
	} {
		rec := httptest.NewRecorder()
		browser.SSOCallback(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/callback"+path, nil))
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login?sso_error="+url.QueryEscape(message) {
			t.Fatalf("callback = %d %q", rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestAuthenticateBearerPrecedence(t *testing.T) {
	sessions, ctx := loadedSession(t)

	store := &principalStore{principal: &Principal{ID: 42}, key: "valid-key"}
	service, err := New(t.Context(), store, sessions, Config{Admit: allowPrincipal})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.startSession(ctx, 42); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		header  string
		wantErr bool
	}{
		{"", false}, {"Basic ignored", false}, {"Bearer", false}, {"Bearer ", false},
		{"Bearer two tokens", false}, {"Bearer\tvalid-key", false},
		{"bEaReR   valid-key  ", false}, {"Bearer invalid-key", true},
	} {
		t.Run(tc.header, func(t *testing.T) {
			principal, err := service.Authenticate(ctx, tc.header)
			if tc.wantErr {
				if !errors.Is(err, ErrNotAuthenticated) || principal != nil {
					t.Fatalf("principal=%v error=%v", principal, err)
				}
				return
			}
			if err != nil || principal.ID != 42 {
				t.Fatalf("principal=%v error=%v", principal, err)
			}
		})
	}
}

type ssoSessionBody struct {
	User *Principal `json:"user,omitempty"`
}

func ssoTestHandler(service *Service, sessions *scs.SessionManager) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/auth/sso/start", service.SSOStart)
	mux.HandleFunc("GET /api/auth/sso/callback", service.SSOCallback)
	mux.HandleFunc("GET /session", func(w http.ResponseWriter, r *http.Request) {
		principal, _ := service.Authenticate(r.Context(), "")
		_ = json.NewEncoder(w).Encode(ssoSessionBody{User: principal})
	})
	sessions.Cookie = scs.SessionCookie{Name: "session", Path: "/", HttpOnly: true}
	return sessions.LoadAndSave(mux)
}
func ssoRequest(t *testing.T, handler http.Handler, method, path, body string, cookie *http.Cookie, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

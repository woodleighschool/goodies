package authhuma

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/woodleighschool/goodies/auth/authn"
	"github.com/woodleighschool/goodies/auth/browser"
	"github.com/woodleighschool/goodies/auth/internal/authtest"
)

func TestBrowserPasswordAdmissionAndRevocation(t *testing.T) {
	const password = "synthetic-test-password"
	hash, err := authn.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	principal := authn.Principal{ID: 42, Name: "Synthetic User", Email: "person@example.invalid"}
	store := &principalStore{principal: &principal, identity: &authn.PasswordIdentity{Principal: principal, PasswordHash: hash}, key: "synthetic-key"}
	sessions, _ := loadedSession(t)
	service := newAuthentication(t, store, sessions)
	var admitted bool
	var admissionErr error
	var logs bytes.Buffer
	browser := browser.New(service, browser.Config{
		Admit:  func(context.Context, int64) (bool, error) { return admitted, admissionErr },
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	handler := browserTestHandler(t, browser, sessions, slog.New(slog.NewJSONHandler(&logs, nil)))
	login := func() *httptest.ResponseRecorder {
		return browserRequest(t, handler, http.MethodPost, sessionPath, `{"email":"person@example.invalid","password":"`+password+`"}`, nil, "")
	}
	denied := login()
	if denied.Code != http.StatusUnauthorized || len(denied.Result().Cookies()) != 0 {
		t.Fatalf("denied login = %d cookies=%v", denied.Code, denied.Result().Cookies())
	}
	admissionErr = errors.New("synthetic grant store failure")
	failed := login()
	if failed.Code != http.StatusInternalServerError || strings.Contains(failed.Body.String(), admissionErr.Error()) || !strings.Contains(logs.String(), admissionErr.Error()) {
		t.Fatalf("failed login = %d %s logs=%s", failed.Code, failed.Body.String(), logs.String())
	}
	admissionErr, admitted = nil, true
	loggedIn := login()
	if loggedIn.Code != http.StatusOK {
		t.Fatalf("login = %d %s", loggedIn.Code, loggedIn.Body.String())
	}
	cookie := loggedIn.Result().Cookies()[0]
	var user authn.Principal
	if err := json.Unmarshal(loggedIn.Body.Bytes(), &user); err != nil || user != principal {
		t.Fatalf("login principal = %+v error=%v", user, err)
	}
	for _, revoked := range []bool{false, true} {
		admitted = !revoked
		want := http.StatusNoContent
		if revoked {
			want = http.StatusUnauthorized
		}
		for _, key := range []string{"", "synthetic-key"} {
			rec := browserRequest(t, handler, http.MethodGet, "/protected", "", cookie, key)
			if rec.Code != want {
				t.Fatalf("revoked=%t bearer=%t: %d %s", revoked, key != "", rec.Code, rec.Body.String())
			}
		}
		session := browserRequest(t, handler, http.MethodGet, sessionPath, "", cookie, "")
		var body sessionBody
		if err := json.Unmarshal(session.Body.Bytes(), &body); err != nil || session.Code != http.StatusOK || (body.User == nil) != revoked {
			t.Fatalf("session revoked=%t: %d %s error=%v", revoked, session.Code, session.Body.String(), err)
		}
	}
	admitted = true
	logout := browserRequest(t, handler, http.MethodDelete, sessionPath, "", cookie, "")
	if logout.Code != http.StatusNoContent {
		t.Fatalf("logout = %d %s", logout.Code, logout.Body.String())
	}
	if rec := browserRequest(t, handler, http.MethodGet, "/protected", "", cookie, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("logged-out cookie = %d", rec.Code)
	}
}

func TestPasswordThrottlePrecedesSessionLoadingAndValidation(t *testing.T) {
	sessions, _ := loadedSession(t)
	store := &countingSessionStore{Store: sessions.Store}
	sessions.Store = store
	browser := browser.New(newAuthentication(t, nil, sessions), browser.Config{})
	handler := browserTestHandler(t, browser, sessions, slog.New(slog.DiscardHandler))
	cookie := &http.Cookie{Name: sessions.Cookie.Name, Value: "synthetic-session-token"} //nolint:gosec // An incoming Cookie header has no response-only attributes.
	for range 4 {
		rec := browserRequest(t, handler, http.MethodPost, sessionPath, `{}`, cookie, "")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("malformed login = %d %s", rec.Code, rec.Body.String())
		}
	}
	lookups := store.lookups
	limited := browserRequest(t, handler, http.MethodPost, sessionPath, `{}`, cookie, "")
	seconds, err := strconv.Atoi(limited.Header().Get("Retry-After"))
	if limited.Code != http.StatusTooManyRequests || err != nil || seconds < 1 || store.lookups != lookups || lookups != 4 {
		t.Fatalf("throttled login = %d retry=%q store lookups=%d/%d", limited.Code, limited.Header().Get("Retry-After"), store.lookups, lookups)
	}
	if limited.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatal("throttle response is not a problem document")
	}
	for path, status := range map[string]int{sessionPath: http.StatusOK, "/api/auth/sso/start": http.StatusNotFound} {
		if rec := browserRequest(t, handler, http.MethodGet, path, "", nil, ""); rec.Code != status {
			t.Fatalf("GET %s after throttle = %d", path, rec.Code)
		}
	}
}

type countingSessionStore struct {
	scs.Store
	lookups int
}

func (s *countingSessionStore) Find(token string) ([]byte, bool, error) {
	s.lookups++
	return s.Store.Find(token)
}

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
			service := newAuthentication(t, &principalStore{principal: &authn.Principal{ID: 42}}, sessions)
			if err := service.ConfigureOIDC(t.Context(), authn.OIDCConfig{IssuerURL: provider.URL, ClientID: "client-id", ClientSecret: "synthetic-secret", RedirectURL: "https://app.example.invalid/callback"}); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			admitted, admissionErr := tc.admitted, tc.admissionErr
			browser := browser.New(service, browser.Config{
				Admit:           func(context.Context, int64) (bool, error) { return admitted, admissionErr },
				SuccessRedirect: "/app/home", FailureRedirect: "/sign-in?from=app",
				Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
			})
			handler := browserTestHandler(t, browser, sessions, slog.New(slog.NewJSONHandler(&logs, nil)))
			start := browserRequest(t, handler, http.MethodGet, "/api/auth/sso/start", "", nil, "")
			authorize, err := url.Parse(start.Header().Get("Location"))
			if err != nil || start.Code != http.StatusFound {
				t.Fatalf("SSO start = %d %s error=%v", start.Code, start.Body.String(), err)
			}
			cookie := start.Result().Cookies()[0]
			query := authorize.Query()
			path := "/api/auth/sso/callback?" + url.Values{"state": {query.Get("state")}, "code": {query.Get("nonce")}}.Encode()
			callback := browserRequest(t, handler, http.MethodGet, path, "", cookie, "")
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
			session := browserRequest(t, handler, http.MethodGet, sessionPath, "", cookie, "")
			var body sessionBody
			if err := json.Unmarshal(session.Body.Bytes(), &body); err != nil || (body.User != nil) != tc.admitted {
				t.Fatalf("SSO session = %s error=%v", session.Body.String(), err)
			}
		})
	}
}

func TestBrowserSSOCallbackErrors(t *testing.T) {
	browser := browser.New(nil, browser.Config{FailureRedirect: "/login"})
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

func browserTestHandler(t *testing.T, flow *browser.Service, sessions *scs.SessionManager, logger *slog.Logger) http.Handler {
	t.Helper()
	sessions.Cookie = scs.SessionCookie{Name: "session", Path: "/", HttpOnly: true}
	router := http.NewServeMux()
	cfg := huma.DefaultConfig("test", "test")
	cfg.OpenAPIPath, cfg.DocsPath, cfg.SchemasPath = "", "", ""
	api := humago.New(router, cfg)
	session := huma.NewGroup(api)
	session.UseMiddleware(OptionalAuth(api, flow, logger))
	protected := huma.NewGroup(api)
	protected.UseMiddleware(RequireAuth(api, flow, logger))
	protected.UseModifier(ProtectedOperation(api))
	passwordRouter := http.NewServeMux()
	password := humago.New(passwordRouter, cfg)
	RegisterSessions(session, password, protected, flow, logger, "Session")
	router.Handle("POST "+sessionPath, flow.LimitPasswordLogin(sessions.LoadAndSave(passwordRouter)))
	router.HandleFunc("GET /api/auth/sso/start", flow.SSOStart)
	router.HandleFunc("GET /api/auth/sso/callback", flow.SSOCallback)
	router.Handle("GET /protected", browser.RequireHTTP(flow, logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })))
	ordinary := sessions.LoadAndSave(router)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == sessionPath {
			router.ServeHTTP(w, r)
			return
		}
		ordinary.ServeHTTP(w, r)
	})
}

func browserRequest(t *testing.T, handler http.Handler, method, path, body string, cookie *http.Cookie, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
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

func TestAuthenticateBearerPrecedence(t *testing.T) {
	sessions, ctx := loadedSession(t)

	store := &principalStore{principal: &authn.Principal{ID: 42}, key: "valid-key"}
	authentication := newAuthentication(t, store, sessions)
	if err := authentication.StartSession(ctx, 42); err != nil {
		t.Fatal(err)
	}
	service := browser.New(authentication, browser.Config{Admit: func(context.Context, int64) (bool, error) { return true, nil }})
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
				if !errors.Is(err, authn.ErrNotAuthenticated) || principal != nil {
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

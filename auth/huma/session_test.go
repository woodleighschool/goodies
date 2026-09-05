package authhuma

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/woodleighschool/goodies/auth/authn"
)

func TestSessionHTTPContract(t *testing.T) {
	// The adapter must accept credentials permitted by the application store.
	const password = ""
	hash, err := authn.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	principal := authn.Principal{ID: 42, Name: "Synthetic User", Email: "person@example.invalid"}
	store := &principalStore{principal: &principal, identity: &authn.PasswordIdentity{Principal: principal, PasswordHash: hash}, key: "synthetic-key"}
	sessions, _ := loadedSession(t)
	var admitted bool
	var admissionErr error
	var logs bytes.Buffer
	browser, err := authn.New(t.Context(), store, sessions, authn.Config{
		Admit:  func(context.Context, int64) (bool, error) { return admitted, admissionErr },
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := sessionTestHandler(t, browser, sessions, slog.New(slog.NewJSONHandler(&logs, nil)))
	login := func() *httptest.ResponseRecorder {
		return sessionRequest(t, handler, http.MethodPost, `{"email":"person@example.invalid","password":"`+password+`"}`, nil)
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
	current := sessionRequest(t, handler, http.MethodGet, "", cookie)
	var session sessionBody
	if err := json.Unmarshal(current.Body.Bytes(), &session); err != nil || current.Code != http.StatusOK || session.User == nil || *session.User != principal {
		t.Fatalf("session=%s error=%v", current.Body.String(), err)
	}
	admitted = false
	logout := sessionRequest(t, handler, http.MethodDelete, "", cookie)
	if logout.Code != http.StatusNoContent {
		t.Fatalf("logout = %d %s", logout.Code, logout.Body.String())
	}
	admitted = true
	current = sessionRequest(t, handler, http.MethodGet, "", cookie)
	session = sessionBody{}
	if err := json.Unmarshal(current.Body.Bytes(), &session); err != nil || current.Code != http.StatusOK || session.User != nil {
		t.Fatalf("logged-out session=%s error=%v", current.Body.String(), err)
	}

}

func sessionTestHandler(t *testing.T, flow *authn.Service, sessions *scs.SessionManager, logger *slog.Logger) http.Handler {
	t.Helper()
	sessions.Cookie = scs.SessionCookie{Name: "session", Path: "/", HttpOnly: true}
	router := http.NewServeMux()
	cfg := huma.DefaultConfig("test", "test")
	cfg.OpenAPIPath, cfg.DocsPath, cfg.SchemasPath = "", "", ""
	api := humago.New(router, cfg)
	session := huma.NewGroup(api)
	session.UseMiddleware(OptionalAuth(api, flow, logger))
	passwordRouter := http.NewServeMux()
	password := humago.New(passwordRouter, cfg)
	RegisterSessions(session, password, api, flow, logger, "Session")
	if response := api.OpenAPI().Paths[sessionPath].Post.Responses["429"]; response == nil || response.Headers["Retry-After"] == nil {
		t.Fatal("missing throttle retry contract")
	}
	if security := api.OpenAPI().Paths[sessionPath].Delete.Security; len(security) != 0 {
		t.Fatal("logout requires admission")
	}

	router.Handle("POST "+sessionPath, flow.LimitPasswordLogin(sessions.LoadAndSave(passwordRouter)))
	ordinary := sessions.LoadAndSave(router)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == sessionPath {
			router.ServeHTTP(w, r)
			return
		}
		ordinary.ServeHTTP(w, r)
	})
}

func sessionRequest(t *testing.T, handler http.Handler, method, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, sessionPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

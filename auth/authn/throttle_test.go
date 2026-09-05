package authn

import (
	"github.com/alexedwards/scs/v2"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestPasswordThrottlePrecedesSessionLoading(t *testing.T) {
	sessions, _ := loadedSession(t)
	store := &countingSessionStore{Store: sessions.Store}
	sessions.Store = store
	service, err := New(t.Context(), &principalStore{}, sessions, Config{Admit: allowPrincipal})
	if err != nil {
		t.Fatal(err)
	}
	var attempts int
	handler := service.LimitPasswordLogin(sessions.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusUnprocessableEntity)
	})))
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/login", nil)
		req.Header.Set("Cookie", "session=synthetic-session-token")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	sessions.Cookie = scs.SessionCookie{Name: "session", Path: "/", HttpOnly: true}
	for range 4 {
		if rec := request(); rec.Code != 422 {
			t.Fatalf("initial attempt=%d", rec.Code)
		}
	}
	rec := request()
	seconds, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if rec.Code != 429 || err != nil || seconds < 1 || attempts != 4 || store.lookups != 4 {
		t.Fatalf("limited=%d retry=%q attempts=%d lookups=%d", rec.Code, rec.Header().Get("Retry-After"), attempts, store.lookups)
	}
	if rec.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatal("throttle response is not a problem document")
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

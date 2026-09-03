package authn

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/alexedwards/scs/v2/memstore"
)

func loadedSession(t *testing.T) (*scs.SessionManager, context.Context) {
	t.Helper()
	// Avoid the default store's asynchronous cleanup startup in short-lived tests.
	sessions := &scs.SessionManager{
		Store:    memstore.NewWithCleanupInterval(0),
		Codec:    scs.GobCodec{},
		Lifetime: time.Hour,
	}
	ctx, err := sessions.Load(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	return sessions, ctx
}

type principalStore struct {
	principal *Principal
	identity  *PasswordIdentity
	err       error
	key       string
	email     string
	id        int64
}

func (s *principalStore) GetPrincipal(_ context.Context, id int64) (*Principal, error) {
	s.id = id
	return s.principal, s.err
}

func (s *principalStore) GetPrincipalByAPIKey(_ context.Context, key string) (*Principal, error) {
	if s.err != nil {
		return nil, s.err
	}
	if key != s.key || key == "" {
		return nil, ErrPrincipalNotFound
	}
	return s.principal, nil
}

func (s *principalStore) GetPasswordIdentityByEmail(_ context.Context, email string) (*PasswordIdentity, error) {
	s.email = email
	return s.identity, s.err
}

func (s *principalStore) GetSSOPrincipalByEmail(_ context.Context, email string) (*Principal, error) {
	s.email = email
	return s.principal, s.err
}

func (s *principalStore) SetAPIKey(_ context.Context, id int64, key string) error {
	if s.err != nil {
		return s.err
	}
	s.id, s.key = id, key
	return nil
}

func (s *principalStore) ClearAPIKey(_ context.Context, id int64) error {
	if s.err != nil {
		return s.err
	}
	s.id, s.key = id, ""
	return nil
}

func TestSessionLifecycle(t *testing.T) {
	sessions, ctx := loadedSession(t)
	store := &principalStore{principal: &Principal{ID: 42}}
	service := &Service{principals: store, sessions: sessions}
	sessions.Put(ctx, "anonymous-data", "value")
	oldToken, _, err := sessions.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.StartSession(ctx, 42); err != nil {
		t.Fatal(err)
	}
	token, _, err := sessions.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if token == oldToken {
		t.Fatal("login retained anonymous token")
	}
	oldCtx, err := sessions.Load(t.Context(), oldToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CurrentPrincipal(oldCtx); !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("old session: %v", err)
	}
	ctx, err = sessions.Load(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := service.CurrentPrincipal(ctx)
	if err != nil || principal.ID != 42 || store.id != 42 {
		t.Fatalf("principal=%v error=%v", principal, err)
	}
	if err := service.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	ctx, err = sessions.Load(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CurrentPrincipal(ctx); !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("logged-out session: %v", err)
	}
}

func TestCurrentPrincipalDistinguishesMissingIdentityFromStoreFailure(t *testing.T) {
	broken := errors.New("store unavailable")
	for _, tc := range []struct {
		name              string
		storeErr, wantErr error
		destroy           bool
	}{
		{"missing", fmt.Errorf("lookup: %w", ErrPrincipalNotFound), ErrNotAuthenticated, true},
		{"unavailable", broken, broken, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions, ctx := loadedSession(t)
			sessions.Put(ctx, sessionUserIDKey, int64(42))
			service := &Service{principals: &principalStore{err: tc.storeErr}, sessions: sessions}
			if _, err := service.CurrentPrincipal(ctx); !errors.Is(err, tc.wantErr) {
				t.Fatalf("error=%v", err)
			}
			if got := sessions.GetInt64(ctx, sessionUserIDKey); (got == 0) != tc.destroy {
				t.Fatalf("session principal=%d", got)
			}
		})
	}
}

func TestMissingPrincipalPreservesSessionRevocationFailure(t *testing.T) {
	sessions, ctx := loadedSession(t)
	service := &Service{principals: &principalStore{err: ErrPrincipalNotFound}, sessions: sessions}
	if err := service.StartSession(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sessions.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	broken := errors.New("session store unavailable")
	sessions.Store = failingDeleteStore{Store: sessions.Store, err: broken}
	if _, err := service.CurrentPrincipal(ctx); !errors.Is(err, broken) {
		t.Fatalf("session revocation error = %v, want %v", err, broken)
	}
}

type failingDeleteStore struct {
	scs.Store
	err error
}

func (s failingDeleteStore) Delete(string) error { return s.err }

func TestAPIKeyRotationAndRevocation(t *testing.T) {
	store := &principalStore{principal: &Principal{ID: 42}}
	service := &Service{principals: store}
	if err := service.RotateAPIKey(t.Context(), 42); err != nil {
		t.Fatal(err)
	}
	old := store.key
	decoded, err := base64.RawURLEncoding.DecodeString(old)
	if err != nil || len(decoded) != 24 || store.id != 42 {
		t.Fatalf("invalid generated key: decoded length=%d error=%v", len(decoded), err)
	}
	if err := service.RotateAPIKey(t.Context(), 42); err != nil {
		t.Fatal(err)
	}
	if old == store.key {
		t.Fatal("rotation retained old key")
	}
	if _, err := service.AuthenticateAPIKey(t.Context(), old); !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("old key: %v", err)
	}
	if p, err := service.AuthenticateAPIKey(t.Context(), store.key); err != nil || p.ID != 42 {
		t.Fatalf("new key: %v %v", p, err)
	}
	key := store.key
	if err := service.RevokeAPIKey(t.Context(), 42); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AuthenticateAPIKey(t.Context(), key); !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("revoked key: %v", err)
	}
	broken := errors.New("store unavailable")
	store.err = broken
	for _, err := range []error{service.RotateAPIKey(t.Context(), 42), service.RevokeAPIKey(t.Context(), 42)} {
		if !errors.Is(err, broken) {
			t.Fatalf("store error: %v", err)
		}
	}
	if _, err := service.AuthenticateAPIKey(t.Context(), "key"); !errors.Is(err, broken) {
		t.Fatalf("lookup error: %v", err)
	}
}

func TestPasswordAuthentication(t *testing.T) {
	const password = "correct horse battery staple"
	sessions, ctx := loadedSession(t)
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	store := &principalStore{identity: &PasswordIdentity{ID: 42, PasswordHash: hash}}
	service, err := NewService(store, sessions)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := service.AuthenticatePassword(ctx, LoginParams{Email: " person@example.invalid ", Password: password})
	if err != nil || principal.ID != 42 || store.email != "person@example.invalid" {
		t.Fatalf("principal=%v email=%q error=%v", principal, store.email, err)
	}
	if sessions.GetInt64(ctx, sessionUserIDKey) != 0 {
		t.Fatal("password authentication created session before admission")
	}
	for _, tc := range []struct {
		name, password string
		missing        bool
	}{
		{"wrong password", "incorrect password", false}, {"unknown identity", password, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.missing {
				store.err = fmt.Errorf("lookup: %w", ErrPrincipalNotFound)
			}
			started := time.Now()
			p, err := service.AuthenticatePassword(ctx, LoginParams{Password: tc.password})
			if p != nil || !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("principal=%v error=%v", p, err)
			}
			if time.Since(started) < minimumCredentialFailureDuration {
				t.Fatal("credential failure was not padded")
			}
		})
	}
	broken := errors.New("store unavailable")
	store.err = broken
	if _, err := service.AuthenticatePassword(ctx, LoginParams{}); !errors.Is(err, broken) {
		t.Fatalf("store failure: %v", err)
	}
	store.err = nil
	store.identity.PasswordHash = "malformed"
	if p, err := service.AuthenticatePassword(ctx, LoginParams{}); p != nil || err == nil || errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("malformed hash: %v %v", p, err)
	}
}

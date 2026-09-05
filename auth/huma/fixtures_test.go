package authhuma

import (
	"context"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/alexedwards/scs/v2/memstore"
	"github.com/woodleighschool/goodies/auth/authn"
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
	principal *authn.Principal
	identity  *authn.PasswordIdentity
	err       error
	key       string
	email     string
	id        int64
}

func (s *principalStore) GetPrincipal(_ context.Context, id int64) (*authn.Principal, error) {
	s.id = id
	return s.principal, s.err
}

func (s *principalStore) GetPrincipalByAPIKey(_ context.Context, key string) (*authn.Principal, error) {
	if s.err != nil {
		return nil, s.err
	}
	if key != s.key || key == "" {
		return nil, authn.ErrPrincipalNotFound
	}
	return s.principal, nil
}

func (s *principalStore) GetPasswordIdentityByEmail(_ context.Context, email string) (*authn.PasswordIdentity, error) {
	s.email = email
	return s.identity, s.err
}

func (s *principalStore) GetSSOPrincipalByEmail(_ context.Context, email string) (*authn.Principal, error) {
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

package authz

import (
	"context"
	"errors"
	"maps"
	"testing"
)

type grantStore struct {
	grants []Grant
	err    error
}

func (s *grantStore) Grants(context.Context, int64) ([]Grant, error) { return s.grants, s.err }

func newService(t *testing.T, store *grantStore) *Service {
	t.Helper()
	s, err := NewService(store, []Resource{"users", "reports", "settings"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCatalogueValidationAndOwnership(t *testing.T) {
	for _, resources := range [][]Resource{nil, {}, {""}, {" "}, {"users", "users"}} {
		if _, err := NewService(&grantStore{}, resources); err == nil {
			t.Fatalf("accepted catalogue %q", resources)
		}
	}
	resources := []Resource{"users"}
	s, err := NewService(&grantStore{}, resources)
	if err != nil {
		t.Fatal(err)
	}
	resources[0] = "changed"
	permissions, err := s.EffectivePermissions(t.Context(), 42)
	if err != nil || !maps.Equal(permissions, map[Resource]Access{"users": None}) {
		t.Fatalf("permissions=%v error=%v", permissions, err)
	}
	permissions["users"] = Edit
	again, err := s.EffectivePermissions(t.Context(), 42)
	if err != nil || again["users"] != None {
		t.Fatalf("result mutation changed policy: %v %v", again, err)
	}
}

func TestMergedGrantsUseHighestAccess(t *testing.T) {
	s := newService(t, &grantStore{grants: []Grant{
		{"users", View}, {"reports", Edit}, {"users", Edit}, {"users", None}, {"reports", View},
	}})
	permissions, err := s.EffectivePermissions(t.Context(), 42)
	want := map[Resource]Access{"users": Edit, "reports": Edit, "settings": None}
	if err != nil || !maps.Equal(permissions, want) {
		t.Fatalf("permissions=%v error=%v", permissions, err)
	}
	if allowed, err := s.HasAccess(t.Context(), 42); err != nil || !allowed {
		t.Fatalf("HasAccess=%v error=%v", allowed, err)
	}
}

func TestGrantFailuresDiscardAllPermissions(t *testing.T) {
	broken := errors.New("store unavailable")
	for _, tc := range []struct {
		name  string
		store grantStore
		want  error
	}{
		{"unknown resource", grantStore{grants: []Grant{{"users", Edit}, {"unknown", View}}}, ErrUnknownResource},
		{"invalid access", grantStore{grants: []Grant{{"users", Edit}, {"reports", "manage"}}}, ErrInvalidAccess},
		{"empty access", grantStore{grants: []Grant{{"users", ""}}}, ErrInvalidAccess},
		{"query error", grantStore{grants: []Grant{{"users", Edit}}, err: broken}, broken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newService(t, &tc.store)
			if permissions, err := s.EffectivePermissions(t.Context(), 42); permissions != nil || !errors.Is(err, tc.want) {
				t.Fatalf("permissions=%v error=%v", permissions, err)
			}
			checks := []func() (bool, error){
				func() (bool, error) { return s.HasAccess(t.Context(), 42) },
				func() (bool, error) { return s.Can(t.Context(), 42, "users", View) },
				func() (bool, error) { return s.CanAll(t.Context(), 42) },
			}
			for _, check := range checks {
				if allowed, err := check(); allowed || !errors.Is(err, tc.want) {
					t.Fatalf("allowed=%v error=%v", allowed, err)
				}
			}
		})
	}
}

func TestNoGrantsAndRequirements(t *testing.T) {
	store := &grantStore{}
	s := newService(t, store)
	permissions, err := s.EffectivePermissions(t.Context(), 42)
	if err != nil || !maps.Equal(permissions, map[Resource]Access{"users": None, "reports": None, "settings": None}) {
		t.Fatalf("permissions=%v error=%v", permissions, err)
	}
	if allowed, err := s.HasAccess(t.Context(), 42); err != nil || allowed {
		t.Fatalf("HasAccess=%v error=%v", allowed, err)
	}
	if allowed, err := s.CanAll(t.Context(), 42); err != nil || !allowed {
		t.Fatalf("empty CanAll=%v error=%v", allowed, err)
	}
	for _, required := range []Access{None, View, Edit} {
		allowed, err := s.Can(t.Context(), 42, "users", required)
		if err != nil || allowed != (required == None) {
			t.Fatalf("Can(%s)=%v error=%v", required, allowed, err)
		}
	}
	store.grants = []Grant{{"users", Edit}, {"reports", View}}
	for _, tc := range []struct {
		requirements []Requirement
		want         bool
	}{
		{[]Requirement{{"users", View}, {"reports", View}}, true},
		{[]Requirement{{"users", Edit}, {"reports", Edit}}, false},
		{[]Requirement{{"users", Edit}, {"settings", None}}, true},
		{[]Requirement{{"users", Edit}, {"settings", View}}, false},
	} {
		if allowed, err := s.CanAll(t.Context(), 42, tc.requirements...); err != nil || allowed != tc.want {
			t.Fatalf("CanAll(%v)=%v error=%v", tc.requirements, allowed, err)
		}
	}
}

func TestInvalidRequirementsFailClosed(t *testing.T) {
	s := newService(t, &grantStore{})
	for _, tc := range []struct {
		requirement Requirement
		want        error
	}{
		{Requirement{"unknown", View}, ErrUnknownResource},
		{Requirement{"users", "manage"}, ErrInvalidAccess},
	} {
		if allowed, err := s.Can(t.Context(), 42, tc.requirement.Resource, tc.requirement.Access); allowed || !errors.Is(err, tc.want) {
			t.Fatalf("Can=%v error=%v", allowed, err)
		}
		if allowed, err := s.CanAll(t.Context(), 42, Requirement{"users", View}, tc.requirement); allowed || !errors.Is(err, tc.want) {
			t.Fatalf("CanAll=%v error=%v", allowed, err)
		}
	}
}

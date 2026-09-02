package authn

import (
	"context"
	"errors"
	"testing"
)

func TestPrincipalRoundTrip(t *testing.T) {
	if got := CurrentPrincipalID(context.Background()); got != nil {
		t.Fatalf("anonymous principal ID = %v, want nil", got)
	}

	want := &Principal{ID: 42, Email: "person@example.invalid"}
	ctx := WithPrincipal(context.Background(), want)

	got, ok := PrincipalFromContext(ctx)
	if !ok || got != want {
		t.Fatalf("Principal = %#v, %t", got, ok)
	}
	if current := CurrentPrincipalID(ctx); current == nil || *current != want.ID {
		t.Fatalf("CurrentPrincipalID = %v", current)
	}
}

func TestRequirePrincipalRejectsAnonymousContext(t *testing.T) {
	for _, ctx := range []context.Context{t.Context(), WithPrincipal(t.Context(), nil)} {
		if _, err := RequirePrincipal(ctx); !errors.Is(err, ErrNotAuthenticated) {
			t.Fatalf("anonymous principal error = %v", err)
		}
		if _, ok := PrincipalFromContext(ctx); ok || CurrentPrincipalID(ctx) != nil {
			t.Fatal("anonymous context contains a principal")
		}
	}
}

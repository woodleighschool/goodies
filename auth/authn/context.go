package authn

import "context"

type principalContextKey struct{}

// WithPrincipal attaches the authenticated principal to ctx.
func WithPrincipal(ctx context.Context, principal *Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, principal)
}

// PrincipalFromContext returns the authenticated principal, if present.
func PrincipalFromContext(ctx context.Context) (*Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(*Principal)
	return principal, ok && principal != nil
}

// RequirePrincipal returns the authenticated principal or ErrNotAuthenticated.
func RequirePrincipal(ctx context.Context) (*Principal, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, ErrNotAuthenticated
	}
	return principal, nil
}

// CurrentPrincipalID returns the authenticated principal's ID, or nil if anonymous.
func CurrentPrincipalID(ctx context.Context) *int64 {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil
	}
	return &principal.ID
}

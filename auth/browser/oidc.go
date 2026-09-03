package browser

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/woodleighschool/goodies/auth/authn"
)

// SSOStart redirects to the configured identity provider using the loaded session.
func (b *Service) SSOStart(w http.ResponseWriter, r *http.Request) {
	if !b.service.SSOEnabled() {
		http.Error(w, "sso not configured", http.StatusNotFound)
		return
	}
	authURL, err := b.service.BeginSSO(r.Context())
	if err != nil {
		b.logger.ErrorContext(r.Context(), "oidc start failed", "operation", "oidc-start", "err", err)
		http.Error(w, "sso sign-in failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, authURL, http.StatusFound)
}

// SSOCallback verifies the callback and admits the identity before creating a session.
func (b *Service) SSOCallback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if providerErr := query.Get("error"); providerErr != "" {
		b.redirectSSOError(w, r, providerErr)
		return
	}
	state, code := query.Get("state"), query.Get("code")
	if state == "" || code == "" {
		b.redirectSSOError(w, r, "missing state or code")
		return
	}
	principal, err := b.service.CompleteSSO(r.Context(), state, code)
	if err == nil {
		err = b.startSession(r.Context(), principal)
	}
	if err != nil {
		message := oidcUserMessage(err)
		if message == "sso sign-in failed" {
			b.logger.ErrorContext(r.Context(), "oidc callback failed", "operation", "oidc-callback", "err", err)
		}
		b.redirectSSOError(w, r, message)
		return
	}
	http.Redirect(w, r, b.successRedirect, http.StatusFound)
}

func oidcUserMessage(err error) string {
	switch {
	case errors.Is(err, authn.ErrSSOStateMismatch):
		return "sso state mismatch; try again"
	case errors.Is(err, authn.ErrSSONonceMismatch):
		return "sso nonce mismatch; try again"
	case errors.Is(err, authn.ErrSSOUnknownUser), errors.Is(err, authn.ErrNotAuthenticated):
		return "no account for this identity"
	case errors.Is(err, authn.ErrSSOEmailClaimEmpty):
		return "identity provider returned no email"
	default:
		return "sso sign-in failed"
	}
}

func (b *Service) redirectSSOError(w http.ResponseWriter, r *http.Request, message string) {
	target, err := url.Parse(b.failureRedirect)
	if err != nil {
		b.logger.ErrorContext(r.Context(), "invalid login redirect", "operation", "oidc-callback", "err", err)
		http.Error(w, "sso sign-in failed", http.StatusInternalServerError)
		return
	}
	query := target.Query()
	query.Set("sso_error", message)
	target.RawQuery = query.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

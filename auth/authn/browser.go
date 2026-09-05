package authn

import (
	"errors"
	"net/http"
	"net/url"
)

// SSOStart redirects to the configured identity provider using the loaded session.
func (s *Service) SSOStart(w http.ResponseWriter, r *http.Request) {
	if !s.SSOEnabled() {
		http.Error(w, "sso not configured", http.StatusNotFound)
		return
	}
	authURL, err := s.beginSSO(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "oidc start failed", "operation", "oidc-start", "err", err)
		http.Error(w, "sso sign-in failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, authURL, http.StatusFound)
}

// SSOCallback verifies the callback and admits the identity before creating a session.
func (s *Service) SSOCallback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if providerErr := query.Get("error"); providerErr != "" {
		s.redirectSSOError(w, r, providerErr)
		return
	}
	state, code := query.Get("state"), query.Get("code")
	if state == "" || code == "" {
		s.redirectSSOError(w, r, "missing state or code")
		return
	}
	principal, err := s.completeSSO(r.Context(), state, code)
	if err == nil {
		err = s.startSession(r.Context(), principal.ID)
	}
	if err != nil {
		message := oidcUserMessage(err)
		if message == "sso sign-in failed" {
			s.logger.ErrorContext(r.Context(), "oidc callback failed", "operation", "oidc-callback", "err", err)
		}
		s.redirectSSOError(w, r, message)
		return
	}
	http.Redirect(w, r, s.successRedirect, http.StatusFound)
}

func oidcUserMessage(err error) string {
	switch {
	case errors.Is(err, ErrSSOStateMismatch):
		return "sso state mismatch; try again"
	case errors.Is(err, ErrSSONonceMismatch):
		return "sso nonce mismatch; try again"
	case errors.Is(err, ErrSSOUnknownUser), errors.Is(err, ErrNotAuthenticated):
		return "no account for this identity"
	case errors.Is(err, ErrSSOEmailClaimEmpty):
		return "identity provider returned no email"
	default:
		return "sso sign-in failed"
	}
}

func (s *Service) redirectSSOError(w http.ResponseWriter, r *http.Request, message string) {
	target, err := url.Parse(s.failureRedirect)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "invalid login redirect", "operation", "oidc-callback", "err", err)
		http.Error(w, "sso sign-in failed", http.StatusInternalServerError)
		return
	}
	query := target.Query()
	query.Set("sso_error", message)
	target.RawQuery = query.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

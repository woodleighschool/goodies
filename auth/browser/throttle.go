package browser

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
)

// LimitPasswordLogin admits ten attempts per minute with a burst of four.
// Mount it only on password login, before session loading and body validation.
func (b *Service) LimitPasswordLogin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b.loginLimiter.Allow() {
			next.ServeHTTP(w, r)
			return
		}
		seconds := max(int(math.Ceil((1-b.loginLimiter.Tokens())/float64(b.loginLimiter.Limit()))), 1)
		w.Header().Set("Content-Type", "application/problem+json")
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(struct {
			Title  string `json:"title"`
			Status int    `json:"status"`
			Detail string `json:"detail"`
		}{http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests, "too many login attempts; try again shortly"})
	})
}

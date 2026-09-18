package api

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// wrapAuth enforces an optional bearer token on every route when
// SAN_API_TOKEN is configured; without a token the API stays open (the
// devnet default). The comparison is constant time and failures use the
// FastAPI error shape: 401 {"detail": "Unauthorized"}.
func (s *Server) wrapAuth(next http.Handler) http.Handler {
	token := strings.TrimSpace(s.config.APIToken)
	if token == "" {
		return next
	}
	expected := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(provided, expected) != 1 {
			writeError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

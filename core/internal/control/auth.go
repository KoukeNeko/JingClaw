package control

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// Binding to 127.0.0.1 is not by itself a security boundary: any web page the
// user visits can issue requests to localhost. This API will grow a shell, so
// it authenticates from the start rather than acquiring a permissive default
// that becomes impossible to tighten later.

const tokenBytes = 32 // 256 bits

// NewToken returns a fresh control token.
func NewToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("control: generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// AuthMiddleware rejects requests without the daemon's bearer token, and
// requests whose Host header does not name the loopback address.
//
// The Host check defends against DNS rebinding: an attacker-controlled name
// can resolve to 127.0.0.1, so the socket alone proves nothing about who is
// calling.
func AuthMiddleware(token string, allowedPort string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostAllowed(r.Host, allowedPort) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}

		provided, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(header[len(prefix):]), true
}

func hostAllowed(host, allowedPort string) bool {
	hostname, port, err := net.SplitHostPort(host)
	if err != nil {
		// No port present; the daemon always listens on an explicit one.
		return false
	}
	if allowedPort != "" && port != allowedPort {
		return false
	}

	switch hostname {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	default:
		return false
	}
}

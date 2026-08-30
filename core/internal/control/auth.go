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

// Scope is a set of services a credential may reach.
//
// Scopes exist because not every caller deserves the whole API. A gateway
// process holds a bot token for somebody else's service and runs a library
// that talks to the internet; if it is compromised, the blast radius should be
// "can deliver messages inward", not "can execute tools".
type Scope string

const (
	// ScopeControl is the operator's own credential: everything.
	ScopeControl Scope = "control"

	// ScopeGateway reaches the ingress and nothing else.
	ScopeGateway Scope = "gateway"
)

// servicesForScope lists the fully-qualified proto services a scope may call.
//
// The mapping is explicit rather than derived from a prefix, so adding a
// service does not silently widen an existing credential: a new service is
// unreachable until somebody decides which scopes should have it.
var servicesForScope = map[Scope]map[string]bool{
	ScopeControl: {
		"jingclaw.control.v1.SessionService":        true,
		"jingclaw.control.v1.ChannelService":        true,
		"jingclaw.control.v1.GatewayIngressService": true,
		"jingclaw.control.v1.ArtifactService":       true,
		"jingclaw.control.v1.MemoryService":         true,
	},
	ScopeGateway: {
		"jingclaw.control.v1.GatewayIngressService": true,
	},
}

// Token is a credential and what it may reach.
type Token struct {
	Value string
	Scope Scope
}

// NewToken returns a fresh credential for a scope.
func NewToken(scope Scope) (Token, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return Token{}, fmt.Errorf("control: generate token: %w", err)
	}
	return Token{Value: base64.RawURLEncoding.EncodeToString(buf), Scope: scope}, nil
}

// AuthMiddleware rejects requests without a known bearer token, requests whose
// Host header does not name the loopback address, and requests for a service
// the presented credential does not cover.
//
// The Host check defends against DNS rebinding: an attacker-controlled name
// can resolve to 127.0.0.1, so the socket alone proves nothing about who is
// calling.
func AuthMiddleware(tokens []Token, allowedPort string, next http.Handler) http.Handler {
	byValue := make(map[string]Scope, len(tokens))
	for _, token := range tokens {
		byValue[token.Value] = token.Scope
	}

	return RequireLoopbackHost(allowedPort, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		scope, matched := lookupToken(byValue, provided)
		if !matched {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		if !servicesForScope[scope][serviceOf(r.URL.Path)] {
			// The credential is genuine but does not cover this service.
			// Forbidden rather than unauthorized: retrying with the same token
			// will never help.
			http.Error(w, "forbidden for this credential", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	}))
}

// RequireLoopbackHost rejects a request whose Host header does not name the
// loopback address and the port this daemon is listening on.
//
// It defends against DNS rebinding: an attacker-controlled name can resolve to
// 127.0.0.1, so the socket alone proves nothing about who is calling. It is
// separate from the credential check because everything this daemon serves
// needs it, including the pages that are served without a credential.
func RequireLoopbackHost(allowedPort string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostAllowed(r.Host, allowedPort) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// lookupToken compares against every known credential rather than indexing by
// the presented value, so the time taken does not reveal which token matched a
// prefix.
func lookupToken(known map[string]Scope, provided string) (Scope, bool) {
	var (
		scope   Scope
		matched bool
	)

	for value, candidate := range known {
		if subtle.ConstantTimeCompare([]byte(value), []byte(provided)) == 1 {
			scope, matched = candidate, true
		}
	}
	return scope, matched
}

// serviceOf extracts the proto service from a Connect path, which has the
// shape /package.Service/Method.
func serviceOf(path string) string {
	trimmed := strings.TrimPrefix(path, "/")
	if index := strings.LastIndex(trimmed, "/"); index >= 0 {
		return trimmed[:index]
	}
	return trimmed
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

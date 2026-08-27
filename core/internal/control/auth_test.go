package control_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/control"
)

// Loopback is not an authorization boundary: any page a user visits can issue
// requests to 127.0.0.1, and a hostile DNS name can resolve there too. These
// cases are what stop a browser tab from driving the daemon.
func TestAuthMiddleware(t *testing.T) {
	const (
		token = "correct-token"
		port  = "49231"
	)

	handler := control.AuthMiddleware(token, port, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name       string
		host       string
		authHeader string
		wantStatus int
	}{
		{
			name:       "valid token on loopback",
			host:       "127.0.0.1:" + port,
			authHeader: "Bearer " + token,
			wantStatus: http.StatusOK,
		},
		{
			name:       "localhost is loopback too",
			host:       "localhost:" + port,
			authHeader: "Bearer " + token,
			wantStatus: http.StatusOK,
		},
		{
			name:       "bearer scheme is case insensitive",
			host:       "127.0.0.1:" + port,
			authHeader: "bearer " + token,
			wantStatus: http.StatusOK,
		},
		{
			name:       "attacker-controlled hostname resolving to loopback",
			host:       "evil.example.com:" + port,
			authHeader: "Bearer " + token,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "loopback name but wrong port",
			host:       "127.0.0.1:1234",
			authHeader: "Bearer " + token,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "no credentials",
			host:       "127.0.0.1:" + port,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong token",
			host:       "127.0.0.1:" + port,
			authHeader: "Bearer wrong",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "token without bearer scheme",
			host:       "127.0.0.1:" + port,
			authHeader: token,
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://"+tc.host+"/rpc", nil)
			req.Host = tc.host
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("got status %d, want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}

func TestNewTokenIsUniqueAndNonEmpty(t *testing.T) {
	seen := make(map[string]bool)

	for range 100 {
		token, err := control.NewToken()
		if err != nil {
			t.Fatalf("new token: %v", err)
		}
		if token == "" {
			t.Fatal("token is empty")
		}
		if seen[token] {
			t.Fatalf("token %q generated twice", token)
		}
		seen[token] = true
	}
}

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

	handler := control.AuthMiddleware(
		[]control.Token{{Value: token, Scope: control.ScopeControl}},
		nil,
		port,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
	)

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
			req := httptest.NewRequest(http.MethodPost,
				"http://"+tc.host+"/jingclaw.control.v1.SessionService/CreateSession", nil)
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

// A gateway process holds somebody else's bot token and runs a library that
// talks to the internet. If it is compromised, the blast radius must be "can
// deliver messages inward", not "can execute tools".
func TestGatewayCredentialCannotReachToolExecutingServices(t *testing.T) {
	const (
		controlValue = "control-token"
		gatewayValue = "gateway-token"
		port         = "49231"
	)

	handler := control.AuthMiddleware(
		[]control.Token{
			{Value: controlValue, Scope: control.ScopeControl},
			{Value: gatewayValue, Scope: control.ScopeGateway},
		},
		nil,
		port,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
	)

	cases := []struct {
		name       string
		token      string
		path       string
		wantStatus int
	}{
		{
			name:       "gateway reaches the ingress",
			token:      gatewayValue,
			path:       "/jingclaw.control.v1.GatewayIngressService/DeliverInbound",
			wantStatus: http.StatusOK,
		},
		{
			name:       "gateway cannot start a run directly",
			token:      gatewayValue,
			path:       "/jingclaw.control.v1.SessionService/SendTurn",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "gateway cannot approve a tool call",
			token:      gatewayValue,
			path:       "/jingclaw.control.v1.SessionService/DecideApproval",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "gateway cannot rewrite its own binding",
			token:      gatewayValue,
			path:       "/jingclaw.control.v1.ChannelService/UpsertBinding",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "control reaches everything",
			token:      controlValue,
			path:       "/jingclaw.control.v1.SessionService/SendTurn",
			wantStatus: http.StatusOK,
		},
		{
			name:       "an unknown service is unreachable by anyone",
			token:      controlValue,
			path:       "/jingclaw.control.v1.FutureService/DoThing",
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := "127.0.0.1:" + port
			req := httptest.NewRequest(http.MethodPost, "http://"+host+tc.path, nil)
			req.Host = host
			req.Header.Set("Authorization", "Bearer "+tc.token)

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
		token, err := control.NewToken(control.ScopeControl)
		if err != nil {
			t.Fatalf("new token: %v", err)
		}
		if token.Value == "" {
			t.Fatal("token is empty")
		}
		if seen[token.Value] {
			t.Fatal("the same token was generated twice")
		}
		seen[token.Value] = true
	}
}

// The console is served without a credential — a browser cannot present one on
// the request that fetches the page it would get the token from — but the Host
// check still applies. An attacker-controlled name resolving to 127.0.0.1 must
// not reach anything this daemon serves.
func TestTheHostCheckAppliesWithoutACredential(t *testing.T) {
	served := control.RequireLoopbackHost("7777", http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	for name, host := range map[string]string{
		"a name that resolves here": "evil.example.com:7777",
		"the wrong port":            "127.0.0.1:9999",
		"no port at all":            "127.0.0.1",
	} {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Host = host

		recorder := httptest.NewRecorder()
		served.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusForbidden {
			t.Errorf("%s (%q) got %d, want 403", name, host, recorder.Code)
		}
	}

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Host = "127.0.0.1:7777"

	recorder := httptest.NewRecorder()
	served.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Errorf("the loopback address itself got %d", recorder.Code)
	}
}

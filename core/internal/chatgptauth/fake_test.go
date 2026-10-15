package chatgptauth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Values nothing else would say, so that finding one anywhere it should not be
// is finding a leak.
const (
	canaryAccess  = "access-canary-Zq7Tn2Wb9"
	canaryRefresh = "refresh-canary-Hc4Xs8Kd1"
	issuedClient  = "oaiapp_canary-client-Pm3Lv5"
)

var (
	keyOnce sync.Once
	testKey *rsa.PrivateKey
)

func signingKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		var err error
		if testKey, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
	})
	return testKey
}

// fakeOAuth is an authorization server built from the documented shapes.
type fakeOAuth struct {
	t      *testing.T
	server *httptest.Server

	mu sync.Mutex

	// What the last authorization asked for.
	challenge, nonce, redirect string
	registering                bool

	tokenRequests  []url.Values
	revokeRequests []url.Values
	refreshes      int
	refresh        string // the refresh token that is currently good

	// Behaviour a test changes.
	scope          string
	expiresIn      int
	idClaims       func(claims map[string]any)
	signWithOther  bool
	refreshError   string // an OAuth error code to answer a refresh with
	refreshStatus  int
	refreshGate    chan struct{} // a refresh waits here, after refreshArrived is signalled
	refreshArrived chan struct{}
	revokeStatus   int
	revokeFailed   int // answer 503 this many times before the revokeStatus
}

func newFakeOAuth(t *testing.T) *fakeOAuth {
	t.Helper()
	f := &fakeOAuth{
		t: t, scope: "openid profile email offline_access resource.invoke " + ScopePlanUsage,
		expiresIn: 3600, revokeStatus: 200,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":              f.server.URL,
			"jwks_uri":            f.server.URL + "/jwks",
			"revocation_endpoint": f.server.URL + "/revoke",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		pub := signingKey(t).PublicKey
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/api/accounts/oauth/token", f.token)
	mux.HandleFunc("/revoke", f.revoke)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// authorize plays the authorization endpoint and the person: it checks what the
// authorization URL asks for and returns the query the browser would be sent
// back with.
func (f *fakeOAuth) authorize(authURL string) url.Values {
	f.t.Helper()
	parsed, err := url.Parse(authURL)
	if err != nil || !strings.HasPrefix(authURL, f.server.URL+"/api/accounts/authorize?") {
		f.t.Fatalf("the authorization URL is not the documented endpoint")
	}
	q := parsed.Query()
	for key, want := range map[string]string{
		"response_type": "code", "code_challenge_method": "S256", "resource": Resource, "scope": requestedScope,
	} {
		if q.Get(key) != want {
			f.t.Errorf("%s is %q, want %q", key, q.Get(key), want)
		}
	}
	if !strings.HasPrefix(q.Get("ext_agent_host_id"), "urn:uuid:") {
		f.t.Errorf("ext_agent_host_id is %q", q.Get("ext_agent_host_id"))
	}
	if !strings.HasPrefix(q.Get("redirect_uri"), "http://127.0.0.1:") || !strings.HasSuffix(q.Get("redirect_uri"), "/auth/callback") {
		f.t.Errorf("redirect_uri is %q", q.Get("redirect_uri"))
	}
	for _, key := range []string{"state", "nonce", "code_challenge"} {
		if q.Get(key) == "" {
			f.t.Errorf("%s is missing", key)
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.challenge, f.nonce, f.redirect = q.Get("code_challenge"), q.Get("nonce"), q.Get("redirect_uri")
	f.registering = q.Get("client_id") == RegistrationClientID

	back := url.Values{"code": {"code-1"}, "state": {q.Get("state")}, "scope": {f.scope}}
	if f.registering {
		if q.Get("agent_name_hint") != AgentName {
			f.t.Errorf("agent_name_hint is %q", q.Get("agent_name_hint"))
		}
		back.Set("client_id", issuedClient)
	} else {
		if q.Get("client_id") != issuedClient {
			f.t.Errorf("a later sign-in asked as %q, want the issued id", q.Get("client_id"))
		}
		if q.Has("agent_name_hint") {
			f.t.Errorf("agent_name_hint is sent after registration")
		}
	}
	return back
}

func (f *fakeOAuth) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenRequests = append(f.tokenRequests, r.PostForm)

	reply := func(status int, body map[string]any) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	if r.PostForm.Get("client_id") != issuedClient {
		reply(400, map[string]any{"error": "invalid_client"})
		return
	}
	if r.PostForm.Get("resource") != Resource {
		reply(400, map[string]any{"error": "invalid_target"})
		return
	}

	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if r.PostForm.Get("code") != "code-1" || base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge ||
			r.PostForm.Get("redirect_uri") != f.redirect {
			reply(400, map[string]any{"error": "invalid_grant"})
			return
		}
		f.refresh = canaryRefresh
		reply(200, map[string]any{
			"access_token": canaryAccess, "refresh_token": canaryRefresh, "token_type": "Bearer",
			"expires_in": f.expiresIn, "scope": f.scope, "id_token": f.idToken(),
		})

	case "refresh_token":
		f.refreshes++
		if f.refreshGate != nil {
			f.mu.Unlock()
			if f.refreshArrived != nil {
				f.refreshArrived <- struct{}{}
			}
			<-f.refreshGate
			f.mu.Lock()
		}
		if r.PostForm.Has("scope") {
			f.t.Errorf("a refresh sends scope")
		}
		if f.refreshError != "" {
			status := f.refreshStatus
			if status == 0 {
				status = 400
			}
			reply(status, map[string]any{"error": f.refreshError})
			return
		}
		if r.PostForm.Get("refresh_token") != f.refresh {
			reply(400, map[string]any{"error": "refresh_token_reused"})
			return
		}
		f.refresh = fmt.Sprintf("%s-%d", canaryRefresh, f.refreshes)
		reply(200, map[string]any{
			"access_token":  fmt.Sprintf("%s-%d", canaryAccess, f.refreshes),
			"refresh_token": f.refresh,
			"expires_in":    3600, "scope": f.scope, "token_type": "Bearer",
		})

	default:
		reply(400, map[string]any{"error": "unsupported_grant_type"})
	}
}

func (f *fakeOAuth) revoke(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokeRequests = append(f.revokeRequests, r.PostForm)
	if f.revokeFailed > 0 {
		f.revokeFailed--
		w.WriteHeader(503)
		return
	}
	w.WriteHeader(f.revokeStatus)
}

// idToken signs an ID token for the last authorization.
func (f *fakeOAuth) idToken() string {
	claims := map[string]any{
		"iss": f.server.URL, "aud": issuedClient, "sub": "user-subject-1", "email": "person@example.test",
		"nonce": f.nonce, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	}
	if f.idClaims != nil {
		f.idClaims(claims)
	}

	encode := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	signed := encode(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"}) + "." + encode(claims)

	key := signingKey(f.t)
	if f.signWithOther {
		key, _ = rsa.GenerateKey(rand.Reader, 2048)
	}
	sum := sha256.Sum256([]byte(signed))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		f.t.Fatal(err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func (f *fakeOAuth) tokenRequestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tokenRequests)
}

// newAuth is a sign-in against the fake, kept in a file of its own, with the
// listener on a free port and no real waiting.
func newAuth(t *testing.T, f *fakeOAuth) (*Auth, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	a := New(path, Endpoints{Auth: f.server.URL, API: f.server.URL + "/v1"})
	a.preferredPort = 0
	a.revokeBackoff = time.Millisecond
	t.Cleanup(a.closeFlow)
	return a, path
}

// callback sends the browser's redirect to the listener.
func callback(t *testing.T, redirect string, query url.Values) (int, string) {
	t.Helper()
	response, err := http.Get(redirect + "?" + query.Encode())
	if err != nil {
		t.Fatalf("the redirect could not be delivered: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(body)
}

// signedIn runs a whole sign-in through the loopback listener.
func signedIn(t *testing.T, f *fakeOAuth, a *Auth) {
	t.Helper()
	started, err := a.Start(t.Context())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if status, _ := callback(t, started.RedirectURI, f.authorize(started.AuthorizationURL)); status != 200 {
		t.Fatalf("the redirect was answered %d", status)
	}
	state, err := a.Flow(started.FlowID)
	if err != nil || state.Status != FlowSucceeded {
		t.Fatalf("the sign-in is %+v, %v", state, err)
	}
}

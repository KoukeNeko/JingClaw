//go:build ignore

// A stand-in for the two services Sign in with ChatGPT talks to: the
// authorization server and the Responses API. Built from the shapes OpenAI
// documents, not from the live services, which nothing here has contacted.
//
// Usage: fake-chatgpt PORT_FILE ACCESS_TOKEN REFRESH_TOKEN
//
// It listens on 127.0.0.1 on a free port, writes the port to PORT_FILE, and
// serves everything on that one address. GET /_state reports what it saw, for
// the script that drives it.
package main

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
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const issued = "oaiapp_e2e-fake-client"

var (
	mu     sync.Mutex
	key, _ = rsa.GenerateKey(rand.Reader, 2048)
	base   string

	access, refresh string

	// code -> what the authorization asked for
	pending = map[string]url.Values{}

	registrations, exchanges, refreshes, revocations, responses int
	lastResponseBody                                            string
	storeFalseStreamTrue                                        = true
	revokedToken                                                string
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func idToken(clientID, nonce string) string {
	enc := func(v any) string { raw, _ := json.Marshal(v); return b64(raw) }
	signed := enc(map[string]string{"alg": "RS256", "kid": "e2e", "typ": "JWT"}) + "." + enc(map[string]any{
		"iss": base, "aud": clientID, "sub": "e2e-subject", "email": "e2e@example.test",
		"nonce": nonce, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	})
	sum := sha256.Sum256([]byte(signed))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	return signed + "." + b64(sig)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	access, refresh = os.Args[2], os.Args[3]
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	base = "http://" + listener.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"issuer": base, "jwks_uri": base + "/jwks", "revocation_endpoint": base + "/revoke"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "e2e", "alg": "RS256", "use": "sig",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})

	// The person approving, in one request: check what is asked for and send
	// the browser back to the loopback address with a code.
	mux.HandleFunc("/api/accounts/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		redirect, err := url.Parse(q.Get("redirect_uri"))
		if err != nil || redirect.Hostname() != "127.0.0.1" || q.Get("code_challenge_method") != "S256" ||
			!strings.HasPrefix(q.Get("ext_agent_host_id"), "urn:uuid:") || q.Get("state") == "" || q.Get("nonce") == "" ||
			!strings.Contains(q.Get("scope"), "chatgpt.tokens.use.direct") {
			http.Error(w, "bad authorization request", 400)
			return
		}
		mu.Lock()
		code := fmt.Sprintf("code-%d", len(pending)+1)
		pending[code] = q
		mu.Unlock()

		back := url.Values{"code": {code}, "state": {q.Get("state")}, "scope": {q.Get("scope")}}
		if q.Get("client_id") == "dynamic_agent_client" {
			mu.Lock()
			registrations++
			mu.Unlock()
			back.Set("client_id", issued)
		}
		redirect.RawQuery = back.Encode()
		http.Redirect(w, r, redirect.String(), http.StatusFound)
	})

	mux.HandleFunc("/api/accounts/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		defer mu.Unlock()
		if r.PostForm.Get("client_id") != issued {
			writeJSON(w, 400, map[string]string{"error": "invalid_client"})
			return
		}
		switch r.PostForm.Get("grant_type") {
		case "authorization_code":
			asked, ok := pending[r.PostForm.Get("code")]
			sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if !ok || b64(sum[:]) != asked.Get("code_challenge") || r.PostForm.Get("redirect_uri") != asked.Get("redirect_uri") {
				writeJSON(w, 400, map[string]string{"error": "invalid_grant"})
				return
			}
			delete(pending, r.PostForm.Get("code"))
			exchanges++
			writeJSON(w, 200, map[string]any{
				"access_token": access, "refresh_token": refresh, "token_type": "Bearer", "expires_in": 3600,
				"scope": asked.Get("scope"), "id_token": idToken(issued, asked.Get("nonce")),
			})
		case "refresh_token":
			refreshes++
			writeJSON(w, 400, map[string]string{"error": "invalid_grant"})
		default:
			writeJSON(w, 400, map[string]string{"error": "unsupported_grant_type"})
		}
	})

	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		revocations++
		revokedToken = r.PostForm.Get("token")
		mu.Unlock()
		w.WriteHeader(200)
	})

	authed := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+access {
			writeJSON(w, 401, map[string]any{"error": map[string]string{"code": "subscription_sharing_invalid_user"}})
			return false
		}
		return true
	}
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		writeJSON(w, 200, map[string]any{"models": []map[string]string{
			{"slug": "fake-gpt", "display_name": "Fake GPT", "visibility": "list"},
			{"slug": "fake-hidden", "display_name": "Hidden", "visibility": "hide"},
		}})
	})
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var sent struct {
			Store  *bool `json:"store"`
			Stream *bool `json:"stream"`
			Input  []struct {
				Role    string `json:"role"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"input"`
		}
		_ = json.Unmarshal(raw, &sent)
		heard := ""
		for _, item := range sent.Input {
			if item.Role == "user" && len(item.Content) > 0 {
				// The last part: the daemon puts its own timestamp note first.
				heard = item.Content[len(item.Content)-1].Text
			}
		}
		mu.Lock()
		responses++
		lastResponseBody = string(raw)
		if sent.Store == nil || *sent.Store || sent.Stream == nil || !*sent.Stream {
			storeFalseStreamTrue = false
		}
		mu.Unlock()

		w.Header().Set("content-type", "text/event-stream")
		for _, delta := range []string{"fake chatgpt heard: ", heard} {
			ev, _ := json.Marshal(map[string]string{"type": "response.output_text.delta", "delta": delta})
			fmt.Fprintf(w, "event: response.output_text.delta\ndata: %s\n\n", ev)
		}
		fmt.Fprint(w, `event: response.completed`+"\n"+`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":7,"output_tokens":4}}}`+"\n\n")
	})

	mux.HandleFunc("/_state", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		writeJSON(w, 200, map[string]any{
			"registrations": registrations, "exchanges": exchanges, "refreshes": refreshes,
			"revocations": revocations, "revoked_token_is_refresh": revokedToken == refresh,
			"responses": responses, "store_false_stream_true": storeFalseStreamTrue,
			"last_response_body": lastResponseBody,
		})
	})

	if err := os.WriteFile(os.Args[1], []byte(base), 0o600); err != nil {
		panic(err)
	}
	_ = http.Serve(listener, mux)
}

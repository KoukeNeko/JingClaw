package chatgptauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	// ErrStateMismatch: the address answers some other sign-in. Nothing was
	// exchanged and the sign-in is still waiting.
	ErrStateMismatch = errors.New("chatgpt: that address does not belong to this sign-in")

	ErrUnknownFlow  = errors.New("chatgpt: there is no such sign-in")
	ErrFlowFinished = errors.New("chatgpt: that sign-in has already finished")
	ErrBadAddress   = errors.New("chatgpt: that is not an address the browser was redirected to")

	// The page tells these apart by the words in them, so they stay distinct.
	ErrAddressIncomplete = errors.New("chatgpt: that address has no authorization code or no state")
	ErrAddressTooLong    = errors.New("chatgpt: that address is too long to be the redirect")
	ErrDeclined          = errors.New("chatgpt: the sign-in was declined in ChatGPT (access_denied)")
)

// maxAddress is far more than a redirect with a code and a state ever needs.
const maxAddress = 8 << 10

const (
	callbackPath = "/auth/callback"

	// flowTTL is how long a person has to finish a sign-in before the listener
	// is closed. A consent screen can need a second factor.
	flowTTL = 10 * time.Minute
)

// FlowStatus is where a sign-in is.
type FlowStatus int

const (
	FlowPending FlowStatus = iota
	FlowSucceeded
	FlowFailed
)

// FlowState is a sign-in as the caller sees it.
type FlowState struct {
	Status FlowStatus

	// Error says why it failed. It never holds a code, a token or an address.
	Error string
}

// Started is a sign-in that is waiting for the browser.
type Started struct {
	FlowID string

	// AuthorizationURL is for the operator's own browser and nowhere else: it
	// carries this attempt's state, nonce and challenge.
	AuthorizationURL string

	// RedirectURI is where the browser will be sent back to.
	RedirectURI string
	ExpiresAt   time.Time
}

type flow struct {
	id, state, nonce, verifier, redirect string
	registering                          bool

	listener net.Listener
	server   *http.Server
	timer    *time.Timer

	// Guarded by flows.mu.
	status  FlowStatus
	failure string
	claimed bool
}

type flows struct {
	mu      sync.Mutex
	current *flow
}

func randomText(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", errors.New("chatgpt: could not generate random values")
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Start opens the loopback listener and returns the authorization URL.
//
// The listener is up before the URL is returned, so the redirect can never
// arrive at nothing, and the same redirect URI is used in both steps. The first
// sign-in registers this host by asking as dynamic_agent_client; the id it is
// issued is saved from the callback and every later sign-in uses that.
func (a *Auth) Start(ctx context.Context) (Started, error) {
	a.mu.Lock()
	if err := a.ensureLoaded(); err != nil {
		a.mu.Unlock()
		return Started{}, err
	}
	if a.rec.HostID == "" {
		id, err := newHostID()
		if err != nil {
			a.mu.Unlock()
			return Started{}, err
		}
		a.rec.HostID = id
		if err := save(a.path, a.rec); err != nil {
			a.mu.Unlock()
			return Started{}, err
		}
	}
	rec := a.rec
	a.mu.Unlock()

	a.closeFlow()

	id, err := randomText(12)
	if err != nil {
		return Started{}, err
	}
	state, err := randomText(32)
	if err != nil {
		return Started{}, err
	}
	nonce, err := randomText(32)
	if err != nil {
		return Started{}, err
	}
	verifier, err := randomText(32)
	if err != nil {
		return Started{}, err
	}

	listener, err := listenLoopback(a.preferredPort)
	if err != nil {
		return Started{}, err
	}
	redirect := "http://" + listener.Addr().String() + callbackPath

	f := &flow{
		id: id, state: state, nonce: nonce, verifier: verifier, redirect: redirect,
		registering: rec.ClientID == "",
		listener:    listener,
	}
	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) { a.receive(f, w, r) })
	f.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = f.server.Serve(listener) }()

	f.timer = time.AfterFunc(flowTTL, func() {
		a.flows.mu.Lock()
		if f.status == FlowPending && !f.claimed {
			f.status, f.failure = FlowFailed, "nobody finished signing in before it timed out"
		}
		a.flows.mu.Unlock()
		f.stop()
	})

	a.flows.mu.Lock()
	a.flows.current = f
	a.flows.mu.Unlock()

	challenge := sha256.Sum256([]byte(verifier))
	query := url.Values{
		"response_type":         {"code"},
		"client_id":             {rec.ClientID},
		"redirect_uri":          {redirect},
		"scope":                 {requestedScope},
		"resource":              {Resource},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
		"ext_agent_host_id":     {rec.HostID},
	}
	if f.registering {
		query.Set("client_id", RegistrationClientID)
		query.Set("agent_name_hint", AgentName)
	} else {
		if rec.IDToken != "" {
			query.Set("id_token_hint", rec.IDToken)
		}
		if rec.Email != "" {
			query.Set("login_hint", rec.Email)
		}
	}

	return Started{
		FlowID:           id,
		AuthorizationURL: a.ep.Auth + "/api/accounts/authorize?" + query.Encode(),
		RedirectURI:      redirect,
		ExpiresAt:        a.now().Add(flowTTL),
	}, nil
}

// listenLoopback prefers the documented port and takes any free one otherwise.
// 127.0.0.1 and not localhost: the redirect URI must name it, and nothing off
// this machine may reach it.
func listenLoopback(preferred int) (net.Listener, error) {
	if preferred > 0 {
		if listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(preferred))); err == nil {
			return listener, nil
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("chatgpt: could not open a local address to be redirected to: %w", err)
	}
	return listener, nil
}

func (f *flow) stop() {
	if f.timer != nil {
		f.timer.Stop()
	}
	shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if f.server.Shutdown(shutdown) != nil {
		_ = f.listener.Close()
	}
}

// closeFlow ends the sign-in in progress, if any.
func (a *Auth) closeFlow() {
	a.flows.mu.Lock()
	f := a.flows.current
	if f != nil && f.status == FlowPending && !f.claimed {
		f.status, f.failure = FlowFailed, "another sign-in replaced this one"
	}
	a.flows.mu.Unlock()
	if f != nil {
		f.stop()
	}
}

// Flow reports a sign-in started earlier.
func (a *Auth) Flow(id string) (FlowState, error) {
	a.flows.mu.Lock()
	defer a.flows.mu.Unlock()
	f := a.flows.current
	if f == nil || f.id != id {
		return FlowState{}, ErrUnknownFlow
	}
	return FlowState{Status: f.status, Error: f.failure}, nil
}

// Complete finishes a sign-in from the address the browser was sent to, for
// when that browser is not on this machine and its redirect to 127.0.0.1 went
// nowhere. The address is read and never logged or echoed.
func (a *Auth) Complete(ctx context.Context, id, address string) error {
	a.flows.mu.Lock()
	f := a.flows.current
	a.flows.mu.Unlock()
	if f == nil || f.id != id {
		return ErrUnknownFlow
	}

	if len(address) > maxAddress {
		return ErrAddressTooLong
	}
	query, err := queryOf(address)
	if err != nil {
		return err
	}
	return a.finish(ctx, f, query)
}

// queryOf takes the query out of a pasted address, or reads the text as a query
// when it is only that. Text that is no address at all is ErrBadAddress; an
// address without the parts a redirect carries is ErrAddressIncomplete.
func queryOf(address string) (url.Values, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return nil, ErrBadAddress
	}
	raw := address
	if strings.Contains(address, "?") {
		parsed, err := url.Parse(address)
		if err != nil {
			return nil, ErrBadAddress
		}
		raw = parsed.RawQuery
	} else if !strings.Contains(address, "=") {
		return nil, ErrBadAddress
	}
	query, err := url.ParseQuery(raw)
	if err != nil {
		return nil, ErrBadAddress
	}
	if (query.Get("code") == "" && query.Get("error") == "") || query.Get("state") == "" {
		return nil, ErrAddressIncomplete
	}
	return query, nil
}

// receive is the browser's redirect arriving.
func (a *Auth) receive(f *flow, w http.ResponseWriter, r *http.Request) {
	err := a.finish(r.Context(), f, r.URL.Query())

	w.Header().Set("content-type", "text/html; charset=utf-8")
	switch {
	case errors.Is(err, ErrStateMismatch), errors.Is(err, ErrFlowFinished):
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, page("Not expected", "Nothing here is waiting for this."))
	case err != nil:
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, page("Not signed in", "The sign-in did not finish. JingClaw has the reason."))
	default:
		fmt.Fprint(w, page("Signed in", "You can close this tab and go back to JingClaw."))
	}
}

func page(title, said string) string {
	return "<!doctype html><meta charset=utf-8><title>" + html.EscapeString(title) +
		"</title><body style=\"font:16px system-ui;margin:4rem auto;max-width:32rem\">" +
		"<h1>" + html.EscapeString(title) + "</h1><p>" + html.EscapeString(said) + "</p>"
}

// finish checks the state and, only if it matches, does the rest. A request
// with the wrong state exchanges nothing and does not end the sign-in, so a
// stray request to the listener cannot cancel it.
func (a *Auth) finish(ctx context.Context, f *flow, query url.Values) error {
	if subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(f.state)) != 1 {
		return ErrStateMismatch
	}

	a.flows.mu.Lock()
	if f.status != FlowPending || f.claimed {
		a.flows.mu.Unlock()
		return ErrFlowFinished
	}
	f.claimed = true
	a.flows.mu.Unlock()

	err := a.exchange(ctx, f, query)

	a.flows.mu.Lock()
	if err != nil {
		f.status, f.failure = FlowFailed, err.Error()
	} else {
		f.status = FlowSucceeded
	}
	a.flows.mu.Unlock()

	// After the reply, so the browser is not cut off from its own answer.
	go f.stop()
	return err
}

// exchange is everything after the state has matched.
func (a *Auth) exchange(ctx context.Context, f *flow, query url.Values) error {
	// access_denied and the rest: stop, and do not exchange a code.
	if declined := query.Get("error"); declined != "" {
		if declined == "access_denied" {
			return ErrDeclined
		}
		return fmt.Errorf("chatgpt: the sign-in was not completed (%s)", cleanCode(declined))
	}
	code := query.Get("code")
	if code == "" {
		return errors.New("chatgpt: the redirect carried no authorization code")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	issued := query.Get("client_id")
	if f.registering {
		if issued == "" || issued == RegistrationClientID {
			return errors.New("chatgpt: the registration did not return a client id")
		}
		a.rec.ClientID = issued
		if err := save(a.path, a.rec); err != nil {
			return err
		}
	} else if issued != "" && issued != a.rec.ClientID {
		return errors.New("chatgpt: the redirect names a different client than this sign-in was for")
	}

	status, reply, err := a.postForm(ctx, a.ep.Auth+"/api/accounts/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {a.rec.ClientID},
		"code":          {code},
		"code_verifier": {f.verifier},
		"redirect_uri":  {f.redirect},
		"resource":      {Resource},
	})
	if err != nil {
		return fmt.Errorf("chatgpt: could not reach the sign-in service: %w", err)
	}
	if status != http.StatusOK || reply.AccessToken == "" {
		if reply.Error == "invalid_grant" {
			return errors.New("chatgpt: the authorization code was not accepted; start the sign-in again")
		}
		return fmt.Errorf("chatgpt: the sign-in service refused the code: status %d %s", status, cleanCode(reply.Error))
	}
	if reply.IDToken == "" {
		return errors.New("chatgpt: the sign-in returned no ID token")
	}

	identity, err := a.verifyIDToken(ctx, reply.IDToken, a.rec.ClientID, f.nonce)
	if err != nil {
		return fmt.Errorf("chatgpt: %w", err)
	}
	// The registration belongs to the account that made it; a different account
	// would be signed in under that account's client id.
	if a.rec.Subject != "" && identity.Subject != a.rec.Subject {
		return errors.New("chatgpt: this is a different ChatGPT account than the one JingClaw was signed in with; " +
			"nothing was changed. Sign in with the original account, or remove " + FileName + " in the JingClaw home to start over")
	}

	// The scopes the token response grants; the callback's are the fallback if
	// the response has none.
	if reply.Scope == "" {
		reply.Scope = query.Get("scope")
	}
	a.rec.Scopes = nil
	a.applyTokens(reply)
	a.rec.IDToken = reply.IDToken
	a.rec.Subject, a.rec.Email, a.rec.Name = identity.Subject, identity.Email, identity.Name

	// Stored as signed in either way; without the plan-usage scope inference is
	// refused with ErrPlanUsageNotGranted rather than sent.
	a.rec.LastErrorCode, a.rec.LastErrorMessage = "", ""
	if !a.rec.planUsage() {
		a.rec.LastErrorCode = "plan_usage_not_granted"
		a.rec.LastErrorMessage = "the sign-in did not grant " + ScopePlanUsage + ", so ChatGPT plan usage is off"
	}
	return save(a.path, a.rec)
}

// cleanCode keeps a server's error code to what a code looks like, since it is
// repeated into messages.
func cleanCode(code string) string {
	if len(code) > 64 {
		code = code[:64]
	}
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
			return r
		}
		return '?'
	}, code)
}

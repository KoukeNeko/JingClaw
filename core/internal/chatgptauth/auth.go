package chatgptauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	// ErrNotSignedIn: there is nothing to serve a request with.
	ErrNotSignedIn = errors.New("chatgpt: not signed in; sign in with ChatGPT from the dashboard")

	// ErrPlanUsageNotGranted: the account is signed in, but did not grant the
	// scope that allows inference on its plan. Nothing falls back to another
	// way of paying.
	ErrPlanUsageNotGranted = errors.New("chatgpt: this sign-in did not grant " + ScopePlanUsage +
		", so ChatGPT plan usage is off; sign in again and allow plan usage")

	// ErrSignInAgain: the refresh token is no longer usable, and the stored
	// tokens have been cleared. The registration is kept, so the next sign-in
	// reuses it.
	ErrSignInAgain = errors.New("chatgpt: the sign-in has expired; sign in with ChatGPT again")
)

// refreshSkew is how close to expiry an access token is replaced.
const refreshSkew = 5 * time.Minute

// Auth is the one signed-in account.
//
// The record is read once and kept, and every change is written before it is
// used. Refresh tokens rotate, so refreshes are serialised as the documentation
// asks: by refreshMu in this process and by a lock file beside the record
// across processes, each reloading the file under the lock first.
type Auth struct {
	path string
	ep   Endpoints
	http *http.Client
	now  func() time.Time

	// refreshMu serialises refreshes and sign-out, which hold it across network
	// calls; mu only guards rec and is never held across one, so Account() and
	// the dashboard poll are not blocked by them.
	refreshMu sync.Mutex
	mu        sync.Mutex
	rec       record
	loaded    bool

	// startedUsable is whether a request could have been served when this
	// process began, which is what a running provider was built from.
	startedUsable bool

	// revokeBackoff is the first wait between attempts to revoke at sign-out.
	revokeBackoff time.Duration

	// preferredPort is tried first for the loopback listener. 1455 is the
	// documented example; any free port is allowed when it is taken.
	preferredPort int

	flows flows
}

// New is the sign-in kept in path, talking to ep.
func New(path string, ep Endpoints) *Auth {
	a := &Auth{
		path:          path,
		ep:            ep,
		http:          &http.Client{Timeout: 30 * time.Second},
		now:           time.Now,
		revokeBackoff: time.Second,
		preferredPort: 1455,
	}
	if err := a.ensureLoaded(); err == nil {
		a.startedUsable = a.rec.signedIn() && a.rec.planUsage()
	}
	return a
}

// ensureLoaded reads the file the first time it is needed. Called with mu held,
// or before the value is shared.
func (a *Auth) ensureLoaded() error {
	if a.loaded {
		return nil
	}
	rec, err := load(a.path)
	if err != nil {
		return err
	}
	a.rec, a.loaded = rec, true
	return nil
}

// Account is what may be shown about the sign-in. It has no token, no client
// id and no host id.
type Account struct {
	SignedIn         bool
	Label            string
	Email            string
	Scopes           []string
	PlanUsageGranted bool
	ExpiresAt        time.Time
	LastErrorCode    string
	LastErrorMessage string
}

// Account describes the sign-in.
func (a *Auth) Account() (Account, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ensureLoaded(); err != nil {
		return Account{}, err
	}

	rec := a.rec
	out := Account{
		SignedIn:         rec.signedIn(),
		Scopes:           append([]string(nil), rec.Scopes...),
		PlanUsageGranted: rec.signedIn() && rec.planUsage(),
		LastErrorCode:    rec.LastErrorCode,
		LastErrorMessage: rec.LastErrorMessage,
	}
	if out.SignedIn {
		out.Email = rec.Email
		out.Label = rec.Email
		if out.Label == "" {
			out.Label = rec.Name
		}
		if out.Label == "" {
			out.Label = "ChatGPT account"
		}
		out.ExpiresAt = rec.ExpiresAt
	}
	return out, nil
}

// StartedUsable is whether the sign-in could serve a request when this process
// began.
func (a *Auth) StartedUsable() bool { return a.startedUsable }

// Usable is whether it can now.
func (a *Auth) Usable() bool {
	account, err := a.Account()
	return err == nil && account.SignedIn && account.PlanUsageGranted
}

// RecordError remembers the last thing that went wrong with ChatGPT, for the
// dashboard to show. An empty code clears it. The message is cut short: it is
// the service's own words and is only ever shown.
func (a *Auth) RecordError(code, message string) {
	// Written under the file lock from the current file, so a stale copy of the
	// tokens never replaces what another process rotated. The lock is taken
	// before mu, which a refresh's network wait must not make Account() wait on.
	if unlock, err := lockFile(a.path + ".lock"); err == nil {
		defer unlock()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ensureLoaded() != nil {
		return
	}
	if rec, err := load(a.path); err == nil {
		a.rec = rec
	}
	if len(message) > 300 {
		message = message[:300]
	}
	if a.rec.LastErrorCode == code && a.rec.LastErrorMessage == message {
		return
	}
	a.rec.LastErrorCode, a.rec.LastErrorMessage = code, message
	_ = save(a.path, a.rec)
}

// AccessToken is a bearer token good for a request, replaced first if it is
// close to expiring. If the replacement fails for a reason that does not end
// the sign-in and the current token has not yet expired, the current one is
// returned: the failed refresh costs nothing yet.
func (a *Auth) AccessToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	err := a.usableLocked()
	token, expiring := a.rec.AccessToken, a.expiring(a.rec)
	a.mu.Unlock()
	if err != nil {
		return "", err
	}
	if !expiring {
		return token, nil
	}

	refreshErr := a.refresh(ctx, a.expiring)
	a.mu.Lock()
	defer a.mu.Unlock()
	if refreshErr != nil {
		if !errors.Is(refreshErr, ErrSignInAgain) && a.rec.AccessToken != "" && a.now().Before(a.rec.ExpiresAt) {
			return a.rec.AccessToken, nil
		}
		return "", refreshErr
	}
	if err := a.usableLocked(); err != nil {
		return "", err
	}
	return a.rec.AccessToken, nil
}

// usableLocked is nil when there are tokens and the plan-usage scope. mu is held.
func (a *Auth) usableLocked() error {
	if err := a.ensureLoaded(); err != nil {
		return err
	}
	if !a.rec.signedIn() {
		return ErrNotSignedIn
	}
	if !a.rec.planUsage() {
		return ErrPlanUsageNotGranted
	}
	return nil
}

func (a *Auth) expiring(r record) bool {
	return r.AccessToken == "" || a.now().Add(refreshSkew).After(r.ExpiresAt)
}

// Refresh replaces an access token the service refused, and returns the one to
// use instead. If another caller or process already replaced it, that one is
// returned and nothing is sent: two requests refused at once cost one refresh.
func (a *Auth) Refresh(ctx context.Context, rejected string) (string, error) {
	a.mu.Lock()
	err := a.ensureLoaded()
	if err == nil && !a.rec.signedIn() {
		err = ErrNotSignedIn
	}
	a.mu.Unlock()
	if err != nil {
		return "", err
	}
	if err := a.refresh(ctx, func(r record) bool { return r.AccessToken == "" || r.AccessToken == rejected }); err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.rec.signedIn() {
		return "", ErrNotSignedIn
	}
	return a.rec.AccessToken, nil
}

// tokenResponse is the token endpoint's answer.
//
// The documentation lists these fields for a successful exchange and says
// nothing of the error body; the standard OAuth shape (error, error_description)
// is assumed for it. earliest_refresh_at is documented as a field but its format
// is not, so it is not used: refresh is by expiry.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`

	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// terminalRefreshErrors are the codes the documentation lists for a refresh
// token that cannot be used again.
var terminalRefreshErrors = map[string]bool{
	"invalid_grant": true, "invalid_refresh_token": true, "token_expired": true,
	"refresh_token_expired": true, "refresh_token_invalidated": true, "refresh_token_reused": true,
}

// refresh exchanges the refresh token if needed still says the record wants it.
// It is serialised in this process by refreshMu and across processes by a lock
// file: refresh tokens rotate, so a second refresher must first read what the
// first stored, and must never clear tokens a newer refresh has replaced.
func (a *Auth) refresh(ctx context.Context, needed func(record) bool) error {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	unlock, err := lockFile(a.path + ".lock")
	if err != nil {
		return fmt.Errorf("chatgpt: %w", err)
	}
	defer unlock()

	a.mu.Lock()
	if rec, err := load(a.path); err == nil {
		a.rec = rec
	}
	if !a.rec.signedIn() || !needed(a.rec) {
		a.mu.Unlock()
		return nil
	}
	refreshToken, clientID := a.rec.RefreshToken, a.rec.ClientID
	if refreshToken == "" {
		a.clearTokensLocked("refresh_token_missing", "the sign-in has no refresh token")
		a.mu.Unlock()
		return ErrSignInAgain
	}
	a.mu.Unlock()

	status, reply, err := a.postForm(ctx, a.ep.Auth+"/api/accounts/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
		"resource":      {Resource},
	})
	if err != nil {
		return fmt.Errorf("chatgpt: could not reach the sign-in service to refresh: %w", err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if status == http.StatusOK && reply.AccessToken != "" {
		a.applyTokens(reply)
		a.rec.LastErrorCode, a.rec.LastErrorMessage = "", ""
		// A replacement refresh token is on disk before the new access token
		// is used. If the write fails the new tokens are still held, so this
		// process keeps working; the error says the next start will not.
		if err := save(a.path, a.rec); err != nil {
			return fmt.Errorf("chatgpt: refreshed, but could not store the new tokens: %w", err)
		}
		return nil
	}

	if terminalRefreshErrors[reply.Error] {
		// A token other than the one just refused is on disk (a sign-in or
		// another writer got there first): it is not ours to clear.
		if rec, err := load(a.path); err == nil && rec.RefreshToken != refreshToken {
			a.rec = rec
			return nil
		}
		a.clearTokensLocked(reply.Error, "ChatGPT no longer accepts this sign-in")
		return fmt.Errorf("%w (%s)", ErrSignInAgain, reply.Error)
	}
	// invalid_client and everything else: the tokens are kept, because nothing
	// says they are bad, and what is wrong is not something signing in fixes.
	return fmt.Errorf("chatgpt: the sign-in service refused the refresh: status %d %s", status, cleanCode(reply.Error))
}

// applyTokens takes a successful token response into the record.
func (a *Auth) applyTokens(reply tokenResponse) {
	a.rec.AccessToken = reply.AccessToken
	if reply.RefreshToken != "" {
		a.rec.RefreshToken = reply.RefreshToken
	}
	lifetime := time.Duration(reply.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = time.Hour
	}
	a.rec.ExpiresAt = a.now().Add(lifetime)
	if reply.Scope != "" {
		a.rec.Scopes = strings.Fields(reply.Scope)
	}
	// A refresh may carry an ID token; it is not validated here, so it does not
	// replace the one that was.
}

// clearTokensLocked forgets the tokens and keeps the registration.
func (a *Auth) clearTokensLocked(code, message string) {
	a.rec.AccessToken, a.rec.RefreshToken, a.rec.IDToken = "", "", ""
	a.rec.ExpiresAt, a.rec.Scopes = time.Time{}, nil
	a.rec.LastErrorCode, a.rec.LastErrorMessage = code, message
	_ = save(a.path, a.rec)
}

// postForm sends a form and reads the answer as a token response. A body that
// is not JSON is an empty response with the status.
func (a *Auth) postForm(ctx context.Context, target string, form url.Values) (int, tokenResponse, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, tokenResponse{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")

	response, err := a.http.Do(request)
	if err != nil {
		return 0, tokenResponse{}, unwrapURLError(err)
	}
	defer func() { _ = response.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return 0, tokenResponse{}, err
	}
	var reply tokenResponse
	_ = json.Unmarshal(raw, &reply)
	return response.StatusCode, reply, nil
}

// unwrapURLError drops the request URL from a transport error: an authorization
// URL is long and carries values that should not be repeated into messages.
func unwrapURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

// discovery is the part of the OpenID configuration that is used.
type discovery struct {
	JWKSURI            string `json:"jwks_uri"`
	RevocationEndpoint string `json:"revocation_endpoint"`
}

func (a *Auth) discover(ctx context.Context) (discovery, error) {
	var found discovery
	err := a.getJSON(ctx, a.ep.Auth+"/.well-known/openid-configuration", &found)
	return found, err
}

func (a *Auth) getJSON(ctx context.Context, target string, into any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	response, err := a.http.Do(request)
	if err != nil {
		return unwrapURLError(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", response.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(into)
}

// SignOutResult says how a sign-out went.
type SignOutResult struct {
	// Revoked is whether the service confirmed the refresh token is revoked.
	// The local tokens are cleared either way.
	Revoked bool

	// Detail is what to tell the person when Revoked is false.
	Detail string
}

// SignOut revokes the refresh token and forgets the tokens.
//
// Revocation is tried with a short backoff while the token is still held, and
// the tokens are cleared whether or not it was confirmed: a person who asked to
// sign out must not stay signed in because a service was down. When it could
// not be confirmed the result says so, and that the app can be disconnected in
// ChatGPT Settings. The registration and the host id are kept.
func (a *Auth) SignOut(ctx context.Context) (SignOutResult, error) {
	a.closeFlow()

	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	unlock, err := lockFile(a.path + ".lock")
	if err != nil {
		return SignOutResult{}, fmt.Errorf("chatgpt: %w", err)
	}
	defer unlock()

	a.mu.Lock()
	if err := a.ensureLoaded(); err != nil {
		a.mu.Unlock()
		return SignOutResult{}, err
	}
	if rec, err := load(a.path); err == nil {
		a.rec = rec
	}
	if !a.rec.signedIn() {
		a.mu.Unlock()
		return SignOutResult{Revoked: true}, nil
	}
	refreshToken, clientID := a.rec.RefreshToken, a.rec.ClientID
	a.mu.Unlock()

	// The revocation retries run with mu released.
	result := SignOutResult{}
	switch {
	case refreshToken == "":
		result.Detail = "There was no refresh token to revoke."
	default:
		result = a.revokeWith(ctx, refreshToken, clientID)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.rec.AccessToken, a.rec.RefreshToken, a.rec.IDToken = "", "", ""
	a.rec.ExpiresAt, a.rec.Scopes = time.Time{}, nil
	a.rec.LastErrorCode, a.rec.LastErrorMessage = "", ""
	if err := save(a.path, a.rec); err != nil {
		return result, err
	}
	return result, nil
}

func (a *Auth) revokeWith(ctx context.Context, refreshToken, clientID string) SignOutResult {
	const attempts = 3
	unconfirmed := SignOutResult{Detail: "ChatGPT did not confirm the revocation, so the access it granted may still be active. " +
		"The tokens on this machine are gone; you can disconnect JingClaw in ChatGPT Settings."}

	found, err := a.discover(ctx)
	if err != nil || found.RevocationEndpoint == "" {
		return unconfirmed
	}

	wait := a.revokeBackoff
	for attempt := 1; attempt <= attempts; attempt++ {
		status, err := a.revoke(ctx, found.RevocationEndpoint, refreshToken, clientID)
		switch {
		case err == nil && status == http.StatusOK:
			// An empty 200 is done, including for a token already invalid.
			return SignOutResult{Revoked: true}
		case err == nil && status < 500:
			// Refused for a reason retrying will not change.
			return unconfirmed
		}
		if attempt == attempts {
			break
		}
		select {
		case <-ctx.Done():
			return unconfirmed
		case <-time.After(wait):
		}
		wait *= 2
	}
	return unconfirmed
}

func (a *Auth) revoke(ctx context.Context, endpoint, refreshToken, clientID string) (int, error) {
	status, _, err := a.postForm(ctx, endpoint, url.Values{
		"token":           {refreshToken},
		"token_type_hint": {"refresh_token"},
		"client_id":       {clientID},
	})
	return status, err
}

// APIBase is where the models and responses are served.
func (a *Auth) APIBase() string { return a.ep.API }

package chatgptauth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTheFirstSignInRegistersThroughTheAuthorizationAndLaterOnesReuseTheIssuedClientId(t *testing.T) {
	oauth := newFakeOAuth(t)
	auth, path := newAuth(t, oauth)

	started, err := auth.Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(started.AuthorizationURL, "client_id=dynamic_agent_client") {
		t.Fatalf("the first authorization is not made as the registration client")
	}
	hostID := auth.rec.HostID

	if status, _ := callback(t, started.RedirectURI, oauth.authorize(started.AuthorizationURL)); status != 200 {
		t.Fatalf("the redirect was answered %d", status)
	}
	if state, _ := auth.Flow(started.FlowID); state.Status != FlowSucceeded {
		t.Fatalf("the sign-in is %+v", state)
	}

	// The token exchange used the issued id, never the registration one.
	for _, form := range oauth.tokenRequests {
		if form.Get("client_id") != issuedClient {
			t.Errorf("a token request was made as %q", form.Get("client_id"))
		}
	}

	account, _ := auth.Account()
	if !account.SignedIn || !account.PlanUsageGranted || account.Email != "person@example.test" {
		t.Errorf("the account is %+v", account)
	}

	// Signed out and in again: the same registration and host, no new consent
	// as a stranger.
	if _, err := auth.SignOut(t.Context()); err != nil {
		t.Fatal(err)
	}
	reload := New(path, Endpoints{Auth: oauth.server.URL})
	if reload.rec.ClientID != issuedClient || reload.rec.HostID != hostID {
		t.Errorf("the registration did not survive a sign-out")
	}
	reload.preferredPort = 0
	again, err := reload.Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reload.closeFlow)
	parsed, _ := url.Parse(again.AuthorizationURL)
	if parsed.Query().Get("client_id") != issuedClient || parsed.Query().Get("ext_agent_host_id") != hostID {
		t.Errorf("a later sign-in does not reuse the registration")
	}
	if parsed.Query().Has("agent_name_hint") {
		t.Errorf("a later sign-in registers again")
	}
}

func TestAStateThatDoesNotMatchIsRefusedWithoutExchangingACodeAndTheSignInKeepsWaiting(t *testing.T) {
	oauth := newFakeOAuth(t)
	auth, _ := newAuth(t, oauth)

	started, _ := auth.Start(t.Context())
	back := oauth.authorize(started.AuthorizationURL)

	forged := url.Values{"code": back["code"], "state": {"someone-elses"}, "client_id": back["client_id"]}
	if status, _ := callback(t, started.RedirectURI, forged); status != 400 {
		t.Errorf("the forged redirect was answered %d", status)
	}
	err := auth.Complete(t.Context(), started.FlowID, started.RedirectURI+"?"+forged.Encode())
	if !errors.Is(err, ErrStateMismatch) {
		t.Errorf("a pasted address with another state gave %v", err)
	}

	if oauth.tokenRequestCount() != 0 {
		t.Errorf("a code was exchanged for a redirect whose state did not match")
	}
	if state, _ := auth.Flow(started.FlowID); state.Status != FlowPending {
		t.Errorf("a stray request ended the sign-in: %+v", state)
	}

	// And the real one still works.
	if status, _ := callback(t, started.RedirectURI, back); status != 200 {
		t.Errorf("the real redirect was answered %d", status)
	}
}

func TestADeclinedAuthorizationEndsTheSignInWithoutExchangingACode(t *testing.T) {
	oauth := newFakeOAuth(t)
	auth, _ := newAuth(t, oauth)

	started, _ := auth.Start(t.Context())
	back := oauth.authorize(started.AuthorizationURL)
	denied := url.Values{"error": {"access_denied"}, "state": back["state"]}
	if status, _ := callback(t, started.RedirectURI, denied); status != 400 {
		t.Errorf("answered %d", status)
	}

	if oauth.tokenRequestCount() != 0 {
		t.Errorf("a code was exchanged after the person said no")
	}
	state, _ := auth.Flow(started.FlowID)
	if state.Status != FlowFailed || !strings.Contains(state.Error, "access_denied") {
		t.Errorf("the sign-in is %+v", state)
	}
	if account, _ := auth.Account(); account.SignedIn {
		t.Errorf("signed in after being declined")
	}
}

func TestAnIDTokenThatIsNotForThisSignInIsRefusedAndNothingIsStored(t *testing.T) {
	for name, break_ := range map[string]func(*fakeOAuth){
		"another nonce":    func(f *fakeOAuth) { f.idClaims = func(c map[string]any) { c["nonce"] = "other" } },
		"another audience": func(f *fakeOAuth) { f.idClaims = func(c map[string]any) { c["aud"] = "oaiapp_someone-else" } },
		"another issuer":   func(f *fakeOAuth) { f.idClaims = func(c map[string]any) { c["iss"] = "https://evil.example" } },
		"expired": func(f *fakeOAuth) {
			f.idClaims = func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }
		},
		"no subject": func(f *fakeOAuth) { f.idClaims = func(c map[string]any) { c["sub"] = "" } },
		"another signing key": func(f *fakeOAuth) {
			f.signWithOther = true
		},
	} {
		t.Run(name, func(t *testing.T) {
			oauth := newFakeOAuth(t)
			break_(oauth)
			auth, path := newAuth(t, oauth)

			started, _ := auth.Start(t.Context())
			callback(t, started.RedirectURI, oauth.authorize(started.AuthorizationURL))

			state, _ := auth.Flow(started.FlowID)
			if state.Status != FlowFailed {
				t.Fatalf("the sign-in is %+v", state)
			}
			if account, _ := auth.Account(); account.SignedIn {
				t.Errorf("signed in on a token that should have been refused")
			}
			raw, _ := os.ReadFile(path)
			if strings.Contains(string(raw), canaryAccess) || strings.Contains(string(raw), canaryRefresh) {
				t.Errorf("tokens were stored for a refused sign-in")
			}
			if strings.Contains(state.Error, canaryAccess) || strings.Contains(state.Error, "code-1") {
				t.Errorf("the failure repeats a secret: %q", state.Error)
			}
		})
	}
}

func TestWithoutThePlanUsageScopeTheAccountIsSignedInWithPlanUsageOffAndInferenceIsRefused(t *testing.T) {
	oauth := newFakeOAuth(t)
	oauth.scope = "openid profile email offline_access"
	auth, _ := newAuth(t, oauth)
	signedIn(t, oauth, auth)

	account, _ := auth.Account()
	if !account.SignedIn || account.PlanUsageGranted || account.LastErrorCode != "plan_usage_not_granted" {
		t.Errorf("the account is %+v", account)
	}
	if _, err := auth.AccessToken(t.Context()); !errors.Is(err, ErrPlanUsageNotGranted) {
		t.Errorf("a token was handed out without the scope: %v", err)
	}
	if auth.Usable() {
		t.Errorf("an account without plan usage is usable")
	}
}

func TestARefreshRotatesTheRefreshTokenAndTheLatestOneIsTheOneStored(t *testing.T) {
	oauth := newFakeOAuth(t)
	oauth.expiresIn = 30 // inside the refresh skew, so the next use refreshes
	auth, path := newAuth(t, oauth)
	signedIn(t, oauth, auth)

	first, err := auth.AccessToken(t.Context())
	if err != nil || first != canaryAccess+"-1" {
		t.Fatalf("first use gave %q, %v", first, err)
	}
	// The replacement is on disk before it is used again.
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), canaryRefresh+"-1") || strings.Contains(string(raw), `"`+canaryRefresh+`"`) {
		t.Errorf("the file does not hold the latest refresh token")
	}

	// A later refresh presents the replacement, not the retired token.
	rejected := first
	second, err := auth.Refresh(t.Context(), rejected)
	if err != nil || second != canaryAccess+"-2" {
		t.Errorf("second refresh gave %q, %v", second, err)
	}
}

func TestTwoCallersNeedingATokenAtOnceCauseOneRefresh(t *testing.T) {
	oauth := newFakeOAuth(t)
	oauth.expiresIn = 30
	auth, _ := newAuth(t, oauth)
	signedIn(t, oauth, auth)

	var (
		wait    sync.WaitGroup
		tokens  = make([]string, 2)
		failure = make([]error, 2)
	)
	for i := range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			tokens[i], failure[i] = auth.AccessToken(t.Context())
		}()
	}
	wait.Wait()

	for i := range 2 {
		if failure[i] != nil {
			t.Fatalf("caller %d: %v", i, failure[i])
		}
	}
	if tokens[0] != tokens[1] {
		t.Errorf("the callers were given different tokens")
	}
	if oauth.refreshes != 1 {
		t.Errorf("%d refreshes were sent, want 1", oauth.refreshes)
	}
}

func TestATokenTheServiceRefusedIsRefreshedOnceAndNotAgainWhenSomeoneElseAlreadyDid(t *testing.T) {
	oauth := newFakeOAuth(t)
	auth, _ := newAuth(t, oauth)
	signedIn(t, oauth, auth)

	one, err := auth.Refresh(t.Context(), canaryAccess)
	if err != nil || one == canaryAccess {
		t.Fatalf("refresh gave %q, %v", one, err)
	}
	// A second request that was refused with the same old token.
	two, err := auth.Refresh(t.Context(), canaryAccess)
	if err != nil || two != one {
		t.Errorf("the second refused request got %q, %v, want the token already issued", two, err)
	}
	if oauth.refreshes != 1 {
		t.Errorf("%d refreshes were sent, want 1", oauth.refreshes)
	}
}

func TestATerminalRefreshErrorClearsTheTokensKeepsTheRegistrationAndAsksToSignInAgain(t *testing.T) {
	for _, code := range []string{
		"invalid_grant", "invalid_refresh_token", "token_expired",
		"refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused",
	} {
		t.Run(code, func(t *testing.T) {
			oauth := newFakeOAuth(t)
			oauth.expiresIn = 30
			auth, path := newAuth(t, oauth)
			signedIn(t, oauth, auth)
			oauth.refreshError = code

			_, err := auth.AccessToken(t.Context())
			if !errors.Is(err, ErrSignInAgain) {
				t.Fatalf("got %v", err)
			}
			account, _ := auth.Account()
			if account.SignedIn || account.LastErrorCode != code {
				t.Errorf("the account is %+v", account)
			}
			raw, _ := os.ReadFile(path)
			if strings.Contains(string(raw), canaryRefresh) || !strings.Contains(string(raw), issuedClient) {
				t.Errorf("the file kept a token or lost the registration")
			}
		})
	}
}

func TestARefreshThatFailsForAnyOtherReasonKeepsTheSignIn(t *testing.T) {
	for name, set := range map[string]func(*fakeOAuth){
		"invalid_client":     func(f *fakeOAuth) { f.refreshError = "invalid_client"; f.refreshStatus = 401 },
		"a server error":     func(f *fakeOAuth) { f.refreshError = "server_error"; f.refreshStatus = 503 },
		"an unreachable one": func(f *fakeOAuth) { f.server.Close() },
	} {
		t.Run(name, func(t *testing.T) {
			oauth := newFakeOAuth(t)
			oauth.expiresIn = 30
			auth, _ := newAuth(t, oauth)
			signedIn(t, oauth, auth)
			set(oauth)
			// Past expiry: a token still valid is returned instead of failing.
			auth.now = func() time.Time { return time.Now().Add(time.Minute) }

			_, err := auth.AccessToken(t.Context())
			if err == nil || errors.Is(err, ErrSignInAgain) {
				t.Fatalf("got %v", err)
			}
			if account, _ := auth.Account(); !account.SignedIn {
				t.Errorf("a refresh that did not say the sign-in was over signed the person out")
			}
		})
	}
}

func TestSignOutRevokesTheRefreshTokenThenClearsTheTokensAndKeepsTheRegistration(t *testing.T) {
	oauth := newFakeOAuth(t)
	auth, path := newAuth(t, oauth)
	signedIn(t, oauth, auth)

	result, err := auth.SignOut(t.Context())
	if err != nil || !result.Revoked {
		t.Fatalf("sign-out gave %+v, %v", result, err)
	}
	form := oauth.revokeRequests[0]
	if form.Get("token") != canaryRefresh || form.Get("token_type_hint") != "refresh_token" || form.Get("client_id") != issuedClient {
		t.Errorf("the revocation request is %v", form)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), canaryRefresh) || strings.Contains(string(raw), canaryAccess) || !strings.Contains(string(raw), issuedClient) {
		t.Errorf("the file after sign-out is wrong")
	}
	if _, err := auth.AccessToken(t.Context()); !errors.Is(err, ErrNotSignedIn) {
		t.Errorf("signed out and still handed a token: %v", err)
	}
}

func TestSignOutRetriesAServerErrorAndClearsTheTokensEvenWhenRevocationIsNeverConfirmed(t *testing.T) {
	oauth := newFakeOAuth(t)
	auth, _ := newAuth(t, oauth)
	signedIn(t, oauth, auth)

	oauth.revokeFailed = 1
	result, err := auth.SignOut(t.Context())
	if err != nil || !result.Revoked || len(oauth.revokeRequests) != 2 {
		t.Errorf("a transient failure was not retried to success: %+v, %v, %d requests", result, err, len(oauth.revokeRequests))
	}

	signedIn(t, oauth, auth)
	oauth.revokeFailed = 100
	result, err = auth.SignOut(t.Context())
	if err != nil || result.Revoked || !strings.Contains(result.Detail, "ChatGPT Settings") {
		t.Errorf("unconfirmed revocation gave %+v, %v", result, err)
	}
	if account, _ := auth.Account(); account.SignedIn {
		t.Errorf("still signed in after an unconfirmed revocation")
	}
}

func TestTheStoredFileIsOwnerOnlyWrittenWholeAndNothingIsLeftBesideIt(t *testing.T) {
	oauth := newFakeOAuth(t)
	auth, path := newAuth(t, oauth)
	signedIn(t, oauth, auth)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the file is mode %o", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, entry := range entries {
		if entry.Name() != FileName {
			t.Errorf("a save left %s beside the file", entry.Name())
		}
	}

	// A file others can read is refused rather than used.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	loose := New(path, Endpoints{Auth: oauth.server.URL})
	if _, err := loose.AccessToken(t.Context()); err == nil || strings.Contains(err.Error(), canaryAccess) {
		t.Errorf("a world-readable file gave %v", err)
	}
}

func TestNoTokenOrIdentifierReachesAnErrorAnAccountOrAFormattedValue(t *testing.T) {
	oauth := newFakeOAuth(t)
	oauth.expiresIn = 30
	auth, _ := newAuth(t, oauth)
	signedIn(t, oauth, auth)

	account, _ := auth.Account()
	seen := []string{fmt.Sprintf("%+v %v", account, account)}

	oauth.refreshError = "refresh_token_reused"
	_, err := auth.AccessToken(t.Context())
	seen = append(seen, err.Error())
	_, err = auth.Refresh(t.Context(), "x")
	if err != nil {
		seen = append(seen, err.Error())
	}
	flowState, _ := auth.Flow("nope")
	seen = append(seen, fmt.Sprint(flowState))

	hostID := auth.rec.HostID
	for _, text := range seen {
		for _, secret := range []string{canaryAccess, canaryRefresh, issuedClient, hostID, "code-1", "eyJ"} {
			if secret != "" && strings.Contains(text, secret) {
				t.Errorf("%q leaked into %q", secret, text)
			}
		}
	}
}

func TestAPastedAddressCompletesTheSignInAndABadOneIsRefused(t *testing.T) {
	oauth := newFakeOAuth(t)
	auth, _ := newAuth(t, oauth)

	started, _ := auth.Start(t.Context())
	back := oauth.authorize(started.AuthorizationURL)

	for _, bad := range []string{"", "not an address", "http://127.0.0.1:1455/auth/callback", "http://[::1"} {
		if err := auth.Complete(t.Context(), started.FlowID, bad); !errors.Is(err, ErrBadAddress) {
			t.Errorf("%q gave %v", bad, err)
		}
	}
	if err := auth.Complete(t.Context(), "other-flow", "x"); !errors.Is(err, ErrUnknownFlow) {
		t.Errorf("an unknown flow gave %v", err)
	}

	if err := auth.Complete(t.Context(), started.FlowID, "http://127.0.0.1:1455/auth/callback?"+back.Encode()); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if account, _ := auth.Account(); !account.SignedIn {
		t.Errorf("not signed in")
	}
	if err := auth.Complete(t.Context(), started.FlowID, "?"+back.Encode()); !errors.Is(err, ErrFlowFinished) {
		t.Errorf("a code used twice gave %v", err)
	}
}

func TestTheEnvironmentNamesATestDoubleOnlyOnLoopbackHTTP(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string
	}{
		{"http://127.0.0.1:9999", "http://127.0.0.1:9999"},
		{"http://localhost:9999/", "http://localhost:9999"},
		{"https://127.0.0.1:9999", DefaultAuthBase},
		{"http://evil.example", DefaultAuthBase},
		{"http://127.0.0.1.evil.example", DefaultAuthBase},
		{"http://user@127.0.0.1:9999", DefaultAuthBase},
		{"", DefaultAuthBase},
	} {
		t.Setenv(AuthURLEnv, tc.value)
		if got := EndpointsFromEnv().Auth; got != tc.want {
			t.Errorf("%q gave %q, want %q", tc.value, got, tc.want)
		}
	}
	t.Setenv(APIURLEnv, "http://127.0.0.1:1/v1")
	if got := EndpointsFromEnv().API; got != "http://127.0.0.1:1/v1" {
		t.Errorf("the API variable gave %q", got)
	}
}

func TestTheAccountOfNobodyIsSignedOutAndAFileMadeByAnotherRunIsRead(t *testing.T) {
	oauth := newFakeOAuth(t)
	auth, path := newAuth(t, oauth)
	if account, err := auth.Account(); err != nil || account.SignedIn {
		t.Fatalf("a fresh deployment is %+v, %v", account, err)
	}
	if _, err := auth.AccessToken(context.Background()); !errors.Is(err, ErrNotSignedIn) {
		t.Errorf("got %v", err)
	}

	signedIn(t, oauth, auth)
	next := New(path, Endpoints{Auth: oauth.server.URL})
	if !next.StartedUsable() {
		t.Errorf("a sign-in made by the previous run is not usable at the next start")
	}
}

func TestAFailedRefreshReturnsTheCurrentTokenWhileItIsStillValid(t *testing.T) {
	oauth := newFakeOAuth(t)
	oauth.expiresIn = 30 // inside the refresh skew, but not yet expired
	auth, _ := newAuth(t, oauth)
	signedIn(t, oauth, auth)
	oauth.refreshError, oauth.refreshStatus = "server_error", 503

	if token, err := auth.AccessToken(t.Context()); err != nil || token == "" {
		t.Fatalf("got %q, %v", token, err)
	}
}

func TestARefreshAnotherProcessAlreadyDidIsAdoptedNotRepeated(t *testing.T) {
	oauth := newFakeOAuth(t)
	oauth.expiresIn = 30
	a, path := newAuth(t, oauth)
	signedIn(t, oauth, a)

	// A second process refreshed and rotated while this one held the old token.
	other := New(path, Endpoints{Auth: oauth.server.URL})
	other.now = func() time.Time { return time.Now().Add(time.Minute) }
	if _, err := other.AccessToken(t.Context()); err != nil {
		t.Fatal(err)
	}
	rotated, _ := load(path)

	a.now = func() time.Time { return time.Now().Add(time.Minute) }
	if _, err := a.AccessToken(t.Context()); err != nil {
		t.Fatalf("got %v", err)
	}
	if oauth.refreshes != 1 {
		t.Errorf("%d refreshes were sent, want 1: the retired token was presented", oauth.refreshes)
	}
	if after, _ := load(path); after.RefreshToken != rotated.RefreshToken || after.RefreshToken == "" {
		t.Errorf("the other process's tokens were lost")
	}
}

func TestATerminalRefreshErrorNeverClearsADifferentTokenOnDisk(t *testing.T) {
	oauth := newFakeOAuth(t)
	oauth.expiresIn = 30
	auth, path := newAuth(t, oauth)
	signedIn(t, oauth, auth)
	oauth.refreshError = "refresh_token_reused"
	oauth.refreshGate, oauth.refreshArrived = make(chan struct{}), make(chan struct{}, 1)
	auth.now = func() time.Time { return time.Now().Add(time.Minute) }

	done := make(chan error, 1)
	go func() { _, err := auth.AccessToken(t.Context()); done <- err }()
	<-oauth.refreshArrived
	// A new sign-in lands on disk while the refresh is in flight.
	rec, _ := load(path)
	rec.RefreshToken = "someone-elses-newer-token"
	if err := save(path, rec); err != nil {
		t.Fatal(err)
	}
	close(oauth.refreshGate)
	<-done

	if after, _ := load(path); after.RefreshToken != "someone-elses-newer-token" {
		t.Errorf("tokens a newer sign-in stored were cleared")
	}
}

func TestAccountIsNotBlockedWhileARefreshWaitsOnTheNetwork(t *testing.T) {
	oauth := newFakeOAuth(t)
	oauth.expiresIn = 30
	auth, _ := newAuth(t, oauth)
	signedIn(t, oauth, auth)
	release := make(chan struct{})
	oauth.refreshGate, oauth.refreshArrived = release, make(chan struct{}, 1)
	done := make(chan struct{})
	go func() { _, _ = auth.AccessToken(t.Context()); close(done) }()

	<-oauth.refreshArrived
	got := make(chan struct{})
	go func() { _, _ = auth.Account(); close(got) }()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Error("Account() waited for the refresh")
	}
	close(release)
	<-done
}

func TestSigningInAgainAsAnotherAccountIsRefusedAndReplacesNothing(t *testing.T) {
	oauth := newFakeOAuth(t)
	auth, path := newAuth(t, oauth)
	signedIn(t, oauth, auth)
	before, _ := os.ReadFile(path)

	oauth.idClaims = func(c map[string]any) { c["sub"] = "another-account" }
	started, err := auth.Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	callback(t, started.RedirectURI, oauth.authorize(started.AuthorizationURL))

	state, _ := auth.Flow(started.FlowID)
	if state.Status != FlowFailed || !strings.Contains(state.Error, "different ChatGPT account") {
		t.Errorf("the sign-in is %+v", state)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Errorf("the stored sign-in changed")
	}
}

func TestAPastedAddressIsRefusedForADistinctReason(t *testing.T) {
	oauth := newFakeOAuth(t)
	auth, _ := newAuth(t, oauth)
	started, _ := auth.Start(t.Context())
	back := oauth.authorize(started.AuthorizationURL)
	state := back.Get("state")

	for name, tc := range map[string]struct {
		address string
		want    error
	}{
		"not an address": {"hello there", ErrBadAddress},
		"no code":        {"http://127.0.0.1/auth/callback?state=" + state, ErrAddressIncomplete},
		"no state":       {"http://127.0.0.1/auth/callback?code=c", ErrAddressIncomplete},
		"another state":  {"http://127.0.0.1/auth/callback?code=c&state=other", ErrStateMismatch},
		"too long":       {"http://127.0.0.1/auth/callback?code=" + strings.Repeat("a", maxAddress), ErrAddressTooLong},
	} {
		if err := auth.Complete(t.Context(), started.FlowID, tc.address); !errors.Is(err, tc.want) {
			t.Errorf("%s gave %v, want %v", name, err, tc.want)
		}
	}
	if oauth.tokenRequestCount() != 0 {
		t.Errorf("a refused address exchanged a code")
	}

	declined := "http://127.0.0.1/auth/callback?error=access_denied&state=" + state
	if err := auth.Complete(t.Context(), started.FlowID, declined); !errors.Is(err, ErrDeclined) {
		t.Errorf("a declined sign-in gave %v", err)
	}
}

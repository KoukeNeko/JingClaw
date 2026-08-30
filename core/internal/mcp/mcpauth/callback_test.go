package mcpauth

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"net/http"
	"strings"
	"testing"
	"time"
)

// listening is a callback that does not open anything, so a test run does not
// launch a browser on whoever's machine is running it.
func listening(t *testing.T) (*Callback, string) {
	t.Helper()

	callback, err := Listen()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = callback.Close() })

	// No Announce: this is a test, nothing should be printed, and a shared
	// variable written from two sign-ins at once is a race in the harness
	// rather than in what it is checking.
	return callback, callback.RedirectURL()
}

func TestTheRedirectIsReachableOnlyFromThisMachine(t *testing.T) {
	_, redirect := listening(t)

	// The literal address, not a name. localhost resolves through whatever
	// this machine's resolver says, and what has to be true is that nothing
	// off it can reach this port.
	if !strings.HasPrefix(redirect, "http://127.0.0.1:") {
		t.Fatalf("the redirect is not bound to loopback: %s", redirect)
	}
	if strings.HasSuffix(redirect, ":0"+callbackPath) {
		t.Fatalf("the port was never assigned: %s", redirect)
	}
}

func TestACodeComesBackWithWhatProvesItIsTheRightOne(t *testing.T) {
	callback, redirect := listening(t)

	answered := make(chan struct{})
	go func() {
		defer close(answered)
		//nolint:bodyclose // closed below, after the select
		response, err := http.Get(redirect +
			"?code=the-code&state=the-state&iss=https://as.example")
		if err != nil {
			t.Errorf("the browser could not reach the redirect: %v", err)
			return
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Errorf("somebody signing in was shown %d", response.StatusCode)
		}
	}()

	result, err := callback.Fetch(context.Background(), authorizationArgs("https://as.example/authorize"))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	<-answered

	if result.Code != "the-code" {
		t.Errorf("code: got %q", result.Code)
	}
	// State is what ties this response to the request that started it, and
	// iss is what stops one authorization server's response being replayed as
	// another's. Both are checked by the SDK and both are useless if this
	// does not carry them out of the query.
	if result.State != "the-state" {
		t.Errorf("state: got %q", result.State)
	}
	if result.Iss != "https://as.example" {
		t.Errorf("iss: got %q", result.Iss)
	}
}

func TestARefusalSaysWhatTheServerSaid(t *testing.T) {
	callback, redirect := listening(t)

	// Deliberately without a state. The spec says a server must echo it even
	// on an error; a server that does not must still be able to say no,
	// rather than leaving somebody waiting out the consent timeout for a
	// consent already declined.
	go func() {
		for range 200 {
			callback.mu.Lock()
			waiting := len(callback.waiting)
			callback.mu.Unlock()
			if waiting > 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		response, err := http.Get(redirect +
			"?error=access_denied&error_description=The+user+declined")
		if err == nil {
			_ = response.Body.Close()
		}
	}()

	_, err := callback.Fetch(context.Background(), authorizationArgs("https://as.example/authorize"))
	if err == nil {
		t.Fatal("a refusal came back as a success")
	}
	// The server's own words, not a sentence written here. What a person needs
	// in order to fix a refused consent is the reason the server gave.
	if !strings.Contains(err.Error(), "The user declined") {
		t.Errorf("the reason was thrown away: %v", err)
	}
}

func TestARedirectWithNoCodeIsNotAnAuthorization(t *testing.T) {
	callback, redirect := listening(t)

	go func() {
		response, err := http.Get(redirect + "?state=the-state")
		if err == nil {
			_ = response.Body.Close()
		}
	}()

	if _, err := callback.Fetch(context.Background(), authorizationArgs("https://as.example/x")); err == nil {
		t.Fatal("a redirect carrying nothing was accepted as an authorization")
	}
}

func TestNobodySigningInEndsInsteadOfWaitingForever(t *testing.T) {
	callback, _ := listening(t)

	// The caller's context, which is what a person pressing ctrl-c reaches.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() {
		_, err := callback.Fetch(ctx, authorizationArgs("https://as.example/x"))
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled sign-in came back as a success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling did not end the wait")
	}
}

func TestSomethingElseOnTheMachineIsNotAnAuthorization(t *testing.T) {
	callback, redirect := listening(t)

	root := strings.TrimSuffix(redirect, callbackPath) + "/favicon.ico"
	response, err := http.Get(root)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusNotFound {
		t.Errorf("a request to %s was answered %d", root, response.StatusCode)
	}

	// And it did not settle any flow, which is the part that matters: a
	// browser probing for a favicon must not be able to end somebody's
	// sign-in.
	callback.mu.Lock()
	waiting := len(callback.waiting)
	callback.mu.Unlock()
	if waiting != 0 {
		t.Fatalf("a request for something else left %d flows changed", waiting)
	}
}

// TestTwoSignInsAtOnceEachGetTheirOwnAnswer is the case a single channel got
// wrong.
//
// The transport authorizes whatever gets refused, and its request and its
// event stream can both be refused at once. With one channel, whichever
// redirect arrived first satisfied whichever call happened to be listening,
// and the state it carried then belonged to the other one — which the caller
// correctly rejected as a state mismatch, failing a sign-in that had actually
// succeeded.
func TestTwoSignInsAtOnceEachGetTheirOwnAnswer(t *testing.T) {
	callback, redirect := listening(t)

	type outcome struct {
		result *auth.AuthorizationResult
		err    error
	}
	first := make(chan outcome, 1)
	second := make(chan outcome, 1)

	go func() {
		got, err := callback.Fetch(context.Background(),
			&auth.AuthorizationArgs{URL: "https://as.example/authorize?state=first"})
		first <- outcome{got, err}
	}()
	go func() {
		got, err := callback.Fetch(context.Background(),
			&auth.AuthorizationArgs{URL: "https://as.example/authorize?state=second"})
		second <- outcome{got, err}
	}()

	// Both registered before either is answered, so this is genuinely two at
	// once rather than two in sequence.
	waitFor(t, callback, 2)

	// Answered out of order on purpose: the one that finishes first is not
	// necessarily the one that started first.
	for _, answer := range []struct{ state, code string }{
		{"second", "code-for-second"},
		{"first", "code-for-first"},
	} {
		response, err := http.Get(redirect + "?code=" + answer.code + "&state=" + answer.state)
		if err != nil {
			t.Fatalf("redirect for %s: %v", answer.state, err)
		}
		_ = response.Body.Close()
	}

	got := <-first
	if got.err != nil {
		t.Fatalf("the first sign-in failed: %v", got.err)
	}
	if got.result.Code != "code-for-first" || got.result.State != "first" {
		t.Errorf("the first sign-in got the other one's answer: %+v", got.result)
	}

	got = <-second
	if got.err != nil {
		t.Fatalf("the second sign-in failed: %v", got.err)
	}
	if got.result.Code != "code-for-second" || got.result.State != "second" {
		t.Errorf("the second sign-in got the other one's answer: %+v", got.result)
	}
}

// TestARedirectNobodyIsWaitingForChangesNothing covers a reloaded page, or
// one arriving after the flow it belonged to has ended.
func TestARedirectNobodyIsWaitingForChangesNothing(t *testing.T) {
	callback, redirect := listening(t)

	answered := make(chan struct{})
	go func() {
		defer close(answered)
		_, err := callback.Fetch(context.Background(),
			&auth.AuthorizationArgs{URL: "https://as.example/authorize?state=mine"})
		if err != nil {
			t.Errorf("the waiting sign-in failed: %v", err)
		}
	}()
	waitFor(t, callback, 1)

	// Somebody else's, or an old one.
	stray, err := http.Get(redirect + "?code=stray&state=not-mine")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = stray.Body.Close()
	if stray.StatusCode == http.StatusOK {
		t.Error("a redirect nobody was waiting for was reported as a success")
	}

	mine, err := http.Get(redirect + "?code=mine&state=mine")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = mine.Body.Close()
	<-answered
}

// waitFor blocks until the given number of sign-ins are registered.
func waitFor(t *testing.T, callback *Callback, want int) {
	t.Helper()

	for range 200 {
		callback.mu.Lock()
		got := len(callback.waiting)
		callback.mu.Unlock()
		if got >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("only %d sign-ins registered, wanted %d", len(callback.waiting), want)
}

// authorizationArgs is an authorization URL carrying a state, which is what
// ties a redirect to the call that is waiting for it.
func authorizationArgs(url string) *auth.AuthorizationArgs {
	if !strings.Contains(url, "state=") {
		url += "?state=the-state"
	}
	return &auth.AuthorizationArgs{URL: url}
}

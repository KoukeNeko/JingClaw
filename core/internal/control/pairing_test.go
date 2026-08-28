package control_test

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/control"
)

func newPairing(t *testing.T, ttl time.Duration) (*control.Pairing, control.Token, *time.Time) {
	t.Helper()

	token, err := control.NewToken(control.ScopeConsole)
	if err != nil {
		t.Fatalf("token: %v", err)
	}

	clock := time.Unix(1_700_000_000, 0).UTC()
	return control.NewPairing(token, ttl, func() time.Time { return clock }), token, &clock
}

// The credential is the thing worth having; the code is what ends up in a
// terminal's scrollback. So the code is the part that stops working.
func TestACodeWorksOnce(t *testing.T) {
	pairing, token, _ := newPairing(t, time.Minute)

	code, _, err := pairing.Issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	redeemed, err := pairing.Redeem(code)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if redeemed.Value != token.Value {
		t.Error("the wrong credential came back")
	}

	// A code that still works after it has been used is a code that is still
	// worth stealing out of a screenshot an hour later.
	if _, err := pairing.Redeem(code); !errors.Is(err, control.ErrNoSuchCode) {
		t.Errorf("the code worked twice: %v", err)
	}
}

func TestACodeExpires(t *testing.T) {
	pairing, _, clock := newPairing(t, time.Minute)

	code, expires, err := pairing.Issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	*clock = expires.Add(time.Second)

	if _, err := pairing.Redeem(code); !errors.Is(err, control.ErrNoSuchCode) {
		t.Errorf("an expired code was accepted: %v", err)
	}
}

// Somebody reading a code off a screen types it the way it looks, and a code
// that only works in one letter case is an obstacle rather than a control.
func TestACodeIsAcceptedTheWayAPersonTypesIt(t *testing.T) {
	pairing, _, _ := newPairing(t, time.Minute)

	code, _, err := pairing.Issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	if !strings.Contains(code, "-") {
		t.Errorf("the code is not grouped for reading: %q", code)
	}

	for _, typed := range []string{
		strings.ToLower(code),
		strings.ReplaceAll(code, "-", ""),
		"  " + code + "  ",
	} {
		fresh, _, err := pairing.Issue()
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		rewritten := rewrite(typed, code, fresh)

		if _, err := pairing.Redeem(rewritten); err != nil {
			t.Errorf("%q was refused: %v", rewritten, err)
		}
	}
}

// rewrite applies the same mangling the test did to one code, to another.
func rewrite(mangled, from, to string) string {
	switch {
	case mangled == strings.ToLower(from):
		return strings.ToLower(to)
	case mangled == strings.ReplaceAll(from, "-", ""):
		return strings.ReplaceAll(to, "-", "")
	default:
		return "  " + to + "  "
	}
}

func TestACodeNobodyIssuedIsRefused(t *testing.T) {
	pairing, _, _ := newPairing(t, time.Minute)

	if _, err := pairing.Redeem("AAAA-BBBB-CCCC-DDDD"); !errors.Is(err, control.ErrNoSuchCode) {
		t.Errorf("an invented code was accepted: %v", err)
	}
}

// Two codes can be outstanding: running "agent console" twice should not
// silently invalidate the line somebody is already looking at.
func TestSeveralCodesCanBeOutstanding(t *testing.T) {
	pairing, _, _ := newPairing(t, time.Minute)

	first, _, err := pairing.Issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	second, _, err := pairing.Issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	if _, err := pairing.Redeem(second); err != nil {
		t.Fatalf("the newer code was refused: %v", err)
	}
	if _, err := pairing.Redeem(first); err != nil {
		t.Errorf("issuing a second code invalidated the first: %v", err)
	}
}

// Repeated minting must not grow without bound; nobody needs a hundred
// unredeemed codes, and an unauthenticated caller must not be able to decide
// how much memory this uses.
func TestOutstandingCodesAreBounded(t *testing.T) {
	pairing, _, clock := newPairing(t, time.Hour)

	var codes []string
	for range 40 {
		code, _, err := pairing.Issue()
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		codes = append(codes, code)

		// Time moves, so "the oldest" means something. Frozen, every code
		// expires at the same instant and which one goes first is arbitrary.
		*clock = clock.Add(time.Second)
	}

	// The most recent still work; the oldest have been dropped.
	if _, err := pairing.Redeem(codes[len(codes)-1]); err != nil {
		t.Errorf("the newest code was dropped: %v", err)
	}
	if _, err := pairing.Redeem(codes[0]); !errors.Is(err, control.ErrNoSuchCode) {
		t.Error("codes accumulate without bound")
	}
}

func redeemOverHTTP(t *testing.T, pairing *control.Pairing, body string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost, control.RedeemPath, bytes.NewBufferString(body))
	recorder := httptest.NewRecorder()
	pairing.RedeemHandler().ServeHTTP(recorder, request)

	return recorder
}

func TestTheExchangeHandsBackACredential(t *testing.T) {
	pairing, token, _ := newPairing(t, time.Minute)

	code, _, err := pairing.Issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	recorder := redeemOverHTTP(t, pairing, `{"code":"`+code+`"}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status is %d: %s", recorder.Code, recorder.Body)
	}
	if !strings.Contains(recorder.Body.String(), token.Value) {
		t.Error("the credential did not come back")
	}
	// It is a credential. It must not sit in a proxy or a browser cache.
	if store := recorder.Header().Get("Cache-Control"); store != "no-store" {
		t.Errorf("Cache-Control is %q, want no-store", store)
	}
}

// Every way a code can fail gets one answer, so a caller cannot learn whether
// it was close.
func TestTheExchangeSaysNothingUseful(t *testing.T) {
	pairing, _, _ := newPairing(t, time.Minute)

	code, _, err := pairing.Issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := pairing.Redeem(code); err != nil {
		t.Fatalf("redeem: %v", err)
	}

	used := redeemOverHTTP(t, pairing, `{"code":"`+code+`"}`)
	invented := redeemOverHTTP(t, pairing, `{"code":"AAAA-BBBB-CCCC-DDDD"}`)

	if used.Code != http.StatusForbidden || invented.Code != http.StatusForbidden {
		t.Fatalf("statuses are %d and %d, want 403 for both", used.Code, invented.Code)
	}
	if used.Body.String() != invented.Body.String() {
		t.Errorf("a used code and an invented one are answered differently:\n%q\n%q",
			used.Body, invented.Body)
	}
}

// An unauthenticated caller must not be able to decide how much this reads.
func TestTheExchangeRefusesAnEnormousBody(t *testing.T) {
	pairing, _, _ := newPairing(t, time.Minute)

	recorder := redeemOverHTTP(t, pairing,
		`{"code":"`+strings.Repeat("A", 1<<20)+`"}`)

	if recorder.Code == http.StatusOK {
		t.Error("a megabyte of code was accepted")
	}
}

func TestTheExchangeIsPostOnly(t *testing.T) {
	pairing, _, _ := newPairing(t, time.Minute)

	request := httptest.NewRequest(http.MethodGet, control.RedeemPath, nil)
	recorder := httptest.NewRecorder()
	pairing.RedeemHandler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Errorf("a GET returned %d", recorder.Code)
	}
}

// The console's credential must not be able to mint more of itself, or being
// let in once would be being let in permanently.
func TestAConsoleCredentialCannotIssueCodes(t *testing.T) {
	consoleToken, err := control.NewToken(control.ScopeConsole)
	if err != nil {
		t.Fatalf("token: %v", err)
	}

	guarded := control.AuthMiddleware([]control.Token{consoleToken}, "7777",
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	for path, want := range map[string]int{
		"/jingclaw.control.v1.ConsoleService/IssuePairingCode": http.StatusForbidden,
		"/jingclaw.control.v1.ChannelService/ListBindings":     http.StatusForbidden,
		"/jingclaw.control.v1.SessionService/ListSessions":     http.StatusOK,
		"/jingclaw.control.v1.ArtifactService/StatArtifact":    http.StatusOK,
	} {
		request := httptest.NewRequest(http.MethodPost, path, nil)
		request.Host = "127.0.0.1:7777"
		request.Header.Set("Authorization", "Bearer "+consoleToken.Value)

		recorder := httptest.NewRecorder()
		guarded.ServeHTTP(recorder, request)

		if recorder.Code != want {
			t.Errorf("%s returned %d, want %d", path, recorder.Code, want)
		}
	}
}

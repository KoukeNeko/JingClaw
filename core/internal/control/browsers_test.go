package control_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/control"
)

// clock is a time that moves when a test says so.
type clock struct{ at time.Time }

func (c *clock) now() time.Time { return c.at }

func newBrowsers() (*control.Browsers, *clock) {
	c := &clock{at: time.Unix(1_700_000_000, 0)}
	return control.NewBrowsers(c.now), c
}

// A code that could be used twice is a credential that outlives the moment it
// was handed over. Whoever sees the address afterwards — in a history, a
// shared screen, a log — must find it already spent.
func TestACodeIsSpentByTheFirstBrowserToPresentIt(t *testing.T) {
	browsers, _ := newBrowsers()

	code, _, err := browsers.Pair()
	if err != nil {
		t.Fatalf("pair: %v", err)
	}

	if _, _, err := browsers.Redeem(code); err != nil {
		t.Fatalf("the first use was refused: %v", err)
	}
	if _, _, err := browsers.Redeem(code); !errors.Is(err, control.ErrNotPaired) {
		t.Errorf("the second use got %v", err)
	}
}

func TestACodeThatRanOutIsRefused(t *testing.T) {
	browsers, now := newBrowsers()

	code, expires, err := browsers.Pair()
	if err != nil {
		t.Fatal(err)
	}

	now.at = expires // not before: the moment it runs out is already too late
	if _, _, err := browsers.Redeem(code); !errors.Is(err, control.ErrNotPaired) {
		t.Errorf("got %v", err)
	}
}

// Whoever is guessing learns nothing from how it fails.
func TestACodeThatWasNeverIssuedFailsLikeASpentOne(t *testing.T) {
	browsers, _ := newBrowsers()

	code, _, _ := browsers.Pair()
	_, _, _ = browsers.Redeem(code)

	_, _, spent := browsers.Redeem(code)
	_, _, unknown := browsers.Redeem("never-issued")

	if spent == nil || unknown == nil || spent.Error() != unknown.Error() {
		t.Errorf("spent: %v, unknown: %v", spent, unknown)
	}
}

// A session is the operator's own credential's reach, for a while, and then it
// is not.
func TestASessionIsAcceptedUntilItRunsOut(t *testing.T) {
	browsers, now := newBrowsers()

	code, _, _ := browsers.Pair()
	token, expires, err := browsers.Redeem(code)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}

	if scope, ok := browsers.Lookup(token.Value); !ok || scope != control.ScopeControl {
		t.Errorf("a live session got %q %v", scope, ok)
	}
	if _, ok := browsers.Lookup(token.Value + "x"); ok {
		t.Error("a different value was accepted")
	}
	if _, ok := browsers.Lookup(code); ok {
		t.Error("the pairing code is not a session")
	}

	now.at = expires
	if _, ok := browsers.Lookup(token.Value); ok {
		t.Error("a session that ran out was accepted")
	}
}

// Pairing is rare and by hand. Hitting the limit means something is looping.
func TestPairingIsBounded(t *testing.T) {
	browsers, now := newBrowsers()

	var failed bool
	for range 100 {
		if _, _, err := browsers.Pair(); err != nil {
			failed = true
			break
		}
	}
	if !failed {
		t.Fatal("a hundred codes were issued at once")
	}

	// Codes expire, and so the limit is not a lockout.
	now.at = now.at.Add(2 * time.Minute)
	if _, _, err := browsers.Pair(); err != nil {
		t.Errorf("still refused after they ran out: %v", err)
	}
}

// ── the exchange over HTTP ─────────────────────────────────────────────

func post(handler http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/ui/pair", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestAPairedBrowserGetsASessionThatTheControlSurfaceAccepts(t *testing.T) {
	browsers, _ := newBrowsers()
	pair := control.PairHandler(browsers)

	code, _, _ := browsers.Pair()
	rec := post(pair, `{"code":"`+code+`"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("a response that carries a credential may be cached")
	}

	var granted struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &granted); err != nil || granted.Token == "" || granted.ExpiresAt == "" {
		t.Fatalf("response: %s (%v)", rec.Body, err)
	}

	const port = "49231"
	guarded := control.AuthMiddleware(nil, browsers, port,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	call := func(token, path string) int {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:"+port+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		return rec.Code
	}

	for _, path := range []string{
		"/jingclaw.control.v1.SessionService/DecideApproval",
		"/jingclaw.control.v1.ConfigService/WriteConfig",
	} {
		if got := call(granted.Token, path); got != http.StatusOK {
			t.Errorf("%s: a paired browser got %d", path, got)
		}
	}
	if got := call(granted.Token, "/jingclaw.control.v1.FutureService/DoThing"); got != http.StatusForbidden {
		t.Errorf("a service nobody gave it: %d", got)
	}
	if got := call("not-a-session", "/jingclaw.control.v1.SessionService/ListSessions"); got != http.StatusUnauthorized {
		t.Errorf("a made-up credential: %d", got)
	}

	// The same code does nothing the second time.
	if rec := post(pair, `{"code":"`+code+`"}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("a spent code got %d", rec.Code)
	}
}

func TestThePairingEndpointRefusesWhatIsNotAPairing(t *testing.T) {
	browsers, _ := newBrowsers()
	pair := control.PairHandler(browsers)

	cases := map[string]struct {
		do   func() *httptest.ResponseRecorder
		want int
	}{
		"a GET": {func() *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			pair.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/pair", nil))
			return rec
		}, http.StatusMethodNotAllowed},
		"no body":       {func() *httptest.ResponseRecorder { return post(pair, "") }, http.StatusBadRequest},
		"not JSON":      {func() *httptest.ResponseRecorder { return post(pair, "code=abc") }, http.StatusBadRequest},
		"an empty code": {func() *httptest.ResponseRecorder { return post(pair, `{"code":""}`) }, http.StatusBadRequest},
		"a wrong code":  {func() *httptest.ResponseRecorder { return post(pair, `{"code":"nope"}`) }, http.StatusUnauthorized},
	}
	for name, tc := range cases {
		if got := tc.do().Code; got != tc.want {
			t.Errorf("%s: got %d, want %d", name, got, tc.want)
		}
	}
}

// ── asking for a code ──────────────────────────────────────────────────

func TestTheOperatorCanAskForACodeAndAGatewayCannot(t *testing.T) {
	browsers, _ := newBrowsers()

	mux := http.NewServeMux()
	mux.Handle(controlv1connect.NewBrowserServiceHandler(control.NewBrowserServer(browsers)))

	const (
		operator = "operator-token"
		gateway  = "gateway-token"
	)
	server := httptest.NewServer(control.AuthMiddleware(
		[]control.Token{{Value: operator, Scope: control.ScopeControl}, {Value: gateway, Scope: control.ScopeGateway}},
		nil, "", mux))
	t.Cleanup(server.Close)

	asking := func(token string) (*controlv1.CreatePairingResponse, error) {
		client := controlv1connect.NewBrowserServiceClient(server.Client(), server.URL,
			connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
				return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
					req.Header().Set("Authorization", "Bearer "+token)
					return next(ctx, req)
				}
			})))
		resp, err := client.CreatePairing(context.Background(), connect.NewRequest(&controlv1.CreatePairingRequest{}))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}

	got, err := asking(operator)
	if err != nil || got.GetCode() == "" || got.GetExpiresAt() == nil {
		t.Fatalf("the operator got %v, %v", got, err)
	}
	if _, _, err := browsers.Redeem(got.GetCode()); err != nil {
		t.Errorf("the code that was handed out does not work: %v", err)
	}

	// A compromised gateway must not be able to let a browser in.
	if _, err := asking(gateway); err == nil {
		t.Error("the gateway credential asked for a pairing code")
	}
}

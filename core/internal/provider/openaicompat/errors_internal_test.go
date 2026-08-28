package openaicompat

import (
	"net/http"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

func kindOf(t *testing.T, profileName string, status int, body string) provider.ErrorKind {
	t.Helper()

	profile, ok := ProfileByName(profileName)
	if !ok {
		t.Fatalf("no profile %q", profileName)
	}
	return provider.KindOf(classify(profile, status, nil, []byte(body), "m"))
}

// The status alone is not the answer, and one server proves it: 403 there
// means the prompt is too long, which the ordinary reading reports as a
// permissions failure nobody can do anything about.
func TestAProfileOverridesTheOrdinaryReadingOfAStatus(t *testing.T) {
	if kind := kindOf(t, "generic", http.StatusForbidden, `{}`); kind != provider.KindAuth {
		t.Errorf("generic 403 is %s, want auth", kind)
	}
	if kind := kindOf(t, "together", http.StatusForbidden, `{}`); kind != provider.KindContextOverflow {
		t.Errorf("together 403 is %s, want context_overflow", kind)
	}
	if kind := kindOf(t, "together", http.StatusPaymentRequired, `{}`); kind != provider.KindQuotaExhausted {
		t.Errorf("together 402 is %s, want quota_exhausted", kind)
	}
}

// A gateway normalizes whatever its upstream said into a type of its own, and
// its documentation says to read that rather than the status it chose.
func TestAGatewaysOwnClassificationIsPreferred(t *testing.T) {
	body := `{"error":{"code":429,"message":"no","metadata":{"error_type":"insufficient_credits"}}}`

	if kind := kindOf(t, "openrouter", http.StatusTooManyRequests, body); kind != provider.KindQuotaExhausted {
		t.Errorf("classified as %s; a spent balance would be retried forever as a rate limit", kind)
	}
}

// The same 429 covers a wait and a wall, and a machine-readable code says
// which even when the status does not.
func TestASpentBalanceIsNotARateLimit(t *testing.T) {
	tests := []struct {
		code string
		want provider.ErrorKind
	}{
		{"rate_limit_exceeded", provider.KindRateLimited},
		{"insufficient_quota", provider.KindQuotaExhausted},
		{"credit_balance_exhausted", provider.KindQuotaExhausted},
		{"organization_spend_limit_exceeded", provider.KindQuotaExhausted},
	}

	for _, test := range tests {
		body := `{"error":{"message":"x","code":"` + test.code + `"}}`
		if kind := kindOf(t, "generic", http.StatusTooManyRequests, body); kind != test.want {
			t.Errorf("%s classified as %s, want %s", test.code, kind, test.want)
		}
	}
}

// Local servers run out of hardware rather than allowance.
func TestRunningOutOfHardwareIsItsOwnKind(t *testing.T) {
	body := `{"error":{"message":"CUDA out of memory"}}`

	if kind := kindOf(t, "generic", http.StatusInternalServerError, body); kind != provider.KindResourceExhausted {
		t.Errorf("classified as %s, want resource_exhausted", kind)
	}
}

// The same 503 means two things on one server, and only the body separates
// them.
func TestAModelStillLoadingIsNotAFullQueue(t *testing.T) {
	loading := `{"error":{"message":"Loading model","type":"unavailable_error"}}`
	if kind := kindOf(t, "llamacpp", http.StatusServiceUnavailable, loading); kind != provider.KindTransient {
		t.Errorf("a loading model is %s, want transient", kind)
	}

	// A busy slot is a queue that drains, not a machine that cannot cope.
	full := `{"error":{"message":"no slot available"}}`
	if kind := kindOf(t, "llamacpp", http.StatusServiceUnavailable, full); kind != provider.KindOverloaded {
		t.Errorf("a server with no free slot is %s, want overloaded", kind)
	}
}

// A proxy in front of the endpoint answers in HTML. Reporting a decode failure
// would hide the outage behind a parser error.
func TestANonJsonBodyStillProducesAUsableFailure(t *testing.T) {
	failure := classify(profiles["generic"], http.StatusBadGateway, nil,
		[]byte("<html><body>502 Bad Gateway</body></html>"), "m")

	if kind := provider.KindOf(failure); kind != provider.KindTransient {
		t.Errorf("classified as %s, want transient", kind)
	}
	if !contains(failure.Error(), "Bad Gateway") {
		t.Errorf("what the server said was lost: %v", failure)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(haystack) > 0 && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// Retry-After is legal in two forms, and the date form is read against the
// server's own clock so that a difference between the machines does not become
// part of the wait.
func TestRetryAfterInBothItsForms(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

	seconds := http.Header{"Retry-After": []string{"20"}}
	if wait, ok := retryAfter(seconds, now); !ok || wait != 20*time.Second {
		t.Errorf("seconds form gave %v (%v)", wait, ok)
	}

	// The client's clock is an hour fast; the server's own Date says otherwise.
	dated := http.Header{
		"Retry-After": []string{"Fri, 28 Aug 2026 12:00:30 GMT"},
		"Date":        []string{"Fri, 28 Aug 2026 12:00:00 GMT"},
	}
	if wait, ok := retryAfter(dated, now.Add(time.Hour)); !ok || wait != 30*time.Second {
		t.Errorf("date form gave %v (%v), want 30s measured against the server's clock", wait, ok)
	}

	// A date already past is a wait of nothing, not a negative one.
	past := http.Header{"Retry-After": []string{"Fri, 28 Aug 2026 11:00:00 GMT"}}
	if wait, ok := retryAfter(past, now); !ok || wait != 0 {
		t.Errorf("a past date gave %v (%v)", wait, ok)
	}

	if _, ok := retryAfter(http.Header{}, now); ok {
		t.Error("a delay was invented from no header")
	}
	if _, ok := retryAfter(http.Header{"Retry-After": []string{"soon"}}, now); ok {
		t.Error("an unparseable header produced a delay")
	}
}

// A typo must not silently become the profile that knows nothing about how
// this server reports being out of credit.
func TestAnUnknownProfileIsRefused(t *testing.T) {
	if _, ok := ProfileByName("vlm"); ok {
		t.Error("a misspelled profile was accepted")
	}
	if _, ok := ProfileByName(""); !ok {
		t.Error("an unset profile did not fall back to generic")
	}
	for _, name := range ProfileNames() {
		if _, ok := ProfileByName(name); !ok {
			t.Errorf("%q is listed but does not resolve", name)
		}
	}
}

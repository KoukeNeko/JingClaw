package webui_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/webui"
)

func get(t *testing.T, path string) *http.Response {
	t.Helper()

	recorder := httptest.NewRecorder()
	webui.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder.Result()
}

// The console has to be in the binary, or the machine with no desktop on it —
// which is the whole reason it exists — has nothing to open.
func TestTheConsoleIsInTheBinary(t *testing.T) {
	for _, path := range []string{"/", "/app.js", "/app.css"} {
		response := get(t, path)
		if response.StatusCode != http.StatusOK {
			t.Errorf("%s returned %d", path, response.StatusCode)
		}
	}
}

// A refresh on a path the console made up must land on the console rather than
// on a 404.
func TestAnUnknownPathIsStillTheConsole(t *testing.T) {
	response := get(t, "/sessions/ses_01ABC")

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status is %d", response.StatusCode)
	}
	if kind := response.Header.Get("Content-Type"); !strings.Contains(kind, "text/html") {
		t.Errorf("content type is %q, want html", kind)
	}
}

// The page holds a credential, so the two cheap ways that becomes somebody
// else's — being framed, and a browser guessing at a content type — are shut
// off, and nothing on the page may reach anywhere but here.
func TestThePageIsNotSomewhereElseToBorrow(t *testing.T) {
	response := get(t, "/")

	for header, want := range map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := response.Header.Get(header); got != want {
			t.Errorf("%s is %q, want %q", header, got, want)
		}
	}

	policy := response.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "connect-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(policy, want) {
			t.Errorf("the policy does not say %q: %s", want, policy)
		}
	}
}

// What arrives in the URL is a code that works once, and even that does not
// stay in the address bar, where it would end up in history and in every
// screenshot of the page.
func TestThePageTradesACodeAndClearsTheURL(t *testing.T) {
	script := body(t, "/app.js")

	if !strings.Contains(script, "history.replaceState") {
		t.Error("the script does not clear the code from the address bar")
	}
	if !strings.Contains(script, "'/pair'") {
		t.Error("the script does not exchange the code for a credential")
	}
	// The credential itself must never be what the URL carries.
	if strings.Contains(script, "params.get('t')") {
		t.Error("the script still reads a credential straight out of the URL")
	}
}

// Everything the console renders comes from somewhere untrusted — a model's
// text, a tool's output, an MCP server's answer — and the page holds a
// credential. Building any of it as markup is how the two meet.
func TestThePageNeverBuildsMarkupFromContent(t *testing.T) {
	script := body(t, "/app.js")

	for _, forbidden := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("the script uses %s", forbidden)
		}
	}
}

func body(t *testing.T, path string) string {
	t.Helper()

	response := get(t, path)
	content, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}

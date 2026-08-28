package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A server is either started here or reached over HTTP. Naming both is a
// configuration where somebody meant something specific and this cannot tell
// which; guessing would silently start a process or open a connection nobody
// asked for.
func TestAServerIsOneKindOrTheOther(t *testing.T) {
	tests := []struct {
		name    string
		cfg     ServerConfig
		mention string
	}{
		{
			name:    "both",
			cfg:     ServerConfig{Name: "s", Command: "npx", URL: "http://localhost:9000/mcp"},
			mention: "only be one",
		},
		{
			name:    "neither",
			cfg:     ServerConfig{Name: "s"},
			mention: "neither",
		},
		{
			name:    "unnamed",
			cfg:     ServerConfig{Command: "npx"},
			mention: "needs a name",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := transportFor(test.cfg)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), test.mention) {
				t.Errorf("the refusal does not explain: %v", err)
			}
		})
	}
}

func TestEachKindProducesItsOwnTransport(t *testing.T) {
	child, err := transportFor(ServerConfig{Name: "s", Command: "echo"})
	if err != nil {
		t.Fatalf("command: %v", err)
	}
	if child == nil {
		t.Error("a command produced no transport")
	}

	remote, err := transportFor(ServerConfig{Name: "s", URL: "http://localhost:9000/mcp"})
	if err != nil {
		t.Fatalf("url: %v", err)
	}
	if remote == nil {
		t.Error("a url produced no transport")
	}
}

// Configured headers reach the server, which is where an authorization header
// goes, and the request that carries them is a copy: a round tripper must not
// modify what it was handed.
func TestHeadersAreSentAndTheRequestIsNotMutated(t *testing.T) {
	var seen http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := &http.Client{Transport: headerTransport(map[string]string{
		"Authorization": "Bearer secret",
		"X-Tenant":      "acme",
	})}

	original, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(original)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_ = response.Body.Close()

	if seen.Get("Authorization") != "Bearer secret" {
		t.Errorf("the credential was not sent: %q", seen.Get("Authorization"))
	}
	if seen.Get("X-Tenant") != "acme" {
		t.Errorf("a header was dropped: %q", seen.Get("X-Tenant"))
	}
	if original.Header.Get("Authorization") != "" {
		t.Error("the caller's own request was modified")
	}
}

// No headers means no wrapper, so an ordinary server pays nothing for a
// feature it does not use.
func TestNoHeadersUsesTheDefaultTransport(t *testing.T) {
	if headerTransport(nil) != http.DefaultTransport {
		t.Error("an empty header set still wrapped the transport")
	}
}

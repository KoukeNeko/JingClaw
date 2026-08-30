package mcp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/mcp/mcpauth"
)

// TestAServerNobodyHasSignedInToSaysSo is the seam that everything else hangs
// on.
//
// The daemon's handler refuses to start a flow and returns NeedsLogin. That
// refusal travels back out through the SDK's transport and its connect path,
// and if it arrives as an ordinary error the daemon reports a broken server
// instead of one waiting on a person — which is a fault somebody goes looking
// for rather than a command they run.
func TestAServerNobodyHasSignedInToSaysSo(t *testing.T) {
	protected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer protected.Close()

	sessions, err := mcpauth.Open(filepath.Join(t.TempDir(), mcpauth.DirName))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	_, err = Connect(context.Background(), ServerConfig{
		Name:     "books",
		URL:      protected.URL,
		OAuth:    true,
		Sessions: sessions,
	}, Limits{}, nil, slog.New(slog.DiscardHandler))

	if err == nil {
		t.Fatal("connecting to a server nobody signed in to succeeded")
	}

	var login *mcpauth.NeedsLogin
	if !errors.As(err, &login) {
		t.Fatalf("the refusal did not survive: %#v\n%v", err, err)
	}
	if login.Server != "books" {
		t.Errorf("it names the wrong server: %q", login.Server)
	}
	// The command, in the error itself. Whoever reads this has to know what
	// to do without going and looking it up.
	if got := login.Error(); !strings.Contains(got, "jingclaw mcp login books") {
		t.Errorf("the error does not say what to run: %s", got)
	}
}

// TestAServerWithNowhereToKeepASessionIsRefusedAtStartup keeps a deployment
// from looking authorized when it cannot be.
func TestAServerWithNowhereToKeepASessionIsRefusedAtStartup(t *testing.T) {
	_, err := transportFor(ServerConfig{Name: "books", URL: "https://books.example/mcp", OAuth: true})
	if err == nil {
		t.Fatal("a server with nowhere to keep a session was accepted")
	}
}

// TestACommandServerCannotAuthorizeWithOAuth says so rather than ignoring it.
//
// MCP is explicit that a server spoken to over a pipe should not use this
// profile. Dropping the setting quietly would leave somebody waiting for a
// sign-in prompt that is never coming.
func TestACommandServerCannotAuthorizeWithOAuth(t *testing.T) {
	_, err := transportFor(ServerConfig{Name: "files", Command: "echo", OAuth: true})
	if err == nil {
		t.Fatal("a child process was accepted as an oauth server")
	}
	if !strings.Contains(err.Error(), "env") {
		t.Errorf("the error does not say what to do instead: %v", err)
	}
}

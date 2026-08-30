package mcpauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"golang.org/x/oauth2"
)

// NeedsLogin says a server can only be reached after somebody signs in.
//
// A type rather than a sentence, because the daemon has to act on it: a
// server in this state stops being retried and its tools stop being offered,
// and both of those are decisions a caller makes by looking at the error
// rather than by reading it.
type NeedsLogin struct {
	Server string

	// Why is what happened, for somebody reading a log. Empty when nobody has
	// ever signed in, which is not a failure.
	Why error
}

func (e *NeedsLogin) Error() string {
	said := fmt.Sprintf("mcp server %q needs authorizing; run: jingclaw mcp login %s",
		e.Server, e.Server)
	if e.Why != nil {
		said += " (" + e.Why.Error() + ")"
	}
	return said
}

func (e *NeedsLogin) Unwrap() error { return e.Why }

// Stored is the daemon's side of authorization: it uses what somebody already
// signed in with, and never starts a flow of its own.
//
// Never, because there is nobody there. A daemon that opened a browser would
// be opening it on whichever machine it runs on, at whatever hour the token
// expired, for a person who is not looking — and in the meantime the run that
// wanted the tool is blocked on a page nobody will ever see.
//
// So Authorize refuses, and the refusal names the command that fixes it.
type Stored struct {
	Server string
	Store  *Store

	mu     sync.Mutex
	source oauth2.TokenSource
	failed error
}

var _ auth.OAuthHandler = (*Stored)(nil)

// TokenSource is the stored session, refreshing itself and writing back.
//
// Writing back matters: an authorization server may hand out a new refresh
// token every time one is used, and a client that keeps presenting the first
// one it was given stops working at the first rotation.
func (s *Stored) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.source != nil {
		return s.source, nil
	}
	if s.failed != nil {
		return nil, s.failed
	}

	session, err := s.Store.Load(s.Server)
	if errors.Is(err, ErrNoSession) {
		// Nil rather than an error: the transport is entitled to try without
		// a token, and what comes back tells us whether this server wanted
		// one at all. Refusing here would make every unauthenticated server
		// unreachable the moment somebody wrote oauth into its settings.
		return nil, nil
	}
	if err != nil {
		s.failed = err
		return nil, err
	}

	s.source = Saving(session.Config.TokenSource(ctx, session.Token),
		session.Config, session.Token, s.Store, s.Server)
	return s.source, nil
}

// Saving is a token source that writes each new token back before returning it.
//
// The wrapping is not optional bookkeeping. An authorization server is
// entitled to issue a new refresh token every time one is used and retire the
// old one, so a client that persists only what it was given at login works
// exactly once. The SDK ships this as an example rather than as API, which is
// why it is written out here.
func Saving(
	wrapped oauth2.TokenSource,
	config *oauth2.Config,
	held *oauth2.Token,
	store *Store,
	server string,
) oauth2.TokenSource {
	source := &savingSource{
		wrapped: wrapped,
		config:  config,
		store:   store,
		server:  server,
	}
	if held != nil {
		// So the first call, which usually returns the token just loaded,
		// does not rewrite the file it came from.
		source.access = held.AccessToken
	}
	return source
}

type savingSource struct {
	// mu also serialises refresh. Two goroutines refreshing at once will have
	// one of them present a refresh token the server has already retired, get
	// invalid_grant, and conclude that working credentials are dead.
	mu      sync.Mutex
	wrapped oauth2.TokenSource
	config  *oauth2.Config
	store   *Store
	server  string
	access  string
}

func (s *savingSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	token, err := s.wrapped.Token()
	if err != nil {
		return nil, err
	}
	if token.AccessToken == s.access {
		return token, nil
	}

	// Stored before it is handed out. A token used and then lost to a crash
	// is one whose rotated refresh token is gone, and the only way back from
	// that is a person and a browser.
	if err := s.store.Save(s.server, s.config, token); err != nil {
		return nil, fmt.Errorf("mcpauth: could not store the refreshed session: %w", err)
	}
	s.access = token.AccessToken
	return token, nil
}

// Authorize is where an interactive flow would go, and does not.
func (s *Stored) Authorize(_ context.Context, _ *http.Request, response *http.Response) error {
	// The caller's, by the interface's contract. Left open it would hold a
	// connection for the life of the daemon.
	closeBody(response)
	return &NeedsLogin{Server: s.Server}
}

// Recoverable says whether a token failure is worth waiting out.
//
// The distinction is the whole of how a headless deployment should behave. An
// authorization server having a bad ten minutes and a refresh token that has
// been revoked both arrive as "could not get a token", and treating them the
// same gives you one of two bad programs: one that sends somebody to a
// browser because of a network blip, or one that retries a dead credential
// every minute forever.
//
// Only the errors the token endpoint defines as final are final. Anything
// else — a timeout, a 502, a DNS failure — is this server being unavailable,
// which is a state it can come back from on its own.
func Recoverable(err error) bool {
	if err == nil {
		return true
	}

	var retrieve *oauth2.RetrieveError
	if !errors.As(err, &retrieve) {
		// Not an answer from the token endpoint at all: it never got there.
		return true
	}

	switch retrieve.ErrorCode {
	case "invalid_grant", "invalid_client", "unauthorized_client", "invalid_scope":
		return false
	}

	// An error code it did not recognise, from a response that was not an
	// error at all, is one to believe. A 5xx is the server's problem.
	if retrieve.Response != nil && retrieve.Response.StatusCode >= 500 {
		return true
	}
	return retrieve.ErrorCode == ""
}

// Forget discards a session, for signing out.
//
// Deliberately not called when a refresh fails. Goose clears its credentials
// on any refresh failure and falls back to a browser, which is defensible in
// a program somebody is sitting in front of and destructive in one nobody is:
// it turns ten minutes of an authorization server being down into a thing a
// person has to notice and come and fix.
func (s *Stored) Forget() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.source = nil
	s.failed = nil
	return s.Store.Forget(s.Server)
}

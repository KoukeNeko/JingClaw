package mcpauth

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// clientName is how this deployment introduces itself to an authorization
// server that has never seen it. It appears on the consent screen.
const clientName = "JingClaw"

// Login is the interactive side: a person, a browser, and one sign-in.
//
// Separate from Stored, and the separation is the design. Authorization needs
// somebody to read a consent screen; the daemon has nobody. So the flow lives
// in a command a person runs, and what it leaves behind is a file the daemon
// picks up.
type Login struct {
	Server   string
	Store    *Store
	Callback *Callback
}

// Handler is what to give a transport for one sign-in.
//
// One, and the wrapper below is what makes that true. The transport
// authorizes whatever gets refused, and its request and its event stream can
// both be refused at once — which without this means two browser tabs, two
// dynamically registered clients, and a person who signs in once while the
// other flow waits out the consent timeout and fails the whole command.
//
// Registration is tried in the order the SDK defines: a client identifier
// document first, then dynamic registration. There is no pre-registered
// client here and there cannot be — a client secret compiled into a program
// anybody can download is not a secret, which is why RFC 8252 calls a native
// application a public client.
func (l *Login) Handler() (auth.OAuthHandler, error) {
	if l.Callback == nil {
		return nil, fmt.Errorf("mcpauth: signing in needs somewhere to be redirected to")
	}

	redirect := l.Callback.RedirectURL()

	handler, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				ClientName:   clientName,
				RedirectURIs: []string{redirect},
				// Asked for explicitly so the authorization server knows to
				// issue one. Without it a token that expires is a sign-in
				// somebody has to repeat, on the server's schedule.
				GrantTypes:    []string{"authorization_code", "refresh_token"},
				ResponseTypes: []string{"code"},
				// A public client: it holds no secret, so there is nothing
				// for the token endpoint to authenticate it with.
				TokenEndpointAuthMethod: "none",
			},
		},
		RedirectURL:              redirect,
		AuthorizationCodeFetcher: l.Callback.Fetch,
		RequestRefreshToken:      true,

		// Called once the code has been exchanged. This is where what was
		// obtained becomes something the daemon can use tomorrow.
		NewTokenSource: func(
			ctx context.Context, config *oauth2.Config, token *oauth2.Token,
		) (oauth2.TokenSource, error) {
			if err := l.Store.Save(l.Server, config, token); err != nil {
				return nil, err
			}
			return Saving(config.TokenSource(ctx, token), config, token, l.Store, l.Server), nil
		},
	})
	if err != nil {
		return nil, err
	}
	return &once{inner: handler}, nil
}

// once lets a sign-in happen a single time, however many requests were
// refused.
//
// The second caller does not start a second flow: it waits for the first and
// then finds the token already there. Returning nil is what the interface
// asks for — the caller retries its request, and by then TokenSource has
// something to give it.
//
// A failure is remembered too, and that is not merely tidiness. Whatever
// stopped the first attempt will stop the second, so a retry means a second
// browser tab for a person who has already signed in once — and, worse, the
// reason they actually need is then buried under whatever the second attempt
// eventually fails with. That happened while this was being written: a real
// refusal from the authorization server was reported as "nobody finished
// signing in", because the second flow was still waiting for a browser that
// had already been used on the first.
type once struct {
	inner auth.OAuthHandler

	mu     sync.Mutex
	done   bool
	failed error
}

var _ auth.OAuthHandler = (*once)(nil)

func (o *once) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	return o.inner.TokenSource(ctx)
}

func (o *once) Authorize(ctx context.Context, request *http.Request, response *http.Response) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	// Ours to close in both of these, by the interface's contract: the inner
	// handler is not being given the chance to.
	if o.done {
		closeBody(response)
		return nil
	}
	if o.failed != nil {
		closeBody(response)
		return o.failed
	}

	if err := o.inner.Authorize(ctx, request, response); err != nil {
		o.failed = err
		return err
	}
	o.done = true
	return nil
}

// Signed reports whether a session is already stored, and whether it still
// works.
//
// Asked before starting a flow so that signing in to a server that is already
// signed in costs nothing. A stored session that cannot get a token is not
// treated as signed in: that is the state a person ran this command to leave.
func Signed(ctx context.Context, store *Store, server string) (bool, error) {
	session, err := store.Load(server)
	if err != nil {
		return false, err
	}

	source := Saving(session.Config.TokenSource(ctx, session.Token),
		session.Config, session.Token, store, server)
	if _, err := source.Token(); err != nil {
		return false, err
	}
	return true, nil
}

// closeBody is the OAuthHandler contract: whoever is handed the response owns
// it. Left open, one connection per failed request stays held.
func closeBody(response *http.Response) {
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
}

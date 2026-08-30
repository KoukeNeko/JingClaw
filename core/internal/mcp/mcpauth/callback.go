// Package mcpauth authorizes this deployment against an MCP server.
//
// The protocol is the SDK's: discovery, registration, PKCE and the token
// exchange all happen in modelcontextprotocol/go-sdk. What is here is the two
// things it deliberately leaves to the client — getting a person to an
// authorization page, and deciding where the resulting token lives.
package mcpauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// callbackPath is where the authorization server sends the browser back.
//
// A path rather than the root, so a stray request to the listener — a browser
// probing for a favicon, something else on this machine scanning ports — is
// answered with 404 instead of being read as an authorization response.
const callbackPath = "/callback"

// callbackTimeout is how long somebody has to finish signing in.
//
// Generous, because what is happening on the other side is a person reading a
// consent screen, possibly finding a second factor. Not unbounded, because a
// process holding a port open forever after somebody closed the tab is one
// that has to be killed.
const callbackTimeout = 5 * time.Minute

// Callback is a loopback address an authorization server can redirect to.
//
// Bound before anything else happens, because the port is part of the redirect
// URI and the redirect URI has to be registered with the authorization server
// before the flow starts. A fixed port would avoid that ordering and is worse:
// it fails when something else holds it, and on a shared machine it is a port
// anybody could have been listening on first.
type Callback struct {
	listener net.Listener
	server   *http.Server

	// waiting maps the state of each authorization in flight to whoever is
	// waiting for it.
	//
	// A map rather than one channel, because more than one can be in flight.
	// The transport authorizes whatever gets refused, and its request and its
	// event stream are two things that can both be refused at once; with one
	// channel, whichever redirect arrived first satisfied whichever call was
	// listening, and the state it carried then belonged to the other one.
	mu      sync.Mutex
	waiting map[string]chan authorizationResult

	// Announce is where the authorization URL is written for somebody who
	// cannot use the browser this opens — over ssh, or on a machine with no
	// browser at all. Nil says nothing.
	Announce func(url string)
}

type authorizationResult struct {
	result *auth.AuthorizationResult
	err    error
}

// Listen binds the loopback address the flow will redirect to.
//
// 127.0.0.1 rather than localhost: the name can resolve to something else, and
// what has to be true here is that nothing off this machine can reach it.
func Listen() (*Callback, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("mcpauth: could not open a local address to be redirected to: %w", err)
	}

	callback := &Callback{
		listener: listener,
		waiting:  make(map[string]chan authorizationResult),
	}

	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, callback.receive)
	callback.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		// Ends when Close is called, which is the ordinary way out.
		_ = callback.server.Serve(listener)
	}()

	return callback, nil
}

// RedirectURL is what to register with the authorization server.
func (c *Callback) RedirectURL() string {
	return "http://" + c.listener.Addr().String() + callbackPath
}

// Close stops listening.
func (c *Callback) Close() error {
	shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := c.server.Shutdown(shutdown); err != nil {
		return c.listener.Close()
	}
	return nil
}

// Fetch is auth.AuthorizationCodeFetcher: it puts somebody in front of the
// authorization page and waits for the answer to come back here.
func (c *Callback) Fetch(
	ctx context.Context, args *auth.AuthorizationArgs,
) (*auth.AuthorizationResult, error) {
	asked, err := url.Parse(args.URL)
	if err != nil {
		return nil, fmt.Errorf("mcpauth: the authorization url is not one: %w", err)
	}

	// What ties the redirect that comes back to this call and not another.
	// Without it there is nothing to route by, and the caller will reject
	// whatever arrives anyway.
	state := asked.Query().Get("state")
	if state == "" {
		return nil, errors.New("mcpauth: the authorization url carries no state")
	}

	mine := make(chan authorizationResult, 1)
	c.mu.Lock()
	c.waiting[state] = mine
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.waiting, state)
		c.mu.Unlock()
	}()

	if c.Announce != nil {
		c.Announce(args.URL)
	}

	// Not fatal. A machine with no browser is a machine where somebody opens
	// the announced URL themselves, and refusing here would make that
	// impossible rather than inconvenient.
	openBrowser(args.URL)

	waiting, cancel := context.WithTimeout(ctx, callbackTimeout)
	defer cancel()

	select {
	case answer := <-mine:
		return answer.result, answer.err
	case <-waiting.Done():
		return nil, fmt.Errorf("mcpauth: nobody finished signing in: %w", context.Cause(waiting))
	}
}

// receive is the authorization server's redirect, arriving as a page load.
func (c *Callback) receive(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	answer := authorizationResult{}
	if failed := query.Get("error"); failed != "" {
		answer.err = authorizationDenied(query)
	} else if code := query.Get("code"); code == "" {
		answer.err = errors.New("mcpauth: the authorization server redirected back without a code")
	} else {
		answer.result = &auth.AuthorizationResult{
			Code:  code,
			State: query.Get("state"),
			// RFC 9207. The SDK checks it against the issuer it discovered,
			// which is what stops a response from one authorization server
			// being replayed as another's.
			Iss: query.Get("iss"),
		}
	}

	if !c.deliver(query.Get("state"), answer) {
		w.Header().Set("content-type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, page("Not expected",
			"Nothing here is waiting for this. It may have already finished."))
		return
	}

	w.Header().Set("content-type", "text/html; charset=utf-8")
	if answer.err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, page("Not authorized", answer.err.Error()))
		return
	}
	fmt.Fprint(w, page("Authorized", "You can close this tab and go back to the terminal."))
}

// deliver hands a redirect to whoever asked for it, and says whether anybody
// had.
//
// A refusal with no state goes to everybody instead of nowhere. The spec says
// an authorization server must echo the state it was sent, including on an
// error, but a server that does not would otherwise leave the sign-in waiting
// for the five minutes somebody has to read a consent screen — for a consent
// that has already been declined. Every flow here belongs to the same
// invocation, so refusing all of them is refusing the thing that was refused.
//
// A success is never delivered that way. A code that cannot be tied to the
// request that asked for it is the exact thing state exists to prevent.
func (c *Callback) deliver(state string, answer authorizationResult) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if mine, expected := c.waiting[state]; expected {
		// Buffered and non-blocking: a second redirect for the same state — a
		// refresh, a browser prefetching — must not hang a request handler on
		// a channel nobody is reading any more.
		select {
		case mine <- answer:
		default:
		}
		return true
	}

	if answer.err == nil || state != "" || len(c.waiting) == 0 {
		return false
	}
	for _, mine := range c.waiting {
		select {
		case mine <- answer:
		default:
		}
	}
	return true
}

// authorizationDenied is the server's own words about why.
//
// Its description is shown rather than replaced, because the reason a consent
// was refused is the server's to give and a generic sentence here would hide
// the one thing somebody needs in order to fix it.
func authorizationDenied(query url.Values) error {
	said := query.Get("error")
	if described := query.Get("error_description"); described != "" {
		said += ": " + described
	}
	return fmt.Errorf("mcpauth: the authorization server refused: %s", said)
}

func page(title, said string) string {
	return "<!doctype html><meta charset=utf-8><title>" + title +
		"</title><body style=\"font:16px system-ui;margin:4rem auto;max-width:32rem\">" +
		"<h1>" + title + "</h1><p>" + said + "</p>"
}

// NoBrowser stops this opening one, for a machine where that is wrong.
//
// A check that signs in has to be able to run without a browser window
// appearing on whoever's screen, and so does a session over ssh where the
// opener would succeed on the wrong machine. The URL is still announced, so
// the flow is still completable by hand.
const NoBrowser = "JINGCLAW_NO_BROWSER"

// openBrowser asks the desktop to open a URL.
//
// The system browser, never an embedded one: RFC 8252 is explicit that an
// embedded user-agent lets the app read what the person types into somebody
// else's login page, and an authorization server has no way to tell.
//
// Failure is ignored on purpose — see Fetch.
func openBrowser(open string) {
	if os.Getenv(NoBrowser) != "" {
		return
	}

	// A URL that is not http(s) is not something to hand to a shell-adjacent
	// opener. The SDK builds this one, so this is a guard against a future
	// caller rather than against the SDK.
	if !strings.HasPrefix(open, "https://") && !strings.HasPrefix(open, "http://") {
		return
	}

	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", open)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", open)
	default:
		command = exec.Command("xdg-open", open)
	}

	_ = command.Start()
	if command.Process != nil {
		// Reaped in the background: the opener exits as soon as the browser
		// has the URL, and leaving it unwaited would leave a zombie behind
		// for the life of the daemon.
		go func() { _ = command.Wait() }()
	}
}

package mcp

import (
	"context"
	"fmt"
	"net/http"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/KoukeNeko/JingClaw/core/internal/mcp/mcpauth"
)

// signInTimeout bounds the whole sign-in, including the part where somebody
// reads a consent screen. Longer than starting a server, for that reason.
const signInTimeout = 6 * time.Minute

// SignIn authorizes this deployment against one server, with a person present.
//
// Driven by an actual connection rather than by calling the authorization
// endpoints directly. What has to be discovered — where the authorization
// server is, what scopes this resource wants — is only told to a client that
// asks for something and is refused, so the way to start is to be refused.
//
// Leaves a session behind. Nothing else about the daemon changes: it picks the
// session up the next time it connects.
func SignIn(ctx context.Context, cfg ServerConfig, callback *mcpauth.Callback) error {
	if cfg.URL == "" {
		return fmt.Errorf("mcp: %s is not reached over http, so there is nothing to sign in to", cfg.Name)
	}
	if cfg.Sessions == nil {
		return fmt.Errorf("mcp: there is nowhere to keep %s's session", cfg.Name)
	}

	login := &mcpauth.Login{Server: cfg.Name, Store: cfg.Sessions, Callback: callback}
	handler, err := login.Handler()
	if err != nil {
		return err
	}

	transport := &sdk.StreamableClientTransport{
		Endpoint:     cfg.URL,
		HTTPClient:   &http.Client{Transport: headerTransport(cfg.Headers)},
		OAuthHandler: handler,
	}

	signing, cancel := context.WithTimeout(ctx, signInTimeout)
	defer cancel()

	client := sdk.NewClient(&sdk.Implementation{Name: clientName, Version: clientVersion}, nil)
	session, err := client.Connect(signing, transport, nil)
	if err != nil {
		return fmt.Errorf("mcp: signing in to %s: %w", cfg.Name, err)
	}
	defer session.Close()

	// Asked for so the sign-in is proved rather than assumed. A token that
	// was issued and is not accepted is a thing to find out now, while
	// somebody is here, rather than at the daemon's next restart.
	if _, err := session.ListTools(signing, nil); err != nil {
		return fmt.Errorf("mcp: %s authorized but would not answer: %w", cfg.Name, err)
	}
	return nil
}

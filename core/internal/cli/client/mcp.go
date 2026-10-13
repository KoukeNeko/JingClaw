package client

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/mcp"
	"github.com/KoukeNeko/JingClaw/core/internal/mcp/mcpauth"
)

// newMCPCommand is signing in to tool servers, and nothing else.
//
// Local, except for one question: which servers there are. That is a setting,
// settings are in the daemon's database, and the daemon is what answers for
// them. What it does with the answer is put somebody in front of a consent
// screen and leave a session on disk, and the daemon reads that the next time
// it connects. Going through the daemon for that would mean asking a
// background process to open a browser, which is the thing this exists to
// avoid.
func newMCPCommand() *cobra.Command {
	mcpCmd := &cobra.Command{
		Use:   "mcp",
		Short: "Sign in to tool servers",
	}
	mcpCmd.AddCommand(
		newMCPLoginCommand(),
		newMCPListCommand(),
		newMCPLogoutCommand(),
	)
	return mcpCmd
}

func newMCPLoginCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "login <server>",
		Short: "Authorize this deployment against a tool server",
		Long: "Opens a browser so you can sign in, then stores the session for the daemon.\n\n" +
			"The daemon never does this itself: it has no browser and nobody is watching it, " +
			"so a server whose session has run out is reported as needing one rather than " +
			"blocking a run on a page no one will open.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			server, _, err := serverNamed(cmd.Context(), args[0])
			if err != nil {
				return err
			}

			callback, err := mcpauth.Listen()
			if err != nil {
				return err
			}
			defer callback.Close()

			// For a machine with no browser, or one reached over ssh. The
			// opener still runs; this is what makes it optional rather than
			// required.
			callback.Announce = func(url string) {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"Opening a browser to sign in to %s.\nIf nothing opens, go to:\n\n%s\n\n",
					server.Name, url)
			}

			if err := mcp.SignIn(cmd.Context(), server, callback); err != nil {
				return err
			}

			fmt.Fprintf(cmd.ErrOrStderr(), "signed in to %s\n", server.Name)
			return nil
		},
	}
}

func newMCPListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show tool servers and whether they are signed in",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			servers, err := configuredServers(cmd.Context())
			if err != nil {
				return err
			}
			if len(servers) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no tool servers are configured")
				return nil
			}

			sessions, err := mcpauth.Open(mcpauth.DefaultDir())
			if err != nil {
				return err
			}

			for _, configured := range servers {
				fmt.Printf("%-20s %s\n", configured.Name,
					describeAuth(cmd.Context(), sessions, configured))
			}
			return nil
		},
	}
}

// describeAuth is the one line a person reads to know what to do next.
//
// It says what to run, because "needs authentication" without the command is
// a state somebody has to go and look up.
func describeAuth(ctx context.Context, sessions *mcpauth.Store, configured config.MCPServer) string {
	if !configured.OAuth {
		if configured.Command != "" {
			return "runs as a command"
		}
		return "no authorization"
	}

	signed, err := mcpauth.Signed(ctx, sessions, configured.Name)
	switch {
	case signed:
		return "signed in"
	case errors.Is(err, mcpauth.ErrNoSession):
		return fmt.Sprintf("not signed in — run: jingclaw mcp login %s", configured.Name)
	case !mcpauth.Recoverable(err):
		return fmt.Sprintf("sign-in expired — run: jingclaw mcp login %s", configured.Name)
	default:
		// Distinct on purpose. A session that could not be checked because
		// the authorization server is unreachable is not one to go and
		// replace: signing in again would mean going to the same server.
		return fmt.Sprintf("could not be checked: %v", err)
	}
}

func newMCPLogoutCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "logout <server>",
		Short: "Forget a tool server's stored session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// The servers are asked for so a typo is reported rather than
			// silently forgetting nothing.
			if _, _, err := serverNamed(cmd.Context(), args[0]); err != nil {
				return err
			}

			sessions, err := mcpauth.Open(mcpauth.DefaultDir())
			if err != nil {
				return err
			}
			if err := sessions.Forget(args[0]); err != nil {
				return err
			}

			fmt.Fprintf(cmd.ErrOrStderr(), "forgot %s\n", args[0])
			return nil
		},
	}
}

// needsTheDaemon is what is said when the daemon cannot be asked, in front of
// the reason it could not be.
const needsTheDaemon = "the tool servers are kept in the daemon's settings, so it has to be running; " +
	"start it first with: jingclaw"

// configuredServers asks the running daemon which tool servers are saved.
//
// The saved settings, not what the daemon is running: signing in to a server
// that was added a minute ago is the point of adding it, and the daemon picks
// the session up when it next connects. A daemon that is not running has
// nobody to ask, and the error says that rather than leaving a missing
// discovery file to be decoded by whoever reads it.
func configuredServers(ctx context.Context) ([]config.MCPServer, error) {
	settings, err := dialConfig()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", needsTheDaemon, err)
	}

	saved, err := settings.GetConfig(ctx, connect.NewRequest(&controlv1.GetConfigRequest{}))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", needsTheDaemon, err)
	}
	if reason := saved.Msg.GetParseError(); reason != "" {
		// The daemon answers for text it cannot read, so that the page that
		// repairs it can show it. There is nothing to list from it.
		return nil, fmt.Errorf(
			"the saved settings cannot be read, so there is no list of tool servers: %s\nfix them in the dashboard: run jingclaw dashboard",
			reason)
	}

	// The same reading the daemon does at start, so what is listed here is what
	// it will connect to.
	cfg, err := config.Candidate([]byte(saved.Msg.GetText()))
	if err != nil {
		return nil, err
	}
	return cfg.MCP.Servers, nil
}

// serverNamed finds one configured server, with the session store attached.
func serverNamed(ctx context.Context, name string) (mcp.ServerConfig, *mcpauth.Store, error) {
	servers, err := configuredServers(ctx)
	if err != nil {
		return mcp.ServerConfig{}, nil, err
	}

	for _, configured := range servers {
		if configured.Name != name {
			continue
		}
		if !configured.OAuth {
			return mcp.ServerConfig{}, nil, fmt.Errorf(
				"%s does not authorize with oauth; set oauth = true in its settings if it should",
				name)
		}

		sessions, err := mcpauth.Open(mcpauth.DefaultDir())
		if err != nil {
			return mcp.ServerConfig{}, nil, err
		}

		return mcp.ServerConfig{
			Name:     configured.Name,
			URL:      configured.URL,
			Headers:  configured.Headers,
			Sessions: sessions,
			OAuth:    true,
		}, sessions, nil
	}

	// The names that do exist, because the commonest reason to be here is a
	// typo and the fix is on screen.
	names := make([]string, 0, len(servers))
	for _, configured := range servers {
		names = append(names, configured.Name)
	}
	sort.Strings(names)

	if len(names) == 0 {
		return mcp.ServerConfig{}, nil, fmt.Errorf("no tool servers are configured")
	}
	return mcp.ServerConfig{}, nil, fmt.Errorf(
		"there is no tool server named %q; configured: %v", name, names)
}

package client

import (
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/mcp/mcpauth"
)

// dashboardPath is where the daemon serves the page.
const dashboardPath = "/ui/"

// newDashboardCommand lets a browser into the dashboard.
//
// The browser is not handed the credential this command holds. It is handed a
// code that works once, for a minute, and trades it for a credential of its
// own. The code is in the fragment of the address, which is never sent to a
// server, and the page removes it as soon as it has read it.
func newDashboardCommand() *cobra.Command {
	var show bool

	command := &cobra.Command{
		Use:   "dashboard",
		Short: "Open the dashboard in a browser",
		Long: "Opens the dashboard: sessions, what is waiting on you, and the settings.\n\n" +
			"The link it prints works once and for a minute. Run this again to let another " +
			"browser in, or the same one after the daemon has restarted: a browser's " +
			"access ends when the daemon stops, and after twelve hours.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			httpClient, baseURL, err := authenticated()
			if err != nil {
				return err
			}

			asked, err := controlv1connect.NewBrowserServiceClient(httpClient, baseURL).
				CreatePairing(cmd.Context(), connect.NewRequest(&controlv1.CreatePairingRequest{Meta: newMeta()}))
			if err != nil {
				return err
			}

			link := baseURL + dashboardPath + "#pair=" + asked.Msg.GetCode()

			if show {
				// For a script, or a machine with no browser: the link alone.
				fmt.Fprintln(cmd.OutOrStdout(), link)
				return nil
			}

			fmt.Fprintf(cmd.ErrOrStderr(),
				"Opening the dashboard. If nothing opens, go to:\n\n%s\n\nThe link works once, for %s.\n",
				link, time.Until(asked.Msg.GetExpiresAt().AsTime()).Round(time.Second))
			mcpauth.OpenBrowser(link)
			return nil
		},
	}

	command.Flags().BoolVar(&show, "print", false, "print the link and do not open a browser")
	return command
}

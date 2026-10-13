package client

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/discovery"
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
			"When the daemon could not start an agent it says what is wrong first; the dashboard " +
			"then shows only what is needed to fix it.\n\n" +
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

			// Before the link, and on the error stream whichever way the link
			// goes out: a daemon with no agent opens a page that only shows what
			// to repair, and somebody who has just been told what is wrong has
			// less reason to wonder why that is all it shows.
			sayWhyThereIsNoAgent(cmd.Context(), cmd.ErrOrStderr())

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

// sayWhyThereIsNoAgent says so when the daemon is running no agent, and what
// is wrong.
//
// Not being able to ask is said and does not stop the link: the dashboard is
// the way to find out more, and refusing to open it over a question about
// it would be the wrong way round.
func sayWhyThereIsNoAgent(ctx context.Context, w io.Writer) {
	state, err := AgentState(ctx, runtimeDir)
	if err != nil {
		fmt.Fprintf(w, "Could not ask whether the agent is running: %v\n\n", err)
		return
	}
	if state.GetMode() != discovery.ModeRepair {
		return
	}

	fmt.Fprintln(w, "The agent is not running, so the dashboard shows only what is needed to fix the settings.")
	fmt.Fprintln(w, "What is wrong:")
	fmt.Fprintln(w)
	for _, problem := range state.GetProblems() {
		fmt.Fprintf(w, "  %s\n", ProblemLine(problem))
		if fix := problem.GetFix(); fix != "" {
			fmt.Fprintf(w, "      %s\n", strings.Join(strings.Fields(fix), " "))
		}
	}
	fmt.Fprintln(w)
}

// AgentState asks the daemon publishing itself in where whether it is running
// an agent, and if it is not, why.
//
// Exported for the supervisor, which has to know before it starts anything that
// needs an agent.
func AgentState(ctx context.Context, where string) (*controlv1.GetAgentStateResponse, error) {
	httpClient, baseURL, err := authenticatedIn(where)
	if err != nil {
		return nil, err
	}

	asked, err := controlv1connect.NewConfigServiceClient(httpClient, baseURL).
		GetAgentState(ctx, connect.NewRequest(&controlv1.GetAgentStateRequest{}))
	if err != nil {
		return nil, err
	}
	return asked.Msg, nil
}

// ProblemLine is one reason there is no agent on one line: the setting and
// what is wrong with it, or only what is wrong when it is not about a setting.
func ProblemLine(problem *controlv1.ConfigProblem) string {
	why := strings.Join(strings.Fields(problem.GetWhy()), " ")
	if problem.GetKey() == "" {
		return why
	}
	if problem.GetValue() == "" {
		return fmt.Sprintf("%s: %s", problem.GetKey(), why)
	}
	return fmt.Sprintf("%s = %s: %s", problem.GetKey(), problem.GetValue(), why)
}

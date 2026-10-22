package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"golang.org/x/term"

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
			"browser in, or the same one after it has signed out: a browser's " +
			"access lasts twelve hours and survives a restart. The dashboard password " +
			"(see jingclaw dashboard password) signs a browser in without a link.",
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
			initial := passwordIsInitial(cmd.Context(), httpClient, baseURL)

			// Before the link, and on the error stream whichever way the link
			// goes out: a daemon with no agent opens a page that only shows what
			// to repair, and somebody who has just been told what is wrong has
			// less reason to wonder why that is all it shows.
			sayWhyThereIsNoAgent(cmd.Context(), cmd.ErrOrStderr())

			if initial {
				// Said on the error stream, so that --print still prints the link alone.
				fmt.Fprintln(cmd.ErrOrStderr(), initialPasswordNote)
			}

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
	command.AddCommand(newDashboardPasswordCommand())
	return command
}

const initialPasswordNote = "The dashboard password is still the initial one. A browser can sign in with it, " +
	"but the dashboard will make you change it first. Set one now with: jingclaw dashboard password"

// passwordIsInitial asks the daemon whether the password is still the initial
// one. Not being able to ask is not worth saying: the link does not depend on it.
func passwordIsInitial(ctx context.Context, httpClient *http.Client, baseURL string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/ui/auth", nil)
	if err != nil {
		return false
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	var state struct {
		Initial bool `json:"initial"`
	}
	return res.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(res.Body, 1<<10)).Decode(&state) == nil && state.Initial
}

// newDashboardPasswordCommand sets the password a browser can sign in with
// instead of a link, or puts it back to the initial one.
func newDashboardPasswordCommand() *cobra.Command {
	var clear bool

	command := &cobra.Command{
		Use:   "password",
		Short: "Set the dashboard password, or reset it to the initial one",
		Long: "Sets the password a browser signs in with, in place of a link from `jingclaw dashboard`. " +
			"Until it has been set, the password is the initial one (JINGCLAW_DASHBOARD_PASSWORD when that is set, " +
			"otherwise the fixed value in the documentation), and the dashboard makes you change it at first login; " +
			"this command changes it without needing to know the initial one.\n\n" +
			"The new password is read from standard input, without echo on a terminal; " +
			"without one, the first line is the new password and a second line, once a password has been set, is the current one. " +
			"Ten to 256 characters, and not the initial one. Changing it signs every other browser out.\n\n" +
			"With --clear the password goes back to the initial one, every browser is signed out, " +
			"and the current one is read instead.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			httpClient, baseURL, err := authenticated()
			if err != nil {
				return err
			}
			browsers := controlv1connect.NewBrowserServiceClient(httpClient, baseURL)

			in := newSecretReader(cmd.InOrStdin(), cmd.ErrOrStderr())
			var newPassword string
			if !clear {
				if newPassword, err = in.read("New password: "); err != nil {
					return err
				}
				if in.terminal {
					again, err := in.read("Repeat it: ")
					if err != nil {
						return err
					}
					if again != newPassword {
						return errors.New("the two passwords differ; nothing was changed")
					}
				}
			}

			current := ""
			if !in.terminal {
				// A pipe has no way to be asked a second time.
				current, _ = in.read("")
			}
			for {
				var err error
				if clear {
					_, err = browsers.ClearDashboardPassword(cmd.Context(), connect.NewRequest(
						&controlv1.ClearDashboardPasswordRequest{Meta: newMeta(), CurrentPassword: current}))
				} else {
					_, err = browsers.SetDashboardPassword(cmd.Context(), connect.NewRequest(
						&controlv1.SetDashboardPasswordRequest{Meta: newMeta(), NewPassword: newPassword, CurrentPassword: current}))
				}
				if connect.CodeOf(err) == connect.CodePermissionDenied && in.terminal && current == "" {
					if current, err = in.read("Current password: "); err != nil {
						return err
					}
					continue
				}
				if err != nil {
					return err
				}
				break
			}

			if clear {
				fmt.Fprintln(cmd.ErrOrStderr(), "The dashboard password is back to the initial one. Every browser is signed out.")
			} else {
				fmt.Fprintln(cmd.ErrOrStderr(), "The dashboard password is set. Every other browser is signed out.")
			}
			return nil
		},
	}

	command.Flags().BoolVar(&clear, "clear", false, "put the password back to the initial one")
	return command
}

// secretReader reads passwords: from a terminal without echo, otherwise one
// line at a time from a stream.
type secretReader struct {
	terminal bool
	fd       int
	lines    *bufio.Reader
	out      io.Writer
}

func newSecretReader(in io.Reader, out io.Writer) *secretReader {
	file, ok := in.(*os.File)
	if ok && term.IsTerminal(int(file.Fd())) {
		return &secretReader{terminal: true, fd: int(file.Fd()), out: out}
	}
	return &secretReader{lines: bufio.NewReader(in), out: out}
}

func (r *secretReader) read(prompt string) (string, error) {
	if r.terminal {
		fmt.Fprint(r.out, prompt)
		raw, err := term.ReadPassword(r.fd)
		fmt.Fprintln(r.out)
		return string(raw), err
	}
	line, err := r.lines.ReadString('\n')
	if err != nil && line == "" {
		if errors.Is(err, io.EOF) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
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
// Exported for the bare command, which has to know before it starts anything that
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

// Command agent is the JingClaw command-line client.
//
// It is a control-plane client and nothing more: it holds no runtime, no
// provider and no state. Everything it shows comes from the daemon's event
// stream, which is what keeps the CLI, the GUIs and the web UI looking at one
// consistent world instead of three divergent ones.
//
// This binary must never import internal/runtime. A test enforces it.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/discovery"
)

const clientName = "jingclaw-cli"

func main() {
	if err := newRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "agent",
		Short:         "Control the JingClaw agent daemon",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(newSessionCommand(), newSendCommand(), newAttachCommand(), newInterruptCommand())
	return root
}

func newSessionCommand() *cobra.Command {
	session := &cobra.Command{Use: "session", Short: "Manage sessions"}

	var title string
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a session and print its ID",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := dial()
			if err != nil {
				return err
			}

			resp, err := client.CreateSession(cmd.Context(), connect.NewRequest(&controlv1.CreateSessionRequest{
				Meta:  newMeta(),
				Title: title,
			}))
			if err != nil {
				return err
			}

			fmt.Println(resp.Msg.GetSession().GetId())
			return nil
		},
	}
	create.Flags().StringVar(&title, "title", "", "human-readable session title")

	session.AddCommand(create)
	return session
}

func newSendCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "send <session-id> <text>",
		Short: "Send a user turn and print the run ID",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dial()
			if err != nil {
				return err
			}

			resp, err := client.SendTurn(cmd.Context(), connect.NewRequest(&controlv1.SendTurnRequest{
				Meta:      newMeta(),
				SessionId: args[0],
				Text:      args[1],
			}))
			if err != nil {
				return err
			}

			// Only the run ID comes back. The answer arrives on the event
			// stream, so this command returning does not stop the run.
			fmt.Println(resp.Msg.GetRunId())
			return nil
		},
	}
}

func newAttachCommand() *cobra.Command {
	var afterSeq uint64

	cmd := &cobra.Command{
		Use:   "attach <session-id>",
		Short: "Follow a session's event stream",
		Long: "Follow a session's event stream.\n\n" +
			"Detaching does not affect the run: the daemon owns it. Reattach with\n" +
			"--after <seq> to resume exactly where you left off.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dial()
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			stream, err := client.SubscribeEvents(ctx, connect.NewRequest(&controlv1.SubscribeEventsRequest{
				SessionId: args[0],
				AfterSeq:  afterSeq,
				ClientId:  clientName,
			}))
			if err != nil {
				return err
			}
			defer func() { _ = stream.Close() }()

			for stream.Receive() {
				printFrame(stream.Msg())
			}

			if err := stream.Err(); err != nil {
				if errors.Is(err, context.Canceled) || connect.CodeOf(err) == connect.CodeCanceled {
					return nil
				}
				return err
			}
			return nil
		},
	}

	cmd.Flags().Uint64Var(&afterSeq, "after", 0, "resume after this sequence number")
	return cmd
}

func newInterruptCommand() *cobra.Command {
	var reason string

	cmd := &cobra.Command{
		Use:   "interrupt <run-id>",
		Short: "Ask a run to stop",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dial()
			if err != nil {
				return err
			}

			resp, err := client.InterruptRun(cmd.Context(), connect.NewRequest(&controlv1.InterruptRunRequest{
				Meta:   newMeta(),
				RunId:  args[0],
				Reason: reason,
			}))
			if err != nil {
				return err
			}

			fmt.Println(strings.ToLower(strings.TrimPrefix(resp.Msg.GetStatus().String(), "RUN_STATUS_")))
			return nil
		},
	}

	cmd.Flags().StringVar(&reason, "reason", "", "why the run is being interrupted")
	return cmd
}

func printFrame(frame *controlv1.SubscribeEventsResponse) {
	switch value := frame.GetValue().(type) {
	case *controlv1.SubscribeEventsResponse_Hello:
		fmt.Fprintf(os.Stderr, "attached at seq %d\n", value.Hello.GetHeadSeq())

	case *controlv1.SubscribeEventsResponse_Heartbeat:
		// Liveness only; nothing for a human to read.

	case *controlv1.SubscribeEventsResponse_Event:
		printEvent(value.Event)
	}
}

func printEvent(ev *controlv1.Event) {
	label, detail := describe(ev)
	fmt.Printf("%06d %-22s %s\n", ev.GetSeq(), label, detail)
}

func describe(ev *controlv1.Event) (label, detail string) {
	switch payload := ev.GetPayload().(type) {
	case *controlv1.Event_UserMessageAdded:
		return "user.message", payload.UserMessageAdded.GetText()

	case *controlv1.Event_RunStateChanged:
		status := strings.ToLower(strings.TrimPrefix(payload.RunStateChanged.GetStatus().String(), "RUN_STATUS_"))
		return "run." + status, payload.RunStateChanged.GetReason()

	case *controlv1.Event_AssistantTextDelta:
		return "assistant.delta", payload.AssistantTextDelta.GetText()

	case *controlv1.Event_AssistantMessageCompleted:
		reason := payload.AssistantMessageCompleted.GetStopReason()
		if reason == controlv1.StopReason_STOP_REASON_END_TURN ||
			reason == controlv1.StopReason_STOP_REASON_UNSPECIFIED {
			return "assistant.completed", ""
		}
		// A truncated or filtered answer must not look like a normal finish.
		return "assistant.completed", strings.ToLower(strings.TrimPrefix(reason.String(), "STOP_REASON_"))

	case *controlv1.Event_UsageChanged:
		usage := payload.UsageChanged.GetUsage()
		return "usage", fmt.Sprintf("in=%d out=%d cached=%d reasoning=%d",
			usage.GetInputTokens(), usage.GetOutputTokens(),
			usage.GetCachedInputTokens(), usage.GetReasoningTokens())

	default:
		return "unknown", ""
	}
}

func newMeta() *controlv1.RequestMeta {
	return &controlv1.RequestMeta{
		ClientId: clientName,
		// One request ID per logical mutation. Reusing it on a retry is what
		// lets the daemon deduplicate a request whose response was lost.
		RequestId: fmt.Sprintf("%s-%d", clientName, time.Now().UnixNano()),
	}
}

// dial reads the daemon's discovery file and returns an authenticated client.
func dial() (controlv1connect.SessionServiceClient, error) {
	path, err := discovery.Path()
	if err != nil {
		return nil, err
	}

	d, err := discovery.Read(path)
	if err != nil {
		return nil, fmt.Errorf("%w (is agentd running?)", err)
	}

	httpClient := &http.Client{
		Transport: &bearerTransport{token: d.Token, base: http.DefaultTransport},
	}

	return controlv1connect.NewSessionServiceClient(httpClient, d.BaseURL), nil
}

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone rather than mutate: RoundTrippers must not modify the request they
	// are given.
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}

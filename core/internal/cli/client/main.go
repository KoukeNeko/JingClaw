// Package client is the command-line client: what a person or a script uses
// to look at a daemon and decide things.
//
// It is a control-plane client and nothing more: it holds no runtime, no
// provider and no state. Everything it shows comes from the daemon's event
// stream, which is what keeps the CLI, the GUIs and the web UI looking at one
// consistent world instead of three divergent ones.
//
// This binary must never import internal/runtime. A test enforces it.
package client

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/discovery"
)

const clientName = "jingclaw-cli"

// runtimeDir is where the daemon publishes itself, taken from the same
// configuration file the daemon reads. It is process-wide state, which a
// larger CLI would thread through a context instead; here every command needs
// exactly this one value and nothing else from the file.
var runtimeDir string

// AddTo gives a parent command everything a client needs: the subcommands,
// the --config flag they share, and the step that reads it before any of them
// runs.
//
// All three together, because they are one thing. A parent that adopted the
// commands without the flag would offer every one of them and have none of
// them able to find a daemon that was told to publish itself somewhere else.
//
// The read-the-config step goes on each adopted command rather than on the
// parent. Cobra runs the nearest one it finds walking up from whatever was
// invoked, so a step left on a shared parent also runs for that parent's other
// children — and the daemon, which is one of them, would then have its
// configuration read twice: once here from the default location, before its
// own --config was ever looked at.
func AddTo(parent *cobra.Command) {
	client := newRootCommand()

	parent.PersistentFlags().AddFlagSet(client.PersistentFlags())
	adopted := client.Commands()
	for _, command := range adopted {
		command.PersistentPreRunE = client.PersistentPreRunE
	}
	parent.AddCommand(adopted...)
}

func newRootCommand() *cobra.Command {
	var configPath string

	root := &cobra.Command{
		Use:           "agent",
		Short:         "Control the JingClaw agent daemon",
		SilenceUsage:  true,
		SilenceErrors: true,
		// The daemon can be told to publish itself somewhere other than the
		// platform default, and a client that does not read the same file
		// would then be unable to find it.
		//
		// Only the location is taken. Validate is deliberately not called: the
		// checks it makes are about running a daemon, and refusing to let an
		// operator interrupt a run because some unrelated setting is out of
		// range would be its own kind of failure.
		PersistentPreRunE: func(*cobra.Command, []string) error {
			cfg, _, err := config.Load(configPath)
			if err != nil {
				return err
			}
			runtimeDir = cfg.Server.RuntimeDir
			return nil
		},
	}

	root.PersistentFlags().StringVar(&configPath, "config", "",
		"configuration file; defaults to the one in the config directory")

	root.AddCommand(newSessionCommand(), newSendCommand(), newAttachCommand(), newInterruptCommand(),
		newApprovalsCommand(), newApproveCommand(), newDenyCommand(), newBindingsCommand(),
		newArtifactCommand(), newMemoryCommand(),
		newQuestionsCommand(), newAnswerCommand(), newProcessesCommand(),
		newMCPCommand(&configPath), newScheduleCommand(), newSkillsCommand())
	return root
}

// newSessionModelCommand shows and changes which model answers in a session.
//
// Per session rather than per daemon because that is what a local deployment
// needs: the small model that fits in memory for most of what gets asked, and
// the large one for the conversation that actually needs it.
func newSessionModelCommand() *cobra.Command {
	model := &cobra.Command{
		Use:   "model <session-id> [model]",
		Short: "Show or set which model answers in a session",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dial()
			if err != nil {
				return err
			}

			if len(args) == 2 {
				resp, err := client.SetSessionModel(cmd.Context(),
					connect.NewRequest(&controlv1.SetSessionModelRequest{
						Meta: newMeta(), SessionId: args[0], Model: args[1],
					}))
				if err != nil {
					return err
				}

				chosen := resp.Msg.GetSession().GetModel()
				if chosen == "" {
					fmt.Fprintln(cmd.ErrOrStderr(), "back to the configured model")
					return nil
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "this session now answers with %s\n", chosen)
				return nil
			}

			resp, err := client.ListModels(cmd.Context(),
				connect.NewRequest(&controlv1.ListModelsRequest{
					Meta: newMeta(), SessionId: args[0],
				}))
			if err != nil {
				return err
			}

			// Marked rather than merely listed: a list of twenty names with no
			// indication of which is in use answers a different question from
			// the one that was asked.
			for _, one := range resp.Msg.GetModels() {
				mark := "  "
				if one.GetId() == resp.Msg.GetCurrent() {
					mark = "* "
				}
				fmt.Printf("%s%s", mark, one.GetId())
				if window := one.GetContextWindow(); window > 0 {
					fmt.Printf("  %s context (%s)", formatCount(window), one.GetContextSource())
				}
				fmt.Println()
			}

			fmt.Fprintf(cmd.ErrOrStderr(), "provider %s; the daemon's default is %s\n",
				resp.Msg.GetProvider(), resp.Msg.GetDefault())
			return nil
		},
	}

	return model
}

// formatCount keeps a context window readable. The exact figure matters to
// nobody choosing a model; the order of magnitude is the whole message.
func formatCount(count int64) string {
	switch {
	case count >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(count)/1_000_000)
	case count >= 1_000:
		return fmt.Sprintf("%dk", count/1_000)
	default:
		return fmt.Sprintf("%d", count)
	}
}

func newSessionCommand() *cobra.Command {
	session := &cobra.Command{Use: "session", Short: "Manage sessions"}

	var title string
	list := &cobra.Command{
		Use:   "list",
		Short: "List sessions, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := dial()
			if err != nil {
				return err
			}

			resp, err := client.ListSessions(cmd.Context(),
				connect.NewRequest(&controlv1.ListSessionsRequest{Meta: newMeta()}))
			if err != nil {
				return err
			}

			sessions := resp.Msg.GetSessions()
			if len(sessions) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no sessions yet")
				return nil
			}

			for _, session := range sessions {
				title := session.GetTitle()
				if title == "" {
					title = "(untitled)"
				}
				fmt.Printf("%s  %s  %s\n",
					session.GetId(),
					session.GetUpdatedAt().AsTime().Local().Format("2006-01-02 15:04"),
					title)
			}
			return nil
		},
	}

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

	session.AddCommand(create, list, newSessionModelCommand())
	return session
}

func newSendCommand() *cobra.Command {
	var attach []string

	cmd := &cobra.Command{
		Use:   "send <session-id> <text>",
		Short: "Send a user turn and print the run ID",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dial()
			if err != nil {
				return err
			}

			attachments, err := readAttachments(attach)
			if err != nil {
				return err
			}

			resp, err := client.SendTurn(cmd.Context(), connect.NewRequest(&controlv1.SendTurnRequest{
				Meta:        newMeta(),
				SessionId:   args[0],
				Text:        args[1],
				Attachments: attachments,
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

	cmd.Flags().StringSliceVar(&attach, "attach", nil,
		"an image to send with the turn; may be repeated")

	return cmd
}

func newAttachCommand() *cobra.Command {
	var (
		afterSeq   uint64
		showOutput bool
	)

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
				printFrame(stream.Msg(), showOutput)
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
	cmd.Flags().BoolVar(&showOutput, "output", false,
		"print what every tool returned, not only the ones that failed")
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

// readAttachments loads the files a turn is being sent with.
//
// The bytes travel to the daemon rather than the path, because the daemon may
// be on another machine reached through a forwarded port, where a path names a
// file it cannot see.
func readAttachments(paths []string) ([]*controlv1.InlineAttachment, error) {
	if len(paths) == 0 {
		return nil, nil
	}

	attachments := make([]*controlv1.InlineAttachment, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}

		// What it is, guessed from the name. The daemon checks the bytes and
		// refuses if the two disagree, so this is a label rather than a claim.
		attachments = append(attachments, &controlv1.InlineAttachment{
			Name:      filepath.Base(path),
			MediaType: mime.TypeByExtension(filepath.Ext(path)),
			Data:      data,
		})
	}

	return attachments, nil
}

func newApprovalsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "approvals <session-id>",
		Short: "List tool calls waiting for a decision",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dial()
			if err != nil {
				return err
			}

			resp, err := client.ListApprovals(cmd.Context(), connect.NewRequest(&controlv1.ListApprovalsRequest{
				SessionId: args[0],
			}))
			if err != nil {
				return err
			}

			approvals := resp.Msg.GetApprovals()
			if len(approvals) == 0 {
				fmt.Fprintln(os.Stderr, "nothing waiting")
				return nil
			}

			for _, approval := range approvals {
				fmt.Printf("%s  %s\n", approval.GetId(), approval.GetSummary())
				for _, effect := range approval.GetEffects() {
					fmt.Printf("    - %s\n", effect)
				}

				// The call as the daemon rendered it: a diff for an edit, the
				// command line for an execution. Indented rather than shown
				// raw so that a diff does not run into the next approval, and
				// only where there is one — most tools have nothing clearer
				// to show than their arguments.
				if preview := approval.GetPreview(); preview != "" {
					for line := range strings.SplitSeq(strings.TrimRight(preview, "\n"), "\n") {
						fmt.Printf("    %s\n", line)
					}
				}
			}
			return nil
		},
	}
}

func newApproveCommand() *cobra.Command {
	var session bool

	cmd := &cobra.Command{
		Use:   "approve <approval-id>",
		Short: "Allow a waiting tool call",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return decide(cmd, args[0], true, session)
		},
	}
	cmd.Flags().BoolVar(&session, "session", false,
		"allow this tool for the rest of the session, not just this call")

	return cmd
}

func newDenyCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "deny <approval-id>",
		Short: "Refuse a waiting tool call",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return decide(cmd, args[0], false, false)
		},
	}
}

func decide(cmd *cobra.Command, approvalID string, allow, forSession bool) error {
	client, err := dial()
	if err != nil {
		return err
	}

	remember := controlv1.RememberScope_REMEMBER_SCOPE_ONCE
	if forSession {
		remember = controlv1.RememberScope_REMEMBER_SCOPE_SESSION
	}

	decision := controlv1.ApprovalDecision_APPROVAL_DECISION_DENY
	if allow {
		decision = controlv1.ApprovalDecision_APPROVAL_DECISION_ALLOW
	}

	resp, err := client.DecideApproval(cmd.Context(), connect.NewRequest(&controlv1.DecideApprovalRequest{
		Meta:       newMeta(),
		ApprovalId: approvalID,
		Decision:   decision,
		Remember:   remember,
	}))
	if err != nil {
		return err
	}

	fmt.Println(strings.ToLower(strings.TrimPrefix(
		resp.Msg.GetApproval().GetStatus().String(), "APPROVAL_STATUS_")))
	return nil
}

func printFrame(frame *controlv1.SubscribeEventsResponse, showOutput bool) {
	switch value := frame.GetValue().(type) {
	case *controlv1.SubscribeEventsResponse_Hello:
		fmt.Fprintf(os.Stderr, "attached at seq %d\n", value.Hello.GetHeadSeq())

	case *controlv1.SubscribeEventsResponse_Heartbeat:
		// Liveness only; nothing for a human to read.

	case *controlv1.SubscribeEventsResponse_Event:
		printEvent(value.Event, showOutput)
	}
}

func printEvent(ev *controlv1.Event, showOutput bool) {
	label, detail := describe(ev, showOutput)
	fmt.Printf("%06d %-22s %s\n", ev.GetSeq(), label, detail)
}

func describe(ev *controlv1.Event, showOutput bool) (label, detail string) {
	switch payload := ev.GetPayload().(type) {
	case *controlv1.Event_UserMessageAdded:
		return "user.message", payload.UserMessageAdded.GetText()

	case *controlv1.Event_RunStateChanged:
		status := strings.ToLower(strings.TrimPrefix(payload.RunStateChanged.GetStatus().String(), "RUN_STATUS_"))
		return "run." + status, payload.RunStateChanged.GetReason()

	case *controlv1.Event_AssistantTextDelta:
		return "assistant.delta", payload.AssistantTextDelta.GetText()

	case *controlv1.Event_QuestionAsked:
		return "question", renderQuestionLine(payload.QuestionAsked)

	case *controlv1.Event_QuestionAnswered:
		return "answered", payload.QuestionAnswered.GetAnswer()

	case *controlv1.Event_PlanChanged:
		return "plan", renderPlanLine(payload.PlanChanged.GetItems())

	case *controlv1.Event_AssistantReasoningDelta:
		return "assistant.thinking", payload.AssistantReasoningDelta.GetText()

	case *controlv1.Event_AssistantMessageCompleted:
		reason := payload.AssistantMessageCompleted.GetStopReason()
		if reason == controlv1.StopReason_STOP_REASON_END_TURN ||
			reason == controlv1.StopReason_STOP_REASON_UNSPECIFIED {
			return "assistant.completed", ""
		}
		// A truncated or filtered answer must not look like a normal finish.
		return "assistant.completed", strings.ToLower(strings.TrimPrefix(reason.String(), "STOP_REASON_"))

	case *controlv1.Event_ToolCallRequested:
		call := payload.ToolCallRequested
		return "tool.requested", fmt.Sprintf("%s %s", call.GetName(), compact(call.GetArguments(), 120))

	case *controlv1.Event_ToolCallCompleted:
		done := payload.ToolCallCompleted
		status := done.GetSummary()
		if done.GetIsError() {
			status = "error"
		}
		// Naming the artifact is what makes a truncated line something a
		// person can follow up on rather than a note that output was lost.
		if stored := done.GetArtifact(); stored != nil {
			status += fmt.Sprintf(" [%d bytes kept as %s]", stored.GetSize(), stored.GetId())
		}
		line := fmt.Sprintf("%s (%dms) %s", done.GetName(), done.GetDurationMs(), status)

		// What it printed, below the line and indented.
		//
		// A failure is shown without being asked for, because a compiler
		// error compacted onto one line is a note that something went wrong
		// rather than something anybody can act on. A success is shown when
		// asked, since most of them are a file the caller already knows the
		// contents of.
		if output := toolOutput(done, showOutput); output != "" {
			line += "\n" + output
		}
		return "tool.completed", line

	case *controlv1.Event_ApprovalRequested:
		request := payload.ApprovalRequested
		detail := fmt.Sprintf("%s — %s", request.GetApprovalId(), request.GetSummary())
		if effects := request.GetEffects(); len(effects) > 0 {
			detail += " [" + strings.Join(effects, "; ") + "]"
		}
		return "approval.requested", detail

	case *controlv1.Event_ApprovalResolved:
		resolved := payload.ApprovalResolved
		return "approval." + strings.ToLower(strings.TrimPrefix(
				resolved.GetStatus().String(), "APPROVAL_STATUS_")),
			fmt.Sprintf("%s by %s", resolved.GetToolName(), describeOrigin(resolved.GetDecidedBy()))

	case *controlv1.Event_UsageChanged:
		usage := payload.UsageChanged.GetUsage()
		return "usage", fmt.Sprintf("in=%d out=%d cached=%d reasoning=%d",
			usage.GetInputTokens(), usage.GetOutputTokens(),
			usage.GetCachedInputTokens(), usage.GetReasoningTokens())

	case *controlv1.Event_RunDirections:
		// The text itself is usually long and the same every run; what is
		// worth a line is that the run had some.
		return "run.directions", fmt.Sprintf("%d bytes of standing directions",
			len(payload.RunDirections.GetText()))

	case *controlv1.Event_ConversationCompacted:
		// Worth showing rather than hiding. Somebody watching a session lose
		// its memory of the last hour should be told, not left to infer it
		// from the model suddenly asking what the file was called.
		compacted := payload.ConversationCompacted
		return "conversation.compacted", fmt.Sprintf(
			"folded %d messages, ~%d tokens to ~%d",
			compacted.GetMessagesFolded(), compacted.GetTokensBefore(), compacted.GetTokensAfter())

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

// dialChannels returns a client for binding management, which uses the same
// control credential: deciding which channels may reach a workspace is the
// operator's business, not the gateway's.
func dialChannels() (controlv1connect.ChannelServiceClient, error) {
	httpClient, baseURL, err := authenticated()
	if err != nil {
		return nil, err
	}
	return controlv1connect.NewChannelServiceClient(httpClient, baseURL), nil
}

// newArtifactCommand fetches output the daemon kept because it was too large
// to put in front of the model.
//
// It writes bytes to stdout by default rather than pretty-printing them: what
// is in an artifact is a build log or a diff, and the useful thing to do with
// one is pipe it somewhere.
func newArtifactCommand() *cobra.Command {
	artifact := &cobra.Command{Use: "artifact", Short: "Read stored tool output"}

	var (
		out    string
		offset int64
		limit  int64
	)

	get := &cobra.Command{
		Use:   "get <artifact-id>",
		Short: "Write an artifact to stdout, or to a file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dialArtifacts()
			if err != nil {
				return err
			}

			stream, err := client.ReadArtifact(cmd.Context(),
				connect.NewRequest(&controlv1.ReadArtifactRequest{
					Meta:   newMeta(),
					Id:     args[0],
					Offset: offset,
					Limit:  limit,
				}))
			if err != nil {
				return err
			}
			defer func() { _ = stream.Close() }()

			destination := cmd.OutOrStdout()
			if out != "" {
				file, err := os.Create(out)
				if err != nil {
					return err
				}
				// Closed explicitly as well, so a write that fails on flush is
				// reported rather than discarded by a deferred close.
				defer func() { _ = file.Close() }()
				destination = file
			}

			for stream.Receive() {
				if _, err := destination.Write(stream.Msg().GetChunk()); err != nil {
					return err
				}
			}
			if err := stream.Err(); err != nil {
				return err
			}

			if file, ok := destination.(*os.File); ok && out != "" {
				if err := file.Close(); err != nil {
					return err
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s\n", out)
			}
			return nil
		},
	}

	get.Flags().StringVar(&out, "out", "", "write to this file instead of stdout")
	get.Flags().Int64Var(&offset, "offset", 0, "byte to start at")
	get.Flags().Int64Var(&limit, "limit", 0, "how many bytes to read; 0 reads to the end")

	artifact.AddCommand(get)
	return artifact
}

// newQuestionsCommand lists what the agent has asked and nobody has answered.
func newQuestionsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "questions <session-id>",
		Short: "List questions the agent is waiting on",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dial()
			if err != nil {
				return err
			}

			resp, err := client.ListQuestions(cmd.Context(),
				connect.NewRequest(&controlv1.ListQuestionsRequest{SessionId: args[0]}))
			if err != nil {
				return err
			}

			questions := resp.Msg.GetQuestions()
			if len(questions) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "nothing waiting")
				return nil
			}

			for _, question := range questions {
				fmt.Printf("%s  %s\n", question.GetId(), question.GetPrompt())
				for _, option := range question.GetOptions() {
					fmt.Printf("    %s  %s", option.GetId(), option.GetLabel())
					if detail := option.GetDetail(); detail != "" {
						fmt.Printf("  (%s)", detail)
					}
					fmt.Println()
				}
				if question.GetKind() == controlv1.QuestionKind_QUESTION_KIND_TEXT {
					fmt.Println("    (answer in your own words)")
				}
			}
			return nil
		},
	}
}

// newAnswerCommand unblocks a run that stopped to ask.
func newAnswerCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "answer <question-id> <answer>",
		Short: "Answer a question the agent is waiting on",
		// The answer is taken as the rest of the line rather than one
		// argument, so a person can type a sentence without quoting it.
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dial()
			if err != nil {
				return err
			}

			resp, err := client.AnswerQuestion(cmd.Context(),
				connect.NewRequest(&controlv1.AnswerQuestionRequest{
					Meta:       newMeta(),
					QuestionId: args[0],
					Answer:     strings.Join(args[1:], " "),
				}))
			if err != nil {
				return err
			}

			fmt.Fprintf(cmd.ErrOrStderr(), "answered %s: %s\n",
				resp.Msg.GetQuestion().GetId(), resp.Msg.GetQuestion().GetAnswer())
			return nil
		},
	}
}

// newMemoryCommand shows and removes what the agent believes.
//
// This is the control the research says is load-bearing: every assistant whose
// memory has been poisoned was poisoned invisibly, and a memory nobody can see
// is one nobody can correct.
func newMemoryCommand() *cobra.Command {
	memory := &cobra.Command{Use: "memory", Short: "See and remove what the agent remembers"}

	var history bool

	list := &cobra.Command{
		Use:   "list",
		Short: "List everything the agent believes, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := dialMemory()
			if err != nil {
				return err
			}

			resp, err := client.ListMemories(cmd.Context(),
				connect.NewRequest(&controlv1.ListMemoriesRequest{
					Meta:               newMeta(),
					IncludeInvalidated: history,
				}))
			if err != nil {
				return err
			}

			memories := resp.Msg.GetMemories()
			if len(memories) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "nothing has been remembered")
				return nil
			}

			for _, memory := range memories {
				printMemory(memory)
			}
			return nil
		},
	}
	list.Flags().BoolVar(&history, "history", false,
		"include memories that have been superseded")

	forget := &cobra.Command{
		Use:   "forget <memory-id>",
		Short: "Stop the agent believing something",
		Long: "Removes a memory, index and all: the agent will not recall it or\n" +
			"carry it again.\n\n" +
			"It does not erase the conversation the memory came from. That is\n" +
			"still in the event log, because an append-only log cannot forget —\n" +
			"which is the price of it being able to say what happened. The\n" +
			"listing shows which session each memory came from, so you know\n" +
			"where else to look.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dialMemory()
			if err != nil {
				return err
			}

			if _, err := client.ForgetMemory(cmd.Context(),
				connect.NewRequest(&controlv1.ForgetMemoryRequest{
					Meta: newMeta(),
					Id:   args[0],
				})); err != nil {
				return err
			}

			fmt.Fprintf(cmd.ErrOrStderr(),
				"forgot %s — the conversation it came from is still in the log\n", args[0])
			return nil
		},
	}

	memory.AddCommand(list, forget)
	return memory
}

// printMemory shows a memory with where it came from.
//
// The provenance is the point. "The agent believes X" is not something a
// person can act on; "the agent believes X because somebody on Discord said so
// in this session" is.
func printMemory(memory *controlv1.Memory) {
	fmt.Printf("%s  [%s · %s · %s]%s\n",
		memory.GetId(), memory.GetActivation(), memory.GetScope(), memory.GetScopeRef(),
		memoryState(memory))
	fmt.Printf("    %s\n", memory.GetText())

	from := "typed here"
	if memory.GetOrigin().GetKind() == controlv1.RunOriginKind_RUN_ORIGIN_KIND_GATEWAY {
		principal := memory.GetOrigin().GetPrincipal()
		from = fmt.Sprintf("%s account %s — from outside this machine",
			principal.GetPlatform(), principal.GetPrincipalId())
	}

	fmt.Printf("    %s · session %s at seq %d · approved by %s\n",
		from, memory.GetSourceSessionId(), memory.GetSourceSeq(), memory.GetApprovedBy())

	if until := memory.GetValidUntil(); until != nil {
		fmt.Printf("    %s\n", renderValidUntil(until.AsTime().Local()))
	}
	if used := memory.GetLastUsedAt(); used != nil {
		fmt.Printf("    last wanted %s\n", used.AsTime().Local().Format("2006-01-02"))
	}
	fmt.Println()
}

// renderValidUntil says when a fact stops holding, in the terms it was given.
//
// The stored instant is exclusive: a freeze that lifts on the fifteenth is in
// force through the fifteenth, so it is recorded as midnight on the
// sixteenth. Printing that instant back is correct and reads as though the
// person got the date wrong, so a whole-day window is shown as the last day
// it holds.
func renderValidUntil(at time.Time) string {
	if at.Hour() == 0 && at.Minute() == 0 && at.Second() == 0 {
		return "in force through " + at.AddDate(0, 0, -1).Format("2006-01-02")
	}
	return "stops being true at " + at.Format("2006-01-02 15:04")
}

// memoryState says why a memory is no longer believed.
//
// Corrected and expired are different things, and calling both "superseded"
// would have a person reading "somebody replaced this" when what happened is
// "nobody wanted it for three months". One of those is worth looking into.
func memoryState(memory *controlv1.Memory) string {
	if memory.GetInvalidatedAt() == nil {
		return ""
	}
	if memory.GetSupersededBy() != "" {
		return " (superseded by " + memory.GetSupersededBy() + ")"
	}
	if until := memory.GetValidUntil(); until != nil && !until.AsTime().After(time.Now()) {
		return " (stopped being true)"
	}
	return " (expired, unused)"
}

func dialMemory() (controlv1connect.MemoryServiceClient, error) {
	httpClient, baseURL, err := authenticated()
	if err != nil {
		return nil, err
	}
	return controlv1connect.NewMemoryServiceClient(httpClient, baseURL), nil
}

func dialArtifacts() (controlv1connect.ArtifactServiceClient, error) {
	httpClient, baseURL, err := authenticated()
	if err != nil {
		return nil, err
	}
	return controlv1connect.NewArtifactServiceClient(httpClient, baseURL), nil
}

// dial reads the daemon's discovery file and returns an authenticated client.
func dial() (controlv1connect.SessionServiceClient, error) {
	httpClient, baseURL, err := authenticated()
	if err != nil {
		return nil, err
	}
	return controlv1connect.NewSessionServiceClient(httpClient, baseURL), nil
}

func authenticated() (*http.Client, string, error) {
	return authenticatedIn(runtimeDir)
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

// compact renders a value on one line, bounded, so a tool argument or error
// cannot flood the terminal.
// toolOutput renders what a tool printed, indented under its line.
func toolOutput(done *controlv1.ToolCallCompleted, always bool) string {
	if !always && !done.GetIsError() {
		return ""
	}

	content := strings.TrimRight(done.GetContent(), "\n")
	if strings.TrimSpace(content) == "" {
		return ""
	}

	const maxLines = 40

	lines := strings.Split(content, "\n")
	// The end rather than the start: a command that failed says why on its
	// last lines, and a build log opens with the compiler announcing itself.
	cut := false
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
		cut = true
	}

	var out strings.Builder
	if cut {
		out.WriteString("       …\n")
	}
	for index, line := range lines {
		if index > 0 {
			out.WriteString("\n")
		}
		out.WriteString("       " + line)
	}
	return out.String()
}

func compact(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > limit {
		return value[:limit] + "…"
	}
	return value
}

// renderPlanLine is a plan on one line, for a stream of events.
//
// One line because this appears every time a step moves. The whole list every
// time would bury everything else the run is doing, and the thing worth
// knowing is how far along it is and what it is on.
func renderPlanLine(items []*controlv1.PlanItem) string {
	if len(items) == 0 {
		return "(empty)"
	}

	var done int
	current := ""
	for _, item := range items {
		switch item.GetStatus() {
		case controlv1.PlanStatus_PLAN_STATUS_COMPLETED,
			controlv1.PlanStatus_PLAN_STATUS_ABANDONED:
			done++
		case controlv1.PlanStatus_PLAN_STATUS_IN_PROGRESS:
			if current == "" {
				current = item.GetTitle()
			}
		}
	}

	line := fmt.Sprintf("%d/%d", done, len(items))
	if current != "" {
		line += "  " + current
	}
	return line
}

// renderQuestionLine is a question and how to answer it.
//
// The id is included because answering needs it, and somebody watching a
// stream is exactly the person about to answer.
func renderQuestionLine(asked *controlv1.QuestionAsked) string {
	line := fmt.Sprintf("%s — %s", asked.GetQuestionId(), asked.GetPrompt())

	options := asked.GetOptions()
	if len(options) == 0 {
		return line
	}

	labels := make([]string, 0, len(options))
	for _, option := range options {
		labels = append(labels, fmt.Sprintf("%s=%s", option.GetId(), option.GetLabel()))
	}
	return line + "  [" + strings.Join(labels, ", ") + "]"
}

// describeOrigin is the one-line form of who did something, for a listing.
//
// The name a person recognises when there is one, the identifier
// authorisation was actually checked against when there is not, and what kind
// of access it was when nobody was named at all. Never empty: a blank in a
// column headed by who decided something reads as a decision nobody made.
func describeOrigin(origin *controlv1.RunOrigin) string {
	if principal := origin.GetPrincipal(); principal != nil {
		if name := principal.GetDisplayName(); name != "" {
			return name
		}
		if id := principal.GetPrincipalId(); id != "" {
			return principal.GetPlatform() + ":" + id
		}
	}
	if client := origin.GetClientId(); client != "" {
		return client
	}
	if kind := origin.GetKind(); kind != controlv1.RunOriginKind_RUN_ORIGIN_KIND_UNSPECIFIED {
		return strings.ToLower(strings.TrimPrefix(kind.String(), "RUN_ORIGIN_KIND_"))
	}
	return "unrecorded"
}

// Dial returns a client for the daemon publishing itself in runtimeDir.
//
// Exported for the console, which is another client of the same daemon and
// has no reason to reimplement finding one. An explicit directory rather than
// the package variable the subcommands share: something that is not one of
// them has nothing to set it.
func Dial(where string) (controlv1connect.SessionServiceClient, error) {
	httpClient, baseURL, err := authenticatedIn(where)
	if err != nil {
		return nil, err
	}
	return controlv1connect.NewSessionServiceClient(httpClient, baseURL), nil
}

func authenticatedIn(where string) (*http.Client, string, error) {
	path, err := discovery.PathIn(where)
	if err != nil {
		return nil, "", err
	}

	found, err := discovery.Read(path)
	if err != nil {
		return nil, "", fmt.Errorf("%w (is jingclaw running?)", err)
	}

	return &http.Client{
		Transport: &bearerTransport{token: found.Token, base: http.DefaultTransport},
	}, found.BaseURL, nil
}

// newProcessesCommand lists what the agent has left running.
//
// The three process tools belong to the model, and until this there was no
// way for a person to see what they had started: a server put up an hour ago
// was visible only to the agent that put it there.
func newProcessesCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "processes <session-id>",
		Short: "List programs the agent has running",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dial()
			if err != nil {
				return err
			}

			resp, err := client.ListProcesses(cmd.Context(),
				connect.NewRequest(&controlv1.ListProcessesRequest{SessionId: args[0]}))
			if err != nil {
				return err
			}

			running := resp.Msg.GetProcesses()
			if len(running) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "nothing running")
				return nil
			}

			for _, one := range running {
				state := "running"
				if !one.GetRunning() {
					state = fmt.Sprintf("exit %d", one.GetExitCode())
				}
				fmt.Printf("%s  pid %-7d %-10s %s %s\n",
					one.GetId(), one.GetPid(), state,
					one.GetProgram(), strings.Join(one.GetArgs(), " "))
			}
			return nil
		},
	}
}

// Package console is the terminal you get when you open JingClaw.
//
// One scrolling log of everything the agent is doing, and one line at the
// bottom to type at — the shape a game server has. Conversation happens in a
// chat channel; this is where you watch it and answer it when it stops to
// ask.
package console

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/internal/cli/client"
	"github.com/KoukeNeko/JingClaw/core/internal/console"
	"github.com/KoukeNeko/JingClaw/core/internal/control"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// clientName is how the console identifies itself to the daemon.
//
// Its own name rather than the CLI's: what decides something is recorded, and
// two ways in that answer to one name cannot be told apart afterwards.
const clientName = "jingclaw-console"

// Leaving says what happens to the agent when the console closes, which is
// not the same in the two places a console is opened from.
type Leaving int

const (
	// LeavesItRunning is a console attached to a daemon somebody else
	// started. Closing it is closing a window.
	LeavesItRunning Leaving = iota

	// StopsIt is the console that comes with starting everything. Whoever
	// started the parts stops them, so closing this closes them too.
	StopsIt
)

// Run shows the log and takes commands until told to stop.
//
// runtimeDir is where the daemon publishes itself. The console is a client
// like any other and finds it the same way.
func Run(ctx context.Context, runtimeDir string, leaving Leaving) error {
	daemon, artifacts, err := client.DialForConsole(runtimeDir)
	if err != nil {
		return err
	}

	// Raw mode, so a keystroke arrives as it is pressed rather than a line at
	// a time. Restored on the way out however that happens: a terminal left
	// in raw mode is one where the shell no longer echoes, and the person it
	// happens to has no obvious way back.
	restore, err := rawMode()
	if err != nil {
		return err
	}
	defer restore()

	// Asked each time rather than remembered: a window can be resized while
	// this is running, and a stale width means the input line is erased by
	// the wrong number of rows.
	screen := console.NewScreen(os.Stdout, func() int {
		width, _, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil {
			return 0
		}
		return width
	})
	defer screen.Close()

	session := &session{
		daemon:    daemon,
		artifacts: artifacts,
		screen:    screen,
		leaving:   leaving,
		opener:    console.TheMachine{},

		// Its own directory under the system temporary one, so what the
		// console wrote out is together when somebody goes looking, and is
		// not read by the next thing to look in there.
		into: filepath.Join(os.TempDir(), "jingclaw-output"),
	}

	ctx, stop := context.WithCancel(ctx)
	defer stop()

	var running sync.WaitGroup
	running.Add(1)
	go func() {
		defer running.Done()
		session.follow(ctx)
	}()

	screen.Log("JingClaw. Type help for what you can do here, " + session.leavingMeans() + ".")
	screen.Prompt()

	err = session.read(ctx)
	stop()
	running.Wait()
	return err
}

// rawMode puts the terminal into raw mode and returns how to undo it.
func rawMode() (func(), error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil, errors.New("console: standard input is not a terminal")
	}

	was, err := term.MakeRaw(fd)
	if err != nil {
		return nil, fmt.Errorf("console: take the terminal: %w", err)
	}
	return func() { _ = term.Restore(fd, was) }, nil
}

// session is one run of the console.
type session struct {
	daemon    controlv1connectSessionService
	artifacts artifactReader
	screen    *console.Screen
	leaving   Leaving

	// opener and into are how stored output reaches something that can read
	// it, and where it is written on the way. Held so a check can watch what
	// would be opened without a reader being launched.
	opener console.Opener
	into   string

	mu      sync.Mutex
	focused domain.SessionID
	history []string

	// stored is the last output a call left behind, so `open` with no
	// argument means the one just mentioned in the log rather than nothing.
	stored storedOutput
}

// storedOutput is an artifact the console has seen go past.
type storedOutput struct {
	id        string
	mediaType string
}

// artifactReader is the part of the artifact service the console uses.
type artifactReader interface {
	ReadArtifact(
		context.Context, *connect.Request[controlv1.ReadArtifactRequest],
	) (*connect.ServerStreamForClient[controlv1.ReadArtifactResponse], error)
}

// controlv1connectSessionService is what the console needs of the daemon.
//
// An interface rather than the generated client, so the parts below can be
// tested without one.
type controlv1connectSessionService interface {
	SubscribeAllEvents(
		context.Context, *connect.Request[controlv1.SubscribeAllEventsRequest],
	) (*connect.ServerStreamForClient[controlv1.SubscribeAllEventsResponse], error)
	ListSessions(
		context.Context, *connect.Request[controlv1.ListSessionsRequest],
	) (*connect.Response[controlv1.ListSessionsResponse], error)
	ListRuns(
		context.Context, *connect.Request[controlv1.ListRunsRequest],
	) (*connect.Response[controlv1.ListRunsResponse], error)
	ListApprovals(
		context.Context, *connect.Request[controlv1.ListApprovalsRequest],
	) (*connect.Response[controlv1.ListApprovalsResponse], error)
	DecideApproval(
		context.Context, *connect.Request[controlv1.DecideApprovalRequest],
	) (*connect.Response[controlv1.DecideApprovalResponse], error)
	ListQuestions(
		context.Context, *connect.Request[controlv1.ListQuestionsRequest],
	) (*connect.Response[controlv1.ListQuestionsResponse], error)
	ListProcesses(
		context.Context, *connect.Request[controlv1.ListProcessesRequest],
	) (*connect.Response[controlv1.ListProcessesResponse], error)
	AnswerQuestion(
		context.Context, *connect.Request[controlv1.AnswerQuestionRequest],
	) (*connect.Response[controlv1.AnswerQuestionResponse], error)
	InterruptRun(
		context.Context, *connect.Request[controlv1.InterruptRunRequest],
	) (*connect.Response[controlv1.InterruptRunResponse], error)
}

// follow prints the log until the context ends.
func (s *session) follow(ctx context.Context) {
	// Where the log ends is not known until the daemon says so, and it says
	// so first. Until then this asks from the beginning; what it draws is
	// decided below, once the hello has arrived.
	cursor := uint64(0)
	attached := false

	// Paced, and said once. Without this a daemon that went away is a
	// terminal filling with one sentence as fast as the machine can print
	// it, which hides whatever somebody was reading and says nothing the
	// first line did not.
	var retries console.Retries

	for ctx.Err() == nil {
		stream, err := s.daemon.SubscribeAllEvents(ctx, connect.NewRequest(
			&controlv1.SubscribeAllEventsRequest{AfterCursor: cursor, ClientId: clientName},
		))
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.screen.Log("lost the daemon: " + err.Error())
			return
		}

		read := false
		for stream.Receive() {
			read = true
			switch frame := stream.Msg().GetValue().(type) {
			case *controlv1.SubscribeAllEventsResponse_Hello:
				// Only on the way in. A reconnection resumes from where this
				// console got to, or it would skip whatever happened while it
				// was away — which is the gap it reconnects to close.
				if !attached {
					attached = true
					cursor = console.AttachFrom(frame.Hello.GetHeadCursor())
				}

			case *controlv1.SubscribeAllEventsResponse_Event:
				if frame.Event.GetGlobalSeq() <= cursor {
					// Older than where this console started. Asked for
					// because the cursor is not known until the hello, and
					// skipped rather than drawn: a console opened to see what
					// is happening now must not spend the first screenful on
					// last week.
					continue
				}
				cursor = frame.Event.GetGlobalSeq()
				s.show(frame.Event)

			case *controlv1.SubscribeAllEventsResponse_ResyncRequired:
				// Said rather than swallowed. The log has moved on past where
				// this console was, and pretending otherwise would leave a
				// gap nobody could see.
				s.screen.Log(fmt.Sprintf(
					"missed events that have since been discarded; showing from %d",
					frame.ResyncRequired.GetOldestSeq()))
				cursor = frame.ResyncRequired.GetOldestSeq() - 1
			}
		}

		// Said before reconnecting. A stream that ends mid-way is retried
		// from the same cursor, so whatever stopped it stops it again — and
		// without this the console sits there looking connected, redrawing a
		// prompt, while every event after that one is never shown.
		if read {
			// Something got through, so the next outage is a new fact and is
			// said, and waited on from the start rather than at whatever
			// interval the last one had backed off to.
			retries.Worked()
		}

		_ = stream.Close()
		if ctx.Err() != nil {
			return
		}

		wait := time.Duration(0)
		if err := stream.Err(); err != nil && ctx.Err() == nil {
			var say string
			say, wait = retries.Failed("the event stream stopped: " + err.Error())
			if say != "" {
				s.screen.Log(say)
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// show prints one event, unless the console is focused elsewhere.
func (s *session) show(event *controlv1.Event) {
	s.mu.Lock()
	focused := s.focused
	s.mu.Unlock()

	if focused != "" && domain.SessionID(event.GetSessionId()) != focused {
		return
	}

	read, err := control.EventFromProto(event)
	if err != nil {
		// Something the daemon sent that this console does not know. Said
		// once rather than dropped: a console that silently ignores what it
		// does not recognise is one where a new kind of event is invisible.
		s.screen.Log("an event this console does not understand: " + err.Error())
		return
	}

	// Noted before it is drawn, so `open` with no argument means the output
	// the line being read just mentioned.
	s.noteStoredOutput(read)

	line, ok := console.Describe(read)
	if !ok {
		return
	}
	s.screen.Log(line.String())
}

// noteStoredOutput keeps the last stored output that went past.
func (s *session) noteStoredOutput(event domain.Event) {
	completed, isCompleted := event.Payload.(domain.ToolCallCompleted)
	if !isCompleted || completed.Artifact == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.stored = storedOutput{
		id: completed.Artifact.ID, mediaType: completed.Artifact.MediaType,
	}
}

// lastStored is the output `open` means when it is given no argument.
func (s *session) lastStored() storedOutput {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stored
}

// read takes keystrokes until the console is told to leave.
func (s *session) read(ctx context.Context) error {
	keys := make([]byte, 256)
	recalled := 0

	for {
		read, err := os.Stdin.Read(keys)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return nil
		}

		for at := 0; at < read; at++ {
			switch key := keys[at]; key {
			case '\r', '\n':
				line := s.screen.Take()
				if line == "" {
					continue
				}
				s.remember(line)
				recalled = 0
				if leave := s.run(ctx, line); leave {
					return nil
				}

			case 0x7f, 0x08: // backspace
				s.screen.Backspace()

			case 0x03: // ctrl-c
				return nil

			case 0x04: // ctrl-d, on an empty line
				if s.screen.Editing() == "" {
					return nil
				}

			case 0x1b: // an escape sequence: arrow keys arrive as three bytes
				if at+2 < read && keys[at+1] == '[' {
					switch keys[at+2] {
					case 'A':
						recalled = s.recall(recalled + 1)
					case 'B':
						recalled = s.recall(recalled - 1)
					case 'C':
						s.screen.Right()
					case 'D':
						s.screen.Left()
					}
					at += 2
				}

			default:
				if key >= 0x20 {
					s.screen.Insert(rune(key))
				}
			}
		}
	}
}

// remember keeps a command for the up arrow.
func (s *session) remember(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Not the same thing twice running: holding down return should not fill
	// the history with one command.
	if len(s.history) > 0 && s.history[len(s.history)-1] == line {
		return
	}
	s.history = append(s.history, line)
}

// recall puts the nth most recent command back on the input line.
func (s *session) recall(n int) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	if n <= 0 {
		s.screen.Set("")
		return 0
	}
	if n > len(s.history) {
		n = len(s.history)
	}
	if n == 0 {
		return 0
	}
	s.screen.Set(s.history[len(s.history)-n])
	return n
}

// say writes a reply to the person who typed, which is the terminal.
func (s *session) say(text string) {
	for _, line := range strings.Split(text, "\n") {
		s.screen.Log(line)
	}
}

// Commands are the subcommand for opening a console on a daemon already
// running, as against the one that starts everything.
func Commands() []*cobra.Command {
	var runtimeDir string

	command := &cobra.Command{
		Use:   "console",
		Short: "Watch what the agent is doing, and answer it",
		Long: "Watch what the agent is doing, and answer it.\n\n" +
			"One scrolling log of every session and one line to type at.\n" +
			"Leaving the console does not stop anything.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return Run(cmd.Context(), runtimeDir, LeavesItRunning)
		},
	}
	command.Flags().StringVar(&runtimeDir, "runtime-dir", "",
		"where the daemon publishes itself; defaults to this deployment's")

	return []*cobra.Command{command}
}

// leavingMeans is what quit does, which differs by where the console was
// opened from and so cannot be a constant in the greeting.
func (s *session) leavingMeans() string {
	if s.leaving == StopsIt {
		return "quit to stop it"
	}
	return "quit to leave it running"
}

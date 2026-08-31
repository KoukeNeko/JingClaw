package runtime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/event"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/tool/builtin"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

// scriptedProvider replays a fixed sequence of turns, so the tool loop can be
// tested without a model deciding to do something different each run. Each
// turn also captures the request it was given, which is how the conversation
// the runtime rebuilt gets asserted.
type scriptedProvider struct {
	turns    [][]provider.Event
	requests []provider.Request
	next     int
}

func (p *scriptedProvider) Name() string { return "scripted" }

func (p *scriptedProvider) Models(context.Context) ([]provider.ModelInfo, error) { return nil, nil }

func (p *scriptedProvider) Generate(_ context.Context, req provider.Request) (provider.Stream, error) {
	p.requests = append(p.requests, req)

	if p.next >= len(p.turns) {
		return nil, fmt.Errorf("scripted provider ran out of turns after %d calls", p.next)
	}

	events := p.turns[p.next]
	p.next++
	return &scriptedStream{events: events}, nil
}

type scriptedStream struct {
	events []provider.Event
	index  int
}

func (s *scriptedStream) Recv(context.Context) (provider.Event, error) {
	if s.index >= len(s.events) {
		return nil, io.EOF
	}
	ev := s.events[s.index]
	s.index++
	return ev, nil
}

func (s *scriptedStream) Close() error { return nil }

func toolCall(id, name string, args map[string]any) provider.ToolCallRequested {
	encoded, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	return provider.ToolCallRequested{ID: id, Name: name, Args: encoded}
}

func newToolHarness(
	t *testing.T, turns [][]provider.Event,
) (*runtime.Runtime, *memory.Store, *scriptedProvider, *tool.Registry) {
	t.Helper()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "main.go"),
		[]byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	ws, err := workspace.Open(root)
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}

	observed := builtin.NewObserver()
	locks := builtin.NewFileLocks()

	registry := tool.NewRegistry()
	registry.MustRegister(
		&builtin.ReadFile{Workspace: ws, Observer: observed},
		&builtin.GlobFiles{Workspace: ws},
		&builtin.Grep{Workspace: ws},
		builtin.NewWriteFile(ws, observed, locks),
		builtin.NewEditFile(ws, observed, locks),
	)

	var counter atomic.Uint64
	next := func(prefix string) runtime.IDGenerator {
		return func() string { return fmt.Sprintf("%s_%d", prefix, counter.Add(1)) }
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store := memory.New()
	scripted := &scriptedProvider{turns: turns}

	rt := runtime.New(ctx, runtime.Options{
		Store:         store,
		Hub:           event.NewHub(),
		Provider:      scripted,
		Model:         "scripted",
		Tools:         registry,
		MaxIterations: 5,
		NewSessionID:  next("ses"),
		NewRunID:      next("run"),
		NewMessageID:  next("msg"),
		NewEventID:    next("evt"),
		NewApprovalID: next("apr"),
		NewPlanItemID: next("todo"),
		NewQuestionID: next("qst"),
		NewScheduleID: next("sch"),
		Now:           func() time.Time { return time.Unix(0, 0).UTC() },
		Logger:        slog.New(slog.DiscardHandler),
	})

	return rt, store, scripted, registry
}

func runTurn(t *testing.T, rt *runtime.Runtime, sessionID domain.SessionID, text string) domain.RunID {
	t.Helper()

	runID, _, err := rt.SendTurn(context.Background(), sessionID, text, domain.RunOrigin{})
	if err != nil {
		t.Fatalf("send turn: %v", err)
	}
	waitForRun(t, rt, runID)
	return runID
}

func eventKinds(t *testing.T, store *memory.Store, sessionID domain.SessionID) []domain.EventKind {
	t.Helper()

	events, err := store.ListAfter(context.Background(), sessionID, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}

	kinds := make([]domain.EventKind, 0, len(events))
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
	}
	return kinds
}

func TestToolLoopExecutesAndFeedsResultsBack(t *testing.T) {
	rt, store, scripted, _ := newToolHarness(t, [][]provider.Event{
		{toolCall("call_1", "glob_files", map[string]any{"pattern": "**/*.go"})},
		{provider.TextDelta{Text: "Found src/main.go."}, provider.Completed{StopReason: domain.StopEndTurn}},
	})
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "tools")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runID := runTurn(t, rt, session.ID, "Which Go files exist?")

	run, err := rt.Run(ctx, runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.Status != domain.RunCompleted {
		t.Fatalf("status %q, want %q", run.Status, domain.RunCompleted)
	}

	// The loop must record the request before the result, so a client can show
	// work in progress and a crash leaves an unfinished call visible.
	want := []domain.EventKind{
		domain.EventUserMessageAdded,
		domain.EventRunStateChanged,
		domain.EventToolCallRequested,
		domain.EventAssistantMessageCompleted,
		domain.EventToolCallCompleted,
		domain.EventAssistantTextDelta,
		domain.EventAssistantMessageCompleted,
		domain.EventRunStateChanged,
	}
	got := eventKinds(t, store, session.ID)

	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d: got %q, want %q", i, got[i], want[i])
		}
	}

	// The second turn must carry the observation, or the model is being asked
	// to answer a question it never received an answer to.
	if len(scripted.requests) != 2 {
		t.Fatalf("provider called %d times, want 2", len(scripted.requests))
	}
	if !conversationHasToolResult(scripted.requests[1], "call_1", "src/main.go") {
		t.Errorf("the tool result never reached the model:\n%s", describe(scripted.requests[1]))
	}
}

// A failed tool is an observation. The model has to see the error to do
// something different, so the loop must continue rather than abort.
func TestToolErrorsAreFedBackToTheModel(t *testing.T) {
	rt, store, scripted, _ := newToolHarness(t, [][]provider.Event{
		{toolCall("call_1", "read_file", map[string]any{"path": "../../etc/passwd"})},
		{toolCall("call_2", "read_file", map[string]any{"path": "src/main.go"})},
		{provider.TextDelta{Text: "Read it."}, provider.Completed{StopReason: domain.StopEndTurn}},
	})
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "tool errors")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runID := runTurn(t, rt, session.ID, "Read the passwd file")

	run, err := rt.Run(ctx, runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.Status != domain.RunCompleted {
		t.Errorf("a tool error ended the run as %q; it should be recoverable", run.Status)
	}

	events, err := store.ListAfter(ctx, session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}

	var sawError bool
	for _, ev := range events {
		completed, ok := ev.Payload.(domain.ToolCallCompleted)
		if !ok || !completed.IsError {
			continue
		}
		sawError = true
		if !strings.Contains(completed.Content, "outside_workspace") {
			t.Errorf("the error does not say what was wrong: %s", completed.Content)
		}
	}
	if !sawError {
		t.Fatal("the escape attempt was not recorded as a failed tool call")
	}

	if len(scripted.requests) < 2 || !conversationHasToolResult(scripted.requests[1], "call_1", "outside_workspace") {
		t.Error("the model never saw why its first call failed")
	}
}

// A model that keeps calling tools without converging has to be stopped, and
// the reason has to be recorded: a run that simply ends looks identical to one
// that finished.
func TestToolLoopStopsAtTheIterationBudget(t *testing.T) {
	turns := make([][]provider.Event, 8)
	for i := range turns {
		turns[i] = []provider.Event{
			toolCall(fmt.Sprintf("call_%d", i), "glob_files", map[string]any{"pattern": "**/*.go"}),
		}
	}

	rt, store, _, _ := newToolHarness(t, turns)
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "runaway")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runID := runTurn(t, rt, session.ID, "Loop forever")

	run, err := rt.Run(ctx, runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.Status != domain.RunFailed {
		t.Errorf("status %q, want %q", run.Status, domain.RunFailed)
	}

	events, err := store.ListAfter(ctx, session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}

	last, ok := events[len(events)-1].Payload.(domain.RunStateChanged)
	if !ok {
		t.Fatalf("last event is %T", events[len(events)-1].Payload)
	}
	if !strings.Contains(last.Reason, "model turns") {
		t.Errorf("the budget stop is not explained: %q", last.Reason)
	}
}

// History lives in the event log, so a second turn must see the first without
// anything being held in memory between runs.
func TestSecondTurnSeesEarlierConversation(t *testing.T) {
	rt, _, scripted, _ := newToolHarness(t, [][]provider.Event{
		{provider.TextDelta{Text: "The file is src/main.go."}, provider.Completed{StopReason: domain.StopEndTurn}},
		{provider.TextDelta{Text: "It defines main."}, provider.Completed{StopReason: domain.StopEndTurn}},
	})
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "history")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runTurn(t, rt, session.ID, "Which file holds main?")
	runTurn(t, rt, session.ID, "What does it define?")

	if len(scripted.requests) != 2 {
		t.Fatalf("provider called %d times, want 2", len(scripted.requests))
	}

	second := scripted.requests[1]
	if len(second.Messages) < 3 {
		t.Fatalf("second turn carried %d messages, want the earlier exchange too:\n%s",
			len(second.Messages), describe(second))
	}

	transcript := describe(second)
	for _, want := range []string{"Which file holds main?", "The file is src/main.go.", "What does it define?"} {
		if !strings.Contains(transcript, want) {
			t.Errorf("history is missing %q:\n%s", want, transcript)
		}
	}
}

// The prompt prefix has to be byte-identical between requests for prompt
// caching to be possible at all.
func TestToolDeclarationsAreStableAcrossTurns(t *testing.T) {
	rt, _, scripted, _ := newToolHarness(t, [][]provider.Event{
		{provider.Completed{StopReason: domain.StopEndTurn}},
		{provider.Completed{StopReason: domain.StopEndTurn}},
	})
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "stable")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runTurn(t, rt, session.ID, "one")
	runTurn(t, rt, session.ID, "two")

	first, second := scripted.requests[0].Tools, scripted.requests[1].Tools
	if len(first) == 0 {
		t.Fatal("no tools were declared to the model")
	}
	if len(first) != len(second) {
		t.Fatalf("declared %d tools then %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Name != second[i].Name {
			t.Errorf("tool order changed: %s then %s", first[i].Name, second[i].Name)
		}
	}
}

func conversationHasToolResult(req provider.Request, callID, substring string) bool {
	for _, message := range req.Messages {
		for _, block := range message.Content {
			result, ok := block.(provider.ToolResultBlock)
			if ok && result.ToolUseID == callID && strings.Contains(result.Content, substring) {
				return true
			}
		}
	}
	return false
}

// describe renders a request's conversation for a failure message.
func describe(req provider.Request) string {
	var out strings.Builder
	for _, message := range req.Messages {
		fmt.Fprintf(&out, "[%s]", message.Role)
		for _, block := range message.Content {
			switch b := block.(type) {
			case provider.TextBlock:
				fmt.Fprintf(&out, " text(%q)", b.Text)
			case provider.ToolUseBlock:
				fmt.Fprintf(&out, " tool_use(%s %s)", b.Name, string(b.Args))
			case provider.ToolResultBlock:
				fmt.Fprintf(&out, " tool_result(%s error=%t %q)", b.Name, b.IsError, b.Content)
			}
		}
		out.WriteString("\n")
	}
	return out.String()
}

package runtime_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
	"github.com/KoukeNeko/JingClaw/core/internal/tool/builtin"
)

// investigating is the harness with the tool registered, since it is the one
// tool whose collaborator is the runtime and so cannot be registered before it
// exists.
func investigating(t *testing.T, turns [][]provider.Event) (*runtime.Runtime, *memory.Store, *scriptedProvider) {
	t.Helper()

	rt, store, scripted, registry := newToolHarness(t, turns)
	registry.MustRegister(&builtin.Investigate{Delegator: rt})
	return rt, store, scripted
}

func TestADelegatedSearchAnswersWithoutJoiningTheConversation(t *testing.T) {
	rt, store, scripted := investigating(t, [][]provider.Event{
		// The conversation asks.
		{toolCall("call_1", "investigate", map[string]any{"question": "Which Go files exist?"})},

		// The worker's own turns.
		{toolCall("call_2", "glob_files", map[string]any{"pattern": "**/*.go"})},
		{provider.TextDelta{Text: "Only src/main.go."}, provider.Completed{StopReason: domain.StopEndTurn}},

		// The conversation answers.
		{provider.TextDelta{Text: "There is one."}, provider.Completed{StopReason: domain.StopEndTurn}},
	})
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "delegating")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	runTurn(t, rt, session.ID, "How many Go files are there?")

	// The worker ran, as its own run, with the parent recorded.
	runs, err := store.ListRuns(ctx, session.ID)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("want a conversation run and a worker run, got %d", len(runs))
	}

	var worker domain.Run
	for _, run := range runs {
		if run.Kind == domain.RunWorker {
			worker = run
		}
	}
	if worker.ID == "" {
		t.Fatal("no run was recorded as a worker")
	}
	if worker.ParentRunID == "" {
		t.Fatal("the worker does not say whose question it was")
	}

	// The answer reached the conversation.
	if len(scripted.requests) != 4 {
		t.Fatalf("want four provider calls, got %d", len(scripted.requests))
	}
	last := scripted.requests[3]
	if !strings.Contains(conversationText(last), "Only src/main.go.") {
		t.Error("the worker's answer never reached the conversation")
	}

	// And the searching did not. This is the whole reason to delegate: the
	// glob and its result belong to the worker's run and must not be replayed
	// to the model that asked.
	if strings.Contains(conversationText(last), "glob_files") {
		t.Error("the worker's tool calls were replayed into the conversation")
	}
}

func TestADelegatedSearchIsNotGivenToolsThatChangeAnything(t *testing.T) {
	rt, _, scripted := investigating(t, [][]provider.Event{
		{toolCall("call_1", "investigate", map[string]any{"question": "Anything."})},
		{provider.TextDelta{Text: "Nothing found."}, provider.Completed{StopReason: domain.StopEndTurn}},
		{provider.TextDelta{Text: "Right."}, provider.Completed{StopReason: domain.StopEndTurn}},
	})
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "confined")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	runTurn(t, rt, session.ID, "Have a look.")

	if len(scripted.requests) < 2 {
		t.Fatalf("the worker never ran: %d provider calls", len(scripted.requests))
	}

	offered := map[string]bool{}
	for _, declared := range scripted.requests[1].Tools {
		offered[declared.Name] = true
	}

	// The precondition: without it, "write_file was not offered" would pass
	// against a worker that was offered no tools at all.
	if !offered["glob_files"] {
		t.Fatal("the worker was not offered the tools it needs to search")
	}
	for _, forbidden := range []string{"write_file", "edit_file", "investigate"} {
		if offered[forbidden] {
			t.Errorf("the worker was offered %s", forbidden)
		}
	}

	// And the conversation still has all of them, so what was withheld was
	// withheld from the worker rather than from everybody.
	parent := map[string]bool{}
	for _, declared := range scripted.requests[0].Tools {
		parent[declared.Name] = true
	}
	if !parent["write_file"] {
		t.Error("the conversation lost write_file too")
	}
}

func TestADelegatedSearchCannotDelegate(t *testing.T) {
	rt, store, _ := investigating(t, [][]provider.Event{
		{provider.TextDelta{Text: "Nothing."}, provider.Completed{StopReason: domain.StopEndTurn}},
	})
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "recursion")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// A worker run, written straight to the store: the tool it would need to
	// ask for this is not one it is given, so the check under test is the one
	// behind the tool rather than the tool's absence.
	worker := domain.Run{
		ID:        "run_worker",
		SessionID: session.ID,
		Status:    domain.RunCompleted,
		Kind:      domain.RunWorker,
	}
	if err := store.CreateRun(ctx, worker); err != nil {
		t.Fatalf("create run: %v", err)
	}

	if _, err := rt.Investigate(ctx, worker.ID, "And again?"); err == nil {
		t.Fatal("a worker was allowed to delegate")
	}
}

func TestADelegatedSearchKeepsTheOriginOfWhoAskedForIt(t *testing.T) {
	rt, store, _ := investigating(t, [][]provider.Event{
		{toolCall("call_1", "investigate", map[string]any{"question": "Anything?"})},
		{provider.TextDelta{Text: "No."}, provider.Completed{StopReason: domain.StopEndTurn}},
		{provider.TextDelta{Text: "Fine."}, provider.Completed{StopReason: domain.StopEndTurn}},
	})
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "origin")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// A turn from a chat platform, which is the least trusted origin there is.
	from := domain.FromAPlatformAccount("discord", "u_1", "someone")
	runID, _, err := rt.SendTurn(ctx, session.ID, "Go and look.", from)
	if err != nil {
		t.Fatalf("send turn: %v", err)
	}
	waitForRun(t, rt, runID)

	runs, err := store.ListRuns(ctx, session.ID)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	for _, run := range runs {
		if run.Kind != domain.RunWorker {
			continue
		}
		// Inheriting rather than defaulting. A worker that ran as something
		// more trusted than the turn that asked for it would be a way to
		// launder authority through delegation.
		if run.Origin.Kind != domain.OriginGateway ||
			run.Origin.Principal == nil || run.Origin.Principal.Platform != "discord" {
			t.Fatalf("the worker did not inherit the origin: %+v", run.Origin)
		}
		return
	}
	t.Fatal("no worker ran")
}

// conversationText is everything the model was sent, flattened.
func conversationText(req provider.Request) string {
	var out strings.Builder
	for _, message := range req.Messages {
		for _, block := range message.Content {
			switch typed := block.(type) {
			case provider.TextBlock:
				out.WriteString(typed.Text)
			case provider.ToolUseBlock:
				out.WriteString(typed.Name)
				out.Write(typed.Args)
			case provider.ToolResultBlock:
				out.WriteString(typed.Name)
				out.WriteString(typed.Content)
			}
			out.WriteString("\n")
		}
	}
	return out.String()
}

// TestADelegatedSearchIsRefusedAToolItWasNotOffered is the second of the two
// layers, and the one that is actually a boundary.
//
// Withholding a tool from the declarations decides what a model that reads
// them asks for. It does nothing about a name invented, replayed, or arrived
// at some other way — and without this, such a call reaches the registry and
// runs. The provider here does exactly that: it calls a tool the worker was
// never told about.
func TestADelegatedSearchIsRefusedAToolItWasNotOffered(t *testing.T) {
	rt, store, _ := investigating(t, [][]provider.Event{
		{toolCall("call_1", "investigate", map[string]any{"question": "Anything?"})},

		// The worker, reaching for something it was not offered.
		{toolCall("call_2", "write_file", map[string]any{
			"path": "taken.md", "content": "should have stopped"})},
		{provider.TextDelta{Text: "I could not."}, provider.Completed{StopReason: domain.StopEndTurn}},

		{provider.TextDelta{Text: "Right."}, provider.Completed{StopReason: domain.StopEndTurn}},
	})
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "refusing")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	runTurn(t, rt, session.ID, "Have a look.")

	var worker domain.RunID
	runs, err := store.ListRuns(ctx, session.ID)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	for _, run := range runs {
		if run.Kind == domain.RunWorker {
			worker = run.ID
		}
	}
	if worker == "" {
		t.Fatal("no worker ran")
	}

	events, err := store.ListAfter(ctx, session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}

	var refused bool
	for _, event := range events {
		done, ok := event.Payload.(domain.ToolCallCompleted)
		if !ok || event.RunID != worker || done.Name != "write_file" {
			continue
		}
		if !done.IsError {
			t.Fatal("a worker wrote a file")
		}
		refused = true
	}
	if !refused {
		t.Fatal("the worker's call never reached the runtime, so this proves nothing")
	}

	// Refused rather than parked. A worker waiting on a person is waiting on
	// nobody: it sits underneath a tool call that is already waiting, and
	// whoever would decide never saw it start.
	waiting, err := store.PendingApprovals(ctx, session.ID)
	if err != nil {
		t.Fatalf("pending approvals: %v", err)
	}
	if len(waiting) != 0 {
		t.Fatalf("a worker asked somebody for permission: %+v", waiting)
	}
}

// TestADelegatedSearchThatNeverFinishedReportsNothing keeps a half-finished
// thought from arriving as a conclusion.
//
// A worker narrates between tool calls. If what comes back is everything it
// said, a run that hit its ceiling hands the parent that narration with
// nothing to mark it as the middle of something.
func TestADelegatedSearchThatNeverFinishedReportsNothing(t *testing.T) {
	turns := [][]provider.Event{
		{toolCall("call_1", "investigate", map[string]any{"question": "Anything?"})},
	}
	// More turns than it is allowed, each one saying something and then
	// asking for another tool: it never stops on an answer.
	for i := 0; i < 6; i++ {
		turns = append(turns, []provider.Event{
			provider.TextDelta{Text: "Still looking."},
			toolCall(fmt.Sprintf("call_look_%d", i), "glob_files", map[string]any{"pattern": "**/*.go"}),
		})
	}
	turns = append(turns, []provider.Event{
		provider.TextDelta{Text: "Right."}, provider.Completed{StopReason: domain.StopEndTurn},
	})

	rt, store, _ := investigating(t, turns)
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "unfinished")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	parent := runTurn(t, rt, session.ID, "Have a look.")

	events, err := store.ListAfter(ctx, session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}

	var answered bool
	for _, event := range events {
		done, ok := event.Payload.(domain.ToolCallCompleted)
		if !ok || event.RunID != parent || done.Name != "investigate" {
			continue
		}
		answered = true

		if !done.IsError {
			t.Errorf("a search that never finished came back as an answer: %s", done.Content)
		}
		if strings.Contains(done.Content, "Still looking") {
			t.Errorf("the worker's narration was handed over as its answer: %s", done.Content)
		}
	}
	if !answered {
		t.Fatal("the conversation never got a result at all")
	}
}

// TestADelegatedSearchCanSeeItsOwnWork is the other half of the isolation.
//
// A worker's events are kept out of the conversation, and the same rule
// applied to the worker itself leaves it reading its opening question again
// every turn, never seeing what it just did, repeating one call until it runs
// out of turns. That failure is invisible from the parent — an answer is a
// tool result either way — so it is asserted here.
func TestADelegatedSearchCanSeeItsOwnWork(t *testing.T) {
	rt, _, scripted := investigating(t, [][]provider.Event{
		{toolCall("call_1", "investigate", map[string]any{"question": "Which Go files exist?"})},

		{toolCall("call_2", "glob_files", map[string]any{"pattern": "**/*.go"})},
		{provider.TextDelta{Text: "Only src/main.go."}, provider.Completed{StopReason: domain.StopEndTurn}},

		{provider.TextDelta{Text: "One."}, provider.Completed{StopReason: domain.StopEndTurn}},
	})
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "self")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	runTurn(t, rt, session.ID, "How many Go files?")

	if len(scripted.requests) != 4 {
		t.Fatalf("want four provider calls, got %d", len(scripted.requests))
	}

	// The worker's second turn: it must show the call it just made and what
	// came back, or the model is being asked the same question again.
	second := conversationText(scripted.requests[2])
	if !strings.Contains(second, "glob_files") {
		t.Errorf("the worker cannot see the call it just made:\n%s", second)
	}
	if !strings.Contains(second, "main.go") {
		t.Errorf("the worker cannot see what its call returned:\n%s", second)
	}

	// And still not the conversation's, which is what it is isolated from.
	if strings.Contains(second, "How many Go files?") {
		t.Errorf("the worker was shown the conversation it was called from:\n%s", second)
	}
}

// TestADelegatedSearchIsDrawnAsAToolCallAndNotAConversation keeps the search
// out of what a person reads.
//
// A worker's question is a user message and its answer an assistant one, in
// the same session as the conversation. Folded into the view they draw as
// somebody else asking something and being answered, halfway through a turn
// nobody else was part of.
func TestADelegatedSearchIsDrawnAsAToolCallAndNotAConversation(t *testing.T) {
	rt, _, _ := investigating(t, [][]provider.Event{
		{toolCall("call_1", "investigate", map[string]any{"question": "Which Go files exist?"})},
		{toolCall("call_2", "glob_files", map[string]any{"pattern": "**/*.go"})},
		{provider.TextDelta{Text: "Only src/main.go."}, provider.Completed{StopReason: domain.StopEndTurn}},
		{provider.TextDelta{Text: "There is one."}, provider.Completed{StopReason: domain.StopEndTurn}},
	})
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "drawing")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	runTurn(t, rt, session.ID, "How many Go files?")

	view, err := rt.SessionViewOf(ctx, session.ID, 0)
	if err != nil {
		t.Fatalf("view: %v", err)
	}

	asked := 0
	for _, message := range view.Messages {
		if message.Role == domain.RoleUser {
			asked++
		}
		if strings.Contains(message.Text, "Only src/main.go.") {
			t.Errorf("the worker's answer is drawn as part of the conversation: %+v", message)
		}
	}
	if asked != 1 {
		t.Errorf("want one question drawn, got %d: %+v", asked, view.Messages)
	}

	// The tool call still shows, which is how somebody sees it happened at
	// all. Without this, hiding the worker would hide the delegation.
	if !strings.Contains(viewText(view), "investigate") {
		t.Errorf("the delegation is not drawn anywhere: %+v", view.Messages)
	}
}

func viewText(view runtime.SessionView) string {
	var out strings.Builder
	for _, message := range view.Messages {
		out.WriteString(message.Text)
		for _, call := range message.ToolCalls {
			out.WriteString(call.Name)
			out.WriteString(call.Summary)
		}
		out.WriteString("\n")
	}
	return out.String()
}

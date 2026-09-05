package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
)

// A client attaching to a session should be able to draw it without replaying
// the log, and what it draws must match what the log says.
func TestAViewIsTheConversationWithoutTheReplay(t *testing.T) {
	arguments := `{"path":"notes.md"}`

	rt, _, _, _ := newToolHarness(t, [][]provider.Event{
		{
			toolCall("call_1", "read_file", map[string]any{"path": "notes.md"}),
			provider.Completed{StopReason: domain.StopToolUse},
		},
		{
			provider.TextDelta{Text: "The note "},
			provider.TextDelta{Text: "says hello."},
			provider.Completed{StopReason: domain.StopEndTurn},
		},
	})

	session, err := rt.CreateSession(context.Background(), "reading")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	runID, _, err := rt.SendTurn(context.Background(), session.ID, "what does the note say", domain.RunOrigin{})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := rt.Wait(context.Background(), runID); err != nil {
		t.Fatalf("wait: %v", err)
	}

	view, err := rt.SessionViewOf(context.Background(), session.ID, 0)
	if err != nil {
		t.Fatalf("view: %v", err)
	}

	if view.Session.ID != session.ID {
		t.Errorf("the view is of %s", view.Session.ID)
	}
	if len(view.Messages) < 2 {
		t.Fatalf("the conversation has %d messages: %+v", len(view.Messages), view.Messages)
	}

	first := view.Messages[0]
	if first.Role != domain.RoleUser || first.Text != "what does the note say" {
		t.Errorf("the first message is %+v", first)
	}

	// Deltas are assembled, because every client doing that separately is the
	// same reconstruction written several times.
	var assistant *runtime.ViewMessage
	for index, message := range view.Messages {
		if message.Role == domain.RoleAssistant && message.Text != "" {
			assistant = &view.Messages[index]
		}
	}
	if assistant == nil {
		t.Fatal("no assistant message carries text")
	}
	if assistant.Text != "The note says hello." {
		t.Errorf("the deltas were not assembled: %q", assistant.Text)
	}

	// The tool the turn asked for is on the turn that asked.
	var found *runtime.ViewToolCall
	for _, message := range view.Messages {
		for index, call := range message.ToolCalls {
			if call.Name == "read_file" {
				found = &message.ToolCalls[index]
			}
		}
	}
	if found == nil {
		t.Fatalf("the tool call is missing from the view: %+v", view.Messages)
	}
	if !found.Completed {
		t.Error("a finished call is shown as still running")
	}
	if !strings.Contains(found.Summary, "notes.md") {
		t.Errorf("the call says nothing about what it did: %q", found.Summary)
	}
	_ = arguments

	// Nothing is running, and subscribing after the head continues exactly
	// where the view stops.
	if view.ActiveRun != nil {
		t.Errorf("a finished session reports run %s as active", view.ActiveRun.ID)
	}
	if view.HeadSeq == 0 {
		t.Error("the view gives no sequence number to subscribe from")
	}
	if view.Truncated {
		t.Error("a short conversation reports itself truncated")
	}
}

// A conversation is read from its end, and says when there is more above.
func TestALongConversationIsCutFromTheTop(t *testing.T) {
	const turns = 6

	replies := make([][]provider.Event, 0, turns)
	for range turns {
		replies = append(replies, []provider.Event{
			provider.TextDelta{Text: "ok"},
			provider.Completed{StopReason: domain.StopEndTurn},
		})
	}

	rt, _, _, _ := newToolHarness(t, replies)

	session, err := rt.CreateSession(context.Background(), "long")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for range turns {
		runID, _, err := rt.SendTurn(context.Background(), session.ID, "again", domain.RunOrigin{})
		if err != nil {
			t.Fatalf("send: %v", err)
		}
		if err := rt.Wait(context.Background(), runID); err != nil {
			t.Fatalf("wait: %v", err)
		}
	}

	view, err := rt.SessionViewOf(context.Background(), session.ID, 3)
	if err != nil {
		t.Fatalf("view: %v", err)
	}

	if len(view.Messages) != 3 {
		t.Errorf("asked for 3 messages and got %d", len(view.Messages))
	}
	if !view.Truncated {
		t.Error("a cut conversation does not say there is more above it")
	}
	// The end, not the beginning: a conversation is read from where it is now.
	//
	// The head is at or past the last message rather than equal to it, because
	// the last event of a finished turn is the run ending, which belongs to no
	// message. Subscribing after the head is what must not replay anything
	// already drawn.
	last := view.Messages[len(view.Messages)-1]
	if last.Seq > view.HeadSeq {
		t.Errorf("a message at seq %d is past the head at %d", last.Seq, view.HeadSeq)
	}
	if view.HeadSeq-last.Seq > 4 {
		t.Errorf("the head at %d is far past the last message at %d, so the view is stale",
			view.HeadSeq, last.Seq)
	}
}

// A view that showed the conversation without what it is blocked on would show
// a session that looks finished and is waiting on somebody.
func TestAViewSaysWhatItIsWaitingOn(t *testing.T) {
	rt, _, _ := newGatedHarness(t, [][]provider.Event{
		{
			toolCall("call_1", "write_file", map[string]any{"path": "a.txt", "content": "x"}),
			provider.Completed{StopReason: domain.StopToolUse},
		},
	})

	session, err := rt.CreateSession(context.Background(), "gated")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	runID, _, err := rt.SendTurn(context.Background(), session.ID, "write it", domain.RunOrigin{})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	waitForRun(t, rt, runID)

	view, err := rt.SessionViewOf(context.Background(), session.ID, 0)
	if err != nil {
		t.Fatalf("view: %v", err)
	}

	if len(view.Pending) != 1 {
		t.Fatalf("the view reports %d approvals waiting", len(view.Pending))
	}
	if view.Pending[0].ToolName != "write_file" {
		t.Errorf("waiting on %q", view.Pending[0].ToolName)
	}
	if view.ActiveRun == nil {
		t.Fatal("a run parked on a person is not reported as active")
	}
	if view.ActiveRun.ID != runID {
		t.Errorf("the active run is %s, want %s", view.ActiveRun.ID, runID)
	}
}

// A question that waited its turn is drawn after the answer it waited for,
// not in the middle of it — the same placement the model is shown.
func TestAViewDrawsAQueuedQuestionAfterTheAnswerItWaitedFor(t *testing.T) {
	model := &orderingProvider{release: make(chan struct{})}
	rt, _ := newQueueRuntime(t, model)
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "queue view")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	first, _, err := rt.SendTurn(ctx, session.ID, "first question", domain.RunOrigin{})
	if err != nil {
		t.Fatalf("send first: %v", err)
	}
	second, _, err := rt.SendTurn(ctx, session.ID, "second question", domain.RunOrigin{})
	if err != nil {
		t.Fatalf("send second: %v", err)
	}

	// While the second is still in line, the view shows it waiting at the end,
	// not splitting the first exchange.
	model.release <- struct{}{}
	if err := rt.Wait(ctx, first); err != nil {
		t.Fatalf("wait first: %v", err)
	}

	view, err := rt.SessionViewOf(ctx, session.ID, 0)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	var texts []string
	for _, message := range view.Messages {
		texts = append(texts, string(message.Role)+":"+strings.TrimSpace(message.Text))
	}
	want := []string{"user:first question", "assistant:answer 1", "user:second question"}
	if strings.Join(texts, "|") != strings.Join(want, "|") {
		t.Errorf("the view is ordered\n  %s\nwant\n  %s",
			strings.Join(texts, "\n  "), strings.Join(want, "\n  "))
	}

	model.release <- struct{}{}
	if err := rt.Wait(ctx, second); err != nil {
		t.Fatalf("wait second: %v", err)
	}
}

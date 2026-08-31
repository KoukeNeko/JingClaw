package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// TestAMessageWithNothingInItStillReachesTheModel is what a picture with no
// caption used to break.
//
// Somebody wrote the bot's name and attached an image. The mention was
// stripped, so the text was empty, and the attachment did not survive the
// trip — leaving a user turn with no content at all. Every provider refuses a
// conversation whose last turn is the model's, because there is nothing to
// answer, and what the person saw was "something went wrong at the model".
func TestAMessageWithNothingInItStillReachesTheModel(t *testing.T) {
	rt, _, scripted, _ := newToolHarness(t, [][]provider.Event{
		{provider.TextDelta{Text: "There is nothing there."},
			provider.Completed{StopReason: domain.StopEndTurn}},
	})
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "empty")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runID, _, err := rt.SendTurn(ctx, session.ID, "", domain.RunOrigin{})
	if err != nil {
		t.Fatalf("send turn: %v", err)
	}
	waitForRun(t, rt, runID)

	if len(scripted.requests) != 1 {
		t.Fatalf("want one provider call, got %d", len(scripted.requests))
	}

	// The conversation has to end with the person, or there is nothing being
	// answered.
	messages := scripted.requests[0].Messages
	if len(messages) == 0 {
		t.Fatal("nothing was sent")
	}
	last := messages[len(messages)-1]
	if last.Role != provider.RoleUser {
		t.Fatalf("the conversation ends with %s, which no provider will answer", last.Role)
	}

	// And that turn has to carry something. A block with an empty string is
	// dropped by the adapters, so an empty turn and no turn are the same
	// thing by the time it reaches the wire.
	var said string
	for _, block := range last.Content {
		if text, ok := block.(provider.TextBlock); ok {
			said += text.Text
		}
	}
	if strings.TrimSpace(said) == "" {
		t.Error("the turn reached the model carrying nothing, which is what the provider refuses")
	}
}

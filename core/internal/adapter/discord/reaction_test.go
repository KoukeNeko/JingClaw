package discord

import (
	"encoding/json"
	"strings"
	"testing"

	jcgateway "github.com/KoukeNeko/JingClaw/core/internal/gateway"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway/render"
)

// statusDispatch is one terminal status as the outbox would carry it.
func statusDispatch(t *testing.T, state string) jcgateway.Dispatch {
	t.Helper()
	payload, err := json.Marshal(jcgateway.StatusPayload{
		State:  state,
		Detail: "the model gave up",
	})
	if err != nil {
		t.Fatalf("encode status: %v", err)
	}
	return jcgateway.Dispatch{Kind: jcgateway.DispatchStatus, Payload: string(payload)}
}

// The reaction and the text must not disagree.
//
// A reader glances at the reaction and reads the text only if something looks
// worth reading, so the two saying different things means the wrong one is
// what gets believed. Written as an invariant over every terminal state rather
// than as an assertion about particular emoji, so that changing the symbols
// stays easy and making them lie does not.
func TestTheReactionAgreesWithTheText(t *testing.T) {
	for _, state := range []string{"completed", "failed", "cancelled"} {
		emoji, _ := reactionForStatus(state)
		text, err := render.Dispatch(statusDispatch(t, state), discordStyle)
		if err != nil {
			t.Fatalf("render %s: %v", state, err)
		}

		saysItWorked := emoji == "✅"
		textSaysItWorked := !strings.Contains(text, "did not work") &&
			!strings.Contains(text, "Stopped")

		if saysItWorked != textSaysItWorked {
			t.Errorf("%s: reaction %q says success=%v but the text says success=%v\n  text: %s",
				state, emoji, saysItWorked, textSaysItWorked, text)
		}
	}
}

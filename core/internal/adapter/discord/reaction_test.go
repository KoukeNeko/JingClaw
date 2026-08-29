package discord

import (
	"strings"
	"testing"

	jcgateway "github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

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
		text := renderStatus(jcgateway.StatusPayload{State: state, Detail: "the model gave up"})

		saysItWorked := emoji == "✅"
		textSaysItWorked := !strings.Contains(text, "did not work") &&
			!strings.Contains(text, "Stopped")

		if saysItWorked != textSaysItWorked {
			t.Errorf("%s: reaction %q says success=%v but the text says success=%v\n  text: %s",
				state, emoji, saysItWorked, textSaysItWorked, text)
		}
	}
}

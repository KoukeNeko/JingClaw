package runtime

import (
	"strings"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// A turn with no time on it is not dated to the year zero.
//
// Every event this runtime writes carries one, so this is about what arrives
// from somewhere that did not: a stamp of 0001-01-01 is worse than none,
// because it reads as a fact rather than as a gap, and a model asked how long
// ago something was would answer in millennia.
func TestATurnWithNoTimeIsNotStampedAtAll(t *testing.T) {
	if said := whenItWasSaid(time.Time{}); said != "" {
		t.Errorf("a turn with no time on it was dated %q", said)
	}

	when := time.Unix(1788197468, 0)
	said := whenItWasSaid(when)
	if said == "" {
		t.Fatal("a turn with a time on it was not dated")
	}
	if !strings.Contains(said, when.Format(time.RFC3339)) {
		t.Errorf("the stamp %q is not the time it was given", said)
	}
}

// Dating a turn does not take away its explanation.
//
// A message with no text and nothing readable attached has to say so, or a
// provider is handed a turn with nothing in it and refuses the request. The
// stamp is a non-empty text block, so adding it in front of that check would
// satisfy the check while leaving the turn just as empty of anything to
// answer — and now with a blank block in it as well.
func TestADatedTurnStillSaysWhenThereIsNothingInIt(t *testing.T) {
	builder := &conversationBuilder{}
	when := time.Unix(1788197468, 0)

	content := builder.userContent(domain.UserMessageAdded{Text: ""}, when)

	joined := ""
	blank := 0
	for _, block := range content {
		text, ok := block.(provider.TextBlock)
		if !ok {
			continue
		}
		if strings.TrimSpace(text.Text) == "" {
			blank++
		}
		joined += text.Text
	}

	if !strings.Contains(joined, "no text") {
		t.Errorf("an empty turn no longer says it is empty: %q", joined)
	}
	if blank != 0 {
		t.Errorf("%d empty blocks were sent to the model: %q", blank, joined)
	}
	if !strings.Contains(joined, when.Format(time.RFC3339)) {
		t.Errorf("the turn lost its date on the way: %q", joined)
	}
}

// The line is built from the event alone. No time and nobody: nothing. A
// sender with no time is still worth naming.
func TestTheTurnLineNamesWhatTheEventKnows(t *testing.T) {
	if line := turnMetadata(domain.RunOrigin{}, time.Time{}); line != "" {
		t.Errorf("an event with nothing on it produced %q", line)
	}

	who := domain.FromAPlatformAccount("discord", "u1", "doeshing")
	undated := turnMetadata(who, time.Time{})
	if !strings.Contains(undated, "doeshing") {
		t.Errorf("a sender with no time was not named: %q", undated)
	}
	if stamped := turnMetadata(who, time.Unix(1788197468, 0)); !strings.Contains(stamped, "doeshing") ||
		!strings.Contains(stamped, "2026-") {
		t.Errorf("the line does not carry both who and when: %q", stamped)
	}
}

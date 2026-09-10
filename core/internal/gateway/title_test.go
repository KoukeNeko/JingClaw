package gateway

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A title is cut to a length, and the cut must fall between characters. Bytes
// do not: a Chinese character is three of them, so a byte cut lands inside one
// often, and leaves the title ending in half a character.
func TestALongTitleIsCutBetweenCharacters(t *testing.T) {
	long := strings.Repeat("蒸", 100) // 300 bytes, 100 characters
	title := (&Ingress{}).title(InboundMessage{Text: long})

	if !utf8.ValidString(title) {
		t.Fatalf("the title is not valid text; a character was cut in half: %q", title)
	}

	// The ellipsis and 60 characters kept, none of them broken.
	if want := strings.Repeat("蒸", 60) + "…"; title != want {
		t.Errorf("title = %q, want %q", title, want)
	}
}

// A title that already fits is left as it is, ellipsis and all.
func TestAShortTitleIsUntouched(t *testing.T) {
	if got := (&Ingress{}).title(InboundMessage{Text: "晚餐怎麼樣"}); got != "晚餐怎麼樣" {
		t.Errorf("title = %q, want it unchanged", got)
	}
}

// An empty message still names the conversation, so a session is never
// untitled.
func TestAnEmptyMessageNamesTheConversation(t *testing.T) {
	got := (&Ingress{}).title(InboundMessage{
		Conversation: ConversationRef{Platform: PlatformDiscord},
	})
	if !strings.Contains(got, "discord") {
		t.Errorf("an empty message produced %q, which does not name the platform", got)
	}
}

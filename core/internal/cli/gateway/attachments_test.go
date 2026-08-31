package gateway

import (
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

// TestAFileSurvivesTheTripInward is the wire this used to lack.
//
// The adapter fetched the file and the ingress was waiting to store it, and
// in between there was nowhere on the message to put one — so every picture
// anybody sent was downloaded and dropped on the way in. A message that was
// only a picture then reached the model carrying nothing at all, and what the
// person saw was the model refusing a request with no question in it.
func TestAFileSurvivesTheTripInward(t *testing.T) {
	sent := gateway.InboundMessage{
		Conversation: gateway.ConversationRef{Platform: "discord", ChannelID: "chan_1"},
		Principal:    gateway.Principal{ID: "someone"},
		Text:         "",
		Attachments: []gateway.Attachment{
			{
				ID: "att_1", Name: "screenshot.png",
				ContentType: "image/png", Size: 4,
				Data: []byte{1, 2, 3, 4},
			},
			{
				// One the adapter declined to fetch. It still travels: a file
				// that was sent is a fact about the message whether or not
				// anybody can look at it.
				ID: "att_2", Name: "recording.mov",
				ContentType: "video/quicktime", Size: 900_000_000,
			},
		},
	}

	got := attachmentsToProto(sent.Attachments)
	if len(got) != 2 {
		t.Fatalf("carried %d of 2 files", len(got))
	}

	if got[0].GetName() != "screenshot.png" || string(got[0].GetData()) != "\x01\x02\x03\x04" {
		t.Errorf("the picture did not survive: %+v", got[0])
	}
	if got[1].GetName() != "recording.mov" || len(got[1].GetData()) != 0 {
		t.Errorf("the one nobody fetched did not survive: %+v", got[1])
	}
	if got[1].GetSize() != 900_000_000 {
		t.Errorf("what it would have been is lost: %d", got[1].GetSize())
	}

	// And nothing is invented for a message that had none.
	if attachmentsToProto(nil) != nil {
		t.Error("a message with no files was given some")
	}
}

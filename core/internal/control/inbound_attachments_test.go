package control

import (
	"testing"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
)

// TestFilesArriveAsTheyWereSent is the daemon's half of the wire.
//
// The gateway's half is checked beside its own converter. Both are needed:
// the field existing on the message is not the same as either end using it,
// and the bug this replaces was exactly that — a fetched file with nowhere to
// go, on a message both ends were otherwise ready for.
func TestFilesArriveAsTheyWereSent(t *testing.T) {
	got := inboundAttachmentsFromProto([]*controlv1.InboundAttachment{
		{
			Id: "att_1", Name: "screenshot.png",
			ContentType: "image/png", Size: 4, Data: []byte{1, 2, 3, 4},
		},
		{
			Id: "att_2", Name: "recording.mov",
			ContentType: "video/quicktime", Size: 900_000_000,
		},
	})

	if len(got) != 2 {
		t.Fatalf("got %d of 2 files", len(got))
	}
	if got[0].Name != "screenshot.png" || len(got[0].Data) != 4 {
		t.Errorf("the picture arrived wrong: %+v", got[0])
	}
	if got[0].ContentType != "image/png" {
		t.Errorf("without its type the model will not be shown it: %+v", got[0])
	}

	// The one nobody fetched arrives without bytes and with everything else,
	// so the model can be told what it is not looking at.
	if got[1].Name != "recording.mov" || len(got[1].Data) != 0 || got[1].Size != 900_000_000 {
		t.Errorf("the unfetched one arrived wrong: %+v", got[1])
	}

	if inboundAttachmentsFromProto(nil) != nil {
		t.Error("a message with no files was given some")
	}
}

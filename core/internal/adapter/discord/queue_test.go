package discord

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	jcgateway "github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

func statusFor(t *testing.T, state string) jcgateway.Dispatch {
	t.Helper()
	payload, _ := json.Marshal(jcgateway.StatusPayload{State: state})
	return jcgateway.Dispatch{
		Kind:    jcgateway.DispatchStatus,
		RunID:   "run_1",
		Payload: string(payload),
		Target: jcgateway.ConversationRef{
			ChannelID:       "987654321098765432",
			SourceMessageID: "111111111111111111",
		},
	}
}

func reactions(posted []sent) (added, removed []string) {
	for _, one := range posted {
		if !strings.Contains(one.Path, "/reactions/") {
			continue
		}
		decoded, _ := url.PathUnescape(one.Path)
		switch one.Method {
		case "PUT":
			added = append(added, decoded)
		case "DELETE":
			removed = append(removed, decoded)
		}
	}
	return added, removed
}

func has(paths []string, emoji string) bool {
	for _, path := range paths {
		if strings.Contains(path, emoji) {
			return true
		}
	}
	return false
}

// A message waiting its turn is marked 📥 on the message itself, and the mark
// comes off the moment the run starts — whether it then goes on to think,
// finish, fail, or be cancelled.
func TestAWaitingMessageIsMarkedAndUnmarkedWhenItStarts(t *testing.T) {
	adapter, posted := stubDiscord(t)

	if _, err := adapter.Post(t.Context(), statusFor(t, "queued")); err != nil {
		t.Fatalf("queued: %v", err)
	}
	added, removed := reactions(*posted)
	if !has(added, "📥") {
		t.Errorf("a waiting message was not marked: added %v", added)
	}
	if has(removed, "📥") {
		t.Errorf("the mark was removed before anything started: removed %v", removed)
	}

	*posted = nil
	if _, err := adapter.Post(t.Context(), statusFor(t, "provider_started")); err != nil {
		t.Fatalf("started: %v", err)
	}
	added, removed = reactions(*posted)
	if !has(removed, "📥") {
		t.Errorf("the waiting mark stayed on a message whose run had started: removed %v", removed)
	}
	if !has(added, "🧠") {
		t.Errorf("starting did not mark the message as being thought about: added %v", added)
	}

	// A waiting run cancelled before it started loses the mark too.
	*posted = nil
	if _, err := adapter.Post(t.Context(), statusFor(t, "cancelled")); err != nil {
		t.Fatalf("cancelled: %v", err)
	}
	if _, removed = reactions(*posted); !has(removed, "📥") {
		t.Errorf("a cancelled run left its waiting mark on: removed %v", removed)
	}
}

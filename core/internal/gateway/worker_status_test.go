package gateway_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	jcgateway "github.com/KoukeNeko/JingClaw/core/internal/gateway"
	"github.com/KoukeNeko/JingClaw/core/internal/provider/fake"
)

// While a worker works, the conversation is told what it is doing — on the
// parent's line, not one of its own — and the tools it ran end up in the
// parent's footer. Its text and its ending never reach the channel.
//
// The fake provider replays its script from the top for every conversation,
// so the parent and the worker both call investigate (the worker's is refused:
// a worker cannot delegate), both read the file, and both answer in the same
// words. What tells them apart is structure: one final message, two working
// lines for the read, and a footer that counts the read twice.
func TestAWorkersWorkShowsOnTheParentsLine(t *testing.T) {
	h := newHarness(t, 0)
	h.bind(t, "gateway", "user_1")
	// The harness clock never moves, so the working-line throttle would let
	// one line through per run and swallow the worker's. Off, for this.
	h.projector.WorkingInterval = 0

	h.provider.Script = []fake.Turn{
		{Tool: "investigate", Args: `{"question":"what is in src/main.go"}`},
		{Tool: "read_file", Args: `{"path":"src/main.go"}`},
		{Text: "a main package"},
	}

	accepted, err := h.ingress.Accept(context.Background(),
		message("m1", "look", discordPrincipal("user_1")))
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := h.runtime.Wait(context.Background(), accepted.RunID); err != nil {
		t.Fatalf("wait: %v", err)
	}

	parent := accepted.RunID
	var finals, workingReads int
	var footer jcgateway.RunSummary
	for _, dispatch := range h.dispatches(t) {
		if dispatch.RunID != parent {
			t.Errorf("a dispatch went out under a run the conversation is not shown: %+v", dispatch)
		}
		switch dispatch.Kind {
		case jcgateway.DispatchStatus:
			var status jcgateway.StatusPayload
			_ = json.Unmarshal([]byte(dispatch.Payload), &status)
			if status.State == "working" && strings.Contains(status.Detail, "read_file") {
				workingReads++
			}
			if status.State == "completed" && status.Summary != nil {
				footer = *status.Summary
			}
		case jcgateway.DispatchMessage:
			var msg jcgateway.MessagePayload
			_ = json.Unmarshal([]byte(dispatch.Payload), &msg)
			if msg.Final {
				finals++
			}
		}
	}

	// The parent's read and the worker's: both told to the conversation.
	if workingReads != 2 {
		t.Errorf("the conversation saw %d working lines for the read, want 2 (the parent's and the worker's)", workingReads)
	}
	// The worker's answer is the parent's tool result, never a message.
	if finals != 1 {
		t.Errorf("%d final messages were posted, want 1 — a worker's answer must not be one", finals)
	}
	// And the footer counts what the worker ran.
	reads := 0
	for _, use := range footer.Tools {
		if use.Name == "read_file" {
			reads = use.Calls
		}
	}
	if reads != 2 {
		t.Errorf("the footer counts read_file %d times, want 2: %+v", reads, footer.Tools)
	}
}

package gateway_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// bindConsole binds a second room, an operator's private one, as a console.
func bindConsole(t *testing.T, h *harness, channelID string) {
	t.Helper()
	conversation := discordConversation()
	if err := h.store.UpsertBinding(context.Background(), gateway.Binding{
		ID:                "binding_console",
		Platform:          conversation.Platform,
		AccountID:         conversation.AccountID,
		TenantID:          conversation.TenantID,
		ChannelID:         channelID,
		WorkspaceID:       "ws_1",
		PermissionProfile: gateway.ConsoleProfileName,
		AllowedPrincipals: []string{"user_1"},
	}); err != nil {
		t.Fatalf("bind console: %v", err)
	}
}

// A console channel is a window on the whole deployment, the way the terminal
// console is. A run in a public room shows up there line by line — the
// message that started it, the run, each tool going out and coming back, the
// answer — and the public room itself sees none of that.
func TestAConsoleMirrorsARunThatHappensElsewhere(t *testing.T) {
	arguments, _ := json.Marshal(map[string]any{"path": "notes.md"})
	h := newSummaryHarness(t, [][]provider.Event{
		{
			provider.ToolCallRequested{ID: "call_1", Name: "read_file", Args: arguments},
			provider.Completed{StopReason: domain.StopToolUse},
		},
		{
			provider.TextDelta{Text: "Read it."},
			provider.Completed{StopReason: domain.StopEndTurn},
		},
	})
	h.bind(t, "gateway", "user_1")       // channel_1: where people talk
	bindConsole(t, h, "channel_console") // the operator's window

	accepted, err := h.ingress.Accept(context.Background(),
		message("m1", "read notes.md", discordPrincipal("user_1")))
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := h.runtime.Wait(context.Background(), accepted.RunID); err != nil {
		t.Fatalf("wait: %v", err)
	}

	kinds := map[string]int{}
	var toolDone *gateway.LogPayload
	for _, dispatch := range h.dispatches(t) {
		if dispatch.Kind != gateway.DispatchLog {
			continue
		}
		if dispatch.Target.ChannelID != "channel_console" {
			t.Errorf("a log line went to %q, not the console", dispatch.Target.ChannelID)
		}
		var payload gateway.LogPayload
		if err := json.Unmarshal([]byte(dispatch.Payload), &payload); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if payload.Line == nil {
			t.Fatalf("a mirrored log carries no line: %+v", payload)
		}
		kinds[payload.Line.Kind+" "+payload.Line.State]++
		if payload.Line.Kind == "TOOL" && payload.Line.State == "✓" {
			toolDone = &payload
		}
	}

	for _, want := range []string{"MESSAGE ", "RUN running", "TOOL →", "TOOL ✓", "ANSWER end_turn", "RUN completed"} {
		if kinds[want] == 0 {
			t.Errorf("the console never saw %q; it saw %v", want, kinds)
		}
	}
	if toolDone == nil || toolDone.Tool != "read_file" {
		t.Fatalf("the finished call is not named on its line: %+v", toolDone)
	}
	// What the tool printed travels with the line, as it always did for a
	// console.
	if toolDone.Output == "" {
		t.Error("the console was not shown what the tool returned")
	}
}

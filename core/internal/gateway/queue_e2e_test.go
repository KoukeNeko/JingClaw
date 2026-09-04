package gateway_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

// A second message in a channel waits for the first to be answered, and the
// channel is told so on the message itself. When the first is done — here,
// stopped — the second starts.
func TestASecondMessageWaitsItsTurnAndIsToldSo(t *testing.T) {
	h := newHarness(t, time.Hour) // the model never finishes on its own
	h.bind(t, "gateway", "user_1")
	ctx := context.Background()

	first, err := h.ingress.Accept(ctx, message("m1", "first", discordPrincipal("user_1")))
	if err != nil {
		t.Fatalf("accept first: %v", err)
	}
	second, err := h.ingress.Accept(ctx, message("m2", "second", discordPrincipal("user_1")))
	if err != nil {
		t.Fatalf("accept second: %v", err)
	}

	var queued *gateway.Dispatch
	deadline := time.Now().Add(3 * time.Second)
	for queued == nil && time.Now().Before(deadline) {
		for _, dispatch := range h.dispatches(t) {
			if dispatch.Kind != gateway.DispatchStatus || dispatch.RunID != second.RunID {
				continue
			}
			var status gateway.StatusPayload
			_ = json.Unmarshal([]byte(dispatch.Payload), &status)
			if status.State == "queued" {
				d := dispatch
				queued = &d
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if queued == nil {
		t.Fatal("the channel was never told the second message was waiting")
	}
	if queued.Target.SourceMessageID != "m2" {
		t.Errorf("the waiting mark is addressed to %q, not the waiting message", queued.Target.SourceMessageID)
	}
	if run, _ := h.store.Run(ctx, second.RunID); run.Status != domain.RunQueued {
		t.Errorf("the second run is %s while the first is still being answered", run.Status)
	}

	// The first is stopped; the second takes its turn.
	if _, err := h.runtime.InterruptRun(ctx, first.RunID, "stopped"); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	started := false
	deadline = time.Now().Add(3 * time.Second)
	for !started && time.Now().Before(deadline) {
		for _, dispatch := range h.dispatches(t) {
			if dispatch.Kind != gateway.DispatchStatus || dispatch.RunID != second.RunID {
				continue
			}
			var status gateway.StatusPayload
			_ = json.Unmarshal([]byte(dispatch.Payload), &status)
			if status.State == "provider_started" {
				started = true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !started {
		t.Fatal("the second message never started after the first was stopped")
	}
	_, _ = h.runtime.InterruptRun(ctx, second.RunID, "done")
}

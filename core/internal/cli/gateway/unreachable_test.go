package gateway

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

// Somebody whose message went nowhere is told so.
//
// The failure this closes lasted nine hours. The adapter marks every message
// it takes with a reaction before delivering it, so a gateway that could not
// reach the daemon produced exactly what a working one produces — the message
// acknowledged, and then nothing. The only way anybody found out was by
// asking why it had gone quiet.
func TestSomebodyIsToldWhenTheAgentCannotBeReached(t *testing.T) {
	link, platform := relayUnreachable()

	if err := link.Deliver(context.Background(), aMessage()); err == nil {
		t.Fatal("an undeliverable message was reported as delivered")
	}

	if len(platform.dispatches) != 1 {
		t.Fatalf("said %d things about it", len(platform.dispatches))
	}
	said := platform.dispatches[0].Payload
	if !strings.Contains(said, "did not get to it") {
		t.Errorf("what was said does not say the message went nowhere: %q", said)
	}
}

// And told it once, not once per message.
//
// A daemon that is down stays down. Answering every message with the same
// line turns one outage into a channel nobody can read.
func TestItSaysTheAgentIsUnreachableOnlyOnce(t *testing.T) {
	link, platform := relayUnreachable()

	for range 4 {
		_ = link.Deliver(context.Background(), aMessage())
	}

	if len(platform.dispatches) != 1 {
		t.Errorf("one outage produced %d messages", len(platform.dispatches))
	}
}

// Each room is told about its own outage.
//
// Somebody in a channel that has not been told has no reason to know, and
// silence is what this exists to end.
func TestEveryRoomIsToldAboutTheOutage(t *testing.T) {
	link, platform := relayUnreachable()

	first := aMessage()
	second := aMessage()
	second.Conversation.ChannelID = "chan_2"

	_ = link.Deliver(context.Background(), first)
	_ = link.Deliver(context.Background(), second)

	if len(platform.dispatches) != 2 {
		t.Fatalf("two rooms were told %d times", len(platform.dispatches))
	}
	if platform.dispatches[0].Target.ChannelID == platform.dispatches[1].Target.ChannelID {
		t.Error("both messages went to the same room")
	}
}

// Once it is back, the next outage is said again.
//
// Otherwise a channel is told once ever, and the second time the agent goes
// away it goes quiet exactly as before.
func TestTheNextOutageIsSaidAgain(t *testing.T) {
	link, platform := relayUnreachable()
	unreachable := link.client.(*refusing)

	_ = link.Deliver(context.Background(), aMessage())

	unreachable.code = 0
	if err := link.Deliver(context.Background(), aMessage()); err != nil {
		t.Fatalf("a working delivery failed: %v", err)
	}

	unreachable.code = connect.CodeUnavailable
	_ = link.Deliver(context.Background(), aMessage())

	if len(platform.dispatches) != 2 {
		t.Errorf("two outages either side of a recovery said it %d times",
			len(platform.dispatches))
	}
}

// relayUnreachable is a relay whose daemon cannot be reached.
func relayUnreachable() (*relay, *posted) {
	platform := &posted{}
	return &relay{
		client:    &refusing{code: connect.CodeUnavailable},
		poster:    platform,
		accountID: "main",
		logger:    slog.New(slog.DiscardHandler),
	}, platform
}

var _ = gateway.Dispatch{}

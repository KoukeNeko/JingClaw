package discord

import (
	"context"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"

	jcgateway "github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

// heard records what the adapter handed inward.
type heard struct {
	withdrawals []jcgateway.Withdrawal
}

func (h *heard) Deliver(context.Context, jcgateway.InboundMessage) error { return nil }
func (h *heard) Withdraw(_ context.Context, withdrawal jcgateway.Withdrawal) error {
	h.withdrawals = append(h.withdrawals, withdrawal)
	return nil
}

func partialEmoji(name string) discord.PartialEmoji {
	return discord.PartialEmoji{Name: &name}
}

// pressed is a reaction event with only what the handler reads.
func pressed(user, message snowflake.ID, emoji string) *events.MessageReactionAdd {
	return &events.MessageReactionAdd{
		GenericReaction: &events.GenericReaction{
			UserID:    user,
			MessageID: message,
			ChannelID: 987654321098765432,
			Emoji:     partialEmoji(emoji),
		},
	}
}

func listening(t *testing.T) (*Adapter, *heard) {
	t.Helper()
	sink := &heard{}
	adapter := New(Config{AccountID: "main"}, sink, nil)
	adapter.selfID.Store(uint64(botUser))
	return adapter, sink
}

// Pressing the waiting mark on a message is handed inward as that person
// taking that message back, under the key the message was claimed with.
func TestPressingTheWaitingMarkTakesTheMessageBack(t *testing.T) {
	adapter, sink := listening(t)
	message := snowflake.ID(111111111111111111)

	adapter.onReaction(pressed(askerUser, message, waitingMark))

	if len(sink.withdrawals) != 1 {
		t.Fatalf("%d withdrawals, want one", len(sink.withdrawals))
	}
	got := sink.withdrawals[0]
	if got.InboundKey != "discord:111111111111111111" {
		t.Errorf("the key is %q; the message was claimed under discord:<id>", got.InboundKey)
	}
	if got.MessageID != message.String() {
		t.Errorf("the message is %q, want %s", got.MessageID, message)
	}
	if got.Principal.ID != askerUser.String() || got.Principal.AccountID != "main" {
		t.Errorf("the principal is %+v", got.Principal)
	}
}

// Any other reaction is somebody reacting, and the bot's own mark landing is
// the bot, not a person.
func TestOtherReactionsAreNotWithdrawals(t *testing.T) {
	adapter, sink := listening(t)
	message := snowflake.ID(111111111111111111)

	adapter.onReaction(pressed(askerUser, message, "👍"))
	adapter.onReaction(pressed(askerUser, message, "🛑"))
	adapter.onReaction(pressed(botUser, message, waitingMark))

	if len(sink.withdrawals) != 0 {
		t.Errorf("handed inward as withdrawals: %+v", sink.withdrawals)
	}
}

// A message taken back is marked as put away, and the waiting mark comes off.
// Nothing is said: the person who took it back is the one who would read it.
func TestAMessageTakenBackIsPutAway(t *testing.T) {
	adapter, posted := stubDiscord(t)

	messages, err := adapter.Post(t.Context(), statusFor(t, "withdrawn"))
	if err != nil {
		t.Fatalf("withdrawn: %v", err)
	}
	if len(messages) != 0 {
		t.Errorf("a withdrawal posted %d messages; it should say nothing", len(messages))
	}
	added, removed := reactions(*posted)
	if !has(added, withdrawnMark) {
		t.Errorf("the message was not marked as put away: added %v", added)
	}
	if !has(removed, waitingMark) {
		t.Errorf("the waiting mark stayed on: removed %v", removed)
	}
	if has(added, "🛑") {
		t.Errorf("a withdrawal was marked as stopped: added %v", added)
	}
}

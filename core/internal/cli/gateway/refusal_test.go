package gateway

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

// posted records what the relay tried to say, in place of a platform.
type posted struct {
	dispatches []gateway.Dispatch
	fails      bool
}

func (p *posted) Run(context.Context) error { return nil }

func (p *posted) Post(_ context.Context, dispatch gateway.Dispatch) ([]string, error) {
	p.dispatches = append(p.dispatches, dispatch)
	if p.fails {
		return nil, errors.New("the platform said no")
	}
	return []string{"posted_1"}, nil
}

// refusing is a control plane that refuses everything with one code.
type refusing struct {
	code connect.Code
	sent int
}

// DeliverInbound refuses with whatever code it was given, and accepts when it
// was given none — so one fake can play a daemon that comes back.
func (r *refusing) DeliverInbound(
	context.Context, *connect.Request[controlv1.DeliverInboundRequest],
) (*connect.Response[controlv1.DeliverInboundResponse], error) {
	r.sent++
	if r.code == 0 {
		return connect.NewResponse(&controlv1.DeliverInboundResponse{
			SessionId: "ses_1", RunId: "run_1",
		}), nil
	}
	return nil, connect.NewError(r.code, errors.New("refused"))
}

func (r *refusing) SubscribeDispatches(
	context.Context, *connect.Request[controlv1.SubscribeDispatchesRequest],
) (*connect.ServerStreamForClient[controlv1.SubscribeDispatchesResponse], error) {
	return nil, errors.New("not used here")
}

func (r *refusing) AcknowledgeDispatch(
	context.Context, *connect.Request[controlv1.AcknowledgeDispatchRequest],
) (*connect.Response[controlv1.AcknowledgeDispatchResponse], error) {
	return nil, errors.New("not used here")
}

func (r *refusing) DeliverDecision(
	context.Context, *connect.Request[controlv1.DeliverDecisionRequest],
) (*connect.Response[controlv1.DeliverDecisionResponse], error) {
	return nil, errors.New("not used here")
}

func relayRefusing(code connect.Code) (*relay, *posted, *refusing) {
	platform := &posted{}
	control := &refusing{code: code}

	return &relay{
		client:    control,
		poster:    platform,
		accountID: "main",
		logger:    slog.New(slog.DiscardHandler),
	}, platform, control
}

func aMessage() gateway.InboundMessage {
	return gateway.InboundMessage{
		Conversation: gateway.ConversationRef{
			Platform: "discord", AccountID: "main", ChannelID: "chan_1",
		},
		Principal: gateway.Principal{ID: "someone", DisplayName: "Somebody"},
		Text:      "hello",
	}
}

// TestSomebodyRefusedIsToldSo is why this exists.
//
// Being ignored by something you spoke to directly is indistinguishable from
// it being broken. The adapter only delivers messages that address the bot,
// so everything refused here was said to it.
func TestSomebodyRefusedIsToldSo(t *testing.T) {
	relay, platform, _ := relayRefusing(connect.CodePermissionDenied)

	if err := relay.Deliver(context.Background(), aMessage()); err != nil {
		t.Fatalf("a refusal was reported as a failure: %v", err)
	}

	if len(platform.dispatches) != 1 {
		t.Fatalf("said %d things, want one", len(platform.dispatches))
	}
	said := platform.dispatches[0]
	if said.Target.ChannelID != "chan_1" {
		t.Errorf("it answered in %q", said.Target.ChannelID)
	}
	if !strings.Contains(said.Payload, "can't take that here") {
		t.Errorf("what it said does not explain anything: %q", said.Payload)
	}
}

// TestItDoesNotReadOutWhoMay keeps the list the operator's to share.
//
// A bot telling whoever asks which accounts are permitted is a different
// decision from the one somebody made when they wrote the list down.
func TestItDoesNotReadOutWhoMay(t *testing.T) {
	relay, platform, _ := relayRefusing(connect.CodePermissionDenied)

	if err := relay.Deliver(context.Background(), aMessage()); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if len(platform.dispatches) != 1 {
		t.Fatal("nothing was said")
	}

	said := platform.dispatches[0].Payload
	for _, leak := range []string{"675724351156518953", "users =", "allowlist", "binding"} {
		if strings.Contains(said, leak) {
			t.Errorf("it mentions %q: %s", leak, said)
		}
	}
}

// TestAnUnboundChannelStaysSilent is the refusal that must not answer.
//
// The bot has no business announcing itself in every room it can see, and a
// message in a channel nobody bound was addressed to something that has not
// been asked to be there. "You may not" and "not here" are different.
func TestAnUnboundChannelStaysSilent(t *testing.T) {
	relay, platform, control := relayRefusing(connect.CodeFailedPrecondition)

	if err := relay.Deliver(context.Background(), aMessage()); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	// The precondition: the message really was refused, so silence here is a
	// decision rather than the message having been accepted.
	if control.sent != 1 {
		t.Fatalf("the message never reached the control plane")
	}
	if len(platform.dispatches) != 0 {
		t.Errorf("it spoke in a channel nobody bound: %+v", platform.dispatches)
	}
}

// TestFailingToExplainIsNotTheCallersProblem keeps a working refusal from
// looking like a broken gateway.
func TestFailingToExplainIsNotTheCallersProblem(t *testing.T) {
	relay, platform, _ := relayRefusing(connect.CodePermissionDenied)
	platform.fails = true

	if err := relay.Deliver(context.Background(), aMessage()); err != nil {
		t.Errorf("a refusal whose explanation failed to send became an error: %v", err)
	}
}

package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// Runtime is the slice of the agent runtime the ingress needs.
//
// Deliberately narrow: an ingress may start work and read what came of it, and
// nothing else. Handing it the whole runtime would let a path that begins with
// untrusted text reach run lifecycle and permissions.
type Runtime interface {
	CreateSession(ctx context.Context, title string) (domain.Session, error)

	// SendTurnTo starts a run whose output goes back to the conversation it
	// came from rather than to a control client.
	SendTurnTo(ctx context.Context, session domain.SessionID, text string,
		origin domain.RunOrigin, targets []domain.DeliveryTarget) (domain.RunID, domain.MessageID, error)
}

// ProfileBinder records which permission profile a session runs under.
type ProfileBinder interface {
	UseProfile(session domain.SessionID, name string) error
}

// Ingress turns a platform message into agent work, or refuses it.
//
// Every refusal here is deliberate. The default for an unconfigured channel,
// an unlisted account or an overheard remark is no, because this is the one
// place where text written by anyone who can type into a channel meets a
// process that can change files.
type Ingress struct {
	Store   Store
	Runtime Runtime
	Binder  ProfileBinder

	NewSessionTitle func(InboundMessage) string
	Now             func() time.Time
	Logger          *slog.Logger
}

// Accepted describes work that was started.
type Accepted struct {
	SessionID domain.SessionID
	RunID     domain.RunID

	// Duplicate marks a message that had already been handled. The caller
	// should treat it as success and not post anything: the reply to the
	// original is already on its way.
	Duplicate bool
}

// Accept validates a message and starts a run.
func (i *Ingress) Accept(ctx context.Context, message InboundMessage) (Accepted, error) {
	if !message.Trigger.IsExplicit() {
		// Overheard text is not a request. Acting on it would mean anything
		// typed near the agent could set it working.
		return Accepted{}, ErrNotExplicit
	}
	if message.IdempotencyKey == "" {
		return Accepted{}, errors.New("gateway: message has no idempotency key")
	}

	binding, err := i.Store.Binding(ctx,
		message.Conversation.Platform,
		message.Conversation.AccountID,
		message.Conversation.TenantID,
		message.Conversation.ChannelID,
	)
	if err != nil {
		// An unbound channel is not an error to paper over: without a binding
		// there is no workspace to work in and nobody who said this channel
		// may reach one.
		return Accepted{}, err
	}

	if !binding.Permits(message.Principal) {
		i.logger().Warn("refused a gateway message",
			"platform", string(message.Conversation.Platform),
			"channel", message.Conversation.ChannelID,
			"principal", message.Principal.ID,
			"reason", "not permitted by the binding",
		)
		return Accepted{}, ErrNotPermitted
	}

	session, err := i.sessionFor(ctx, message, binding)
	if err != nil {
		return Accepted{}, err
	}

	// The claim is taken before the run starts. A platform redelivering after
	// a reconnect must not produce a second run doing the same work, and
	// claiming afterwards would leave a window where it could.
	fresh, err := i.Store.ClaimInbound(ctx,
		message.IdempotencyKey, message.Conversation.AccountID, session, "", i.now())
	if err != nil {
		return Accepted{}, err
	}
	if !fresh {
		return Accepted{SessionID: session, Duplicate: true}, nil
	}

	// The text is the platform's, so it is the platform's account that gets
	// recorded as the origin, not the operator running the daemon.
	origin := message.Principal.Origin()

	// The reply belongs in the conversation the request came from.
	targets := []domain.DeliveryTarget{message.Conversation.DeliveryTarget()}

	runID, _, err := i.Runtime.SendTurnTo(ctx, session, message.Text, origin, targets)
	if err != nil {
		return Accepted{}, err
	}

	return Accepted{SessionID: session, RunID: runID}, nil
}

// sessionFor finds or creates the session for a conversation.
//
// A thread maps to one session, so a follow-up continues the same
// conversation instead of starting a fresh one that has forgotten everything.
func (i *Ingress) sessionFor(ctx context.Context, message InboundMessage, binding Binding) (domain.SessionID, error) {
	key := message.Conversation.Key()

	existing, found, err := i.Store.SessionForConversation(ctx, key)
	if err != nil {
		return "", err
	}
	if found {
		return existing, nil
	}

	created, err := i.Runtime.CreateSession(ctx, i.title(message))
	if err != nil {
		return "", err
	}
	session := created.ID

	// The profile is bound before any work starts. A session that ran even one
	// turn under the wrong profile would have done so with the wrong answers
	// to every permission question.
	if i.Binder != nil {
		profile := binding.PermissionProfile
		if profile == "" {
			profile = "gateway"
		}
		if err := i.Binder.UseProfile(session, profile); err != nil {
			return "", fmt.Errorf("gateway: binding %s names an unknown profile: %w", binding.ID, err)
		}
	}

	if err := i.Store.LinkConversation(ctx, key, session, binding.ID, i.now()); err != nil {
		if errors.Is(err, ErrAlreadyProcessed) {
			// Another message for the same thread won the race. Its session is
			// the one to use; ours is discarded rather than becoming a second
			// history for one conversation.
			winner, found, lookupErr := i.Store.SessionForConversation(ctx, key)
			if lookupErr == nil && found {
				return winner, nil
			}
		}
		return "", err
	}

	return session, nil
}

func (i *Ingress) title(message InboundMessage) string {
	if i.NewSessionTitle != nil {
		return i.NewSessionTitle(message)
	}

	text := message.Text
	const maxTitle = 60
	if len(text) > maxTitle {
		text = text[:maxTitle] + "…"
	}
	if text == "" {
		text = string(message.Conversation.Platform) + " conversation"
	}
	return text
}

func (i *Ingress) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}

func (i *Ingress) logger() *slog.Logger {
	if i.Logger != nil {
		return i.Logger
	}
	return slog.Default()
}

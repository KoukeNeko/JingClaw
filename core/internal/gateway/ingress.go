package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/artifact"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/media"
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
	SendTurnTo(ctx context.Context, session domain.SessionID, turn domain.Turn) (domain.RunID, domain.MessageID, error)
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
// ArtifactStore is where the bytes that arrive with a message are kept.
//
// Declared here rather than imported as a concrete type, so the ingress
// depends on the one thing it does — putting bytes somewhere durable — and not
// on everything an artifact store can do.
type ArtifactStore interface {
	PutBytes(ctx context.Context, content []byte, mediaType string) (artifact.Ref, error)
}

// ArtifactReader is the other direction, for handing something back.
//
// A separate interface an ArtifactStore may also satisfy, so that storing what
// arrives and reading what was stored are separate capabilities. An ingress
// that only receives does not acquire the ability to hand things out by
// having somewhere to put them.
type ArtifactReader interface {
	Stat(id string) (artifact.Ref, error)
	ReadRange(id string, offset, limit int64) ([]byte, int64, error)
}

type Ingress struct {
	Store     Store
	Runtime   Runtime
	Binder    ProfileBinder
	Artifacts ArtifactStore

	// Decisions is the extra reach needed to answer an approval or a
	// question. Left nil, a console binding still runs under its profile and
	// a button still appears where one is configured, but both answer that
	// deciding is unavailable rather than silently doing nothing.
	Decisions DecidingRuntime

	// NewDispatchID names a message this program sends on its own behalf,
	// rather than one produced by a run.
	NewDispatchID func() string

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

	// A console channel can be told things rather than asked them. A command
	// is answered here and never becomes a turn: the agent reading its own
	// console as something somebody said to it is how an instruction to this
	// program turns into a prompt for the model.
	if binding.PermissionProfile == ConsoleProfileName {
		if command, ok := parseConsoleCommand(message.Text); ok {
			// Claimed like any other message, so a platform redelivering
			// after a reconnect cannot decide the same thing twice.
			fresh, claimErr := i.Store.ClaimInbound(ctx,
				message.IdempotencyKey, message.Conversation.AccountID, session, "", i.now())
			if claimErr != nil {
				return Accepted{}, claimErr
			}
			if !fresh {
				return Accepted{SessionID: session, Duplicate: true}, nil
			}
			return Accepted{SessionID: session},
				i.handleConsole(ctx, message, binding, session, command)
		}
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

	// The reply belongs in the conversation the request came from. Keep the
	// source message ID in the target so a gateway can acknowledge it when the
	// provider call actually begins.
	message.Conversation.SourceMessageID = message.PlatformMessageID
	targets := []domain.DeliveryTarget{message.Conversation.DeliveryTarget()}

	stored, err := i.storeAttachments(ctx, message)
	if err != nil {
		return Accepted{}, err
	}

	runID, _, err := i.Runtime.SendTurnTo(ctx, session, domain.Turn{
		Text:        message.Text,
		Origin:      origin,
		Targets:     targets,
		Attachments: stored,
	})
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

	// Said where the rule applies, the first time somebody uses the channel.
	// A boundary that lives only in a configuration file is one everybody in
	// the room will assume the shape of instead.
	if binding.PermissionProfile == ConsoleProfileName {
		if err := i.say(ctx, message, session, consoleMOTD(binding)); err != nil {
			// Not fatal: failing to post an explanation is no reason to
			// refuse the work it was explaining.
			i.log().Warn("could not post the console notice",
				"binding_id", binding.ID, "error", err)
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

// log is the ingress logger, or a silent one when none was given.
func (i *Ingress) log() *slog.Logger {
	if i.Logger != nil {
		return i.Logger
	}
	return slog.New(slog.DiscardHandler)
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

// storeAttachments puts what arrived with a message into the artifact store.
//
// What is recorded in the log is a reference, never the bytes: an image is
// large and the log is replayed on every turn. The store is content-addressed,
// so the same screenshot sent twice costs nothing the second time.
//
// An attachment the adapter did not fetch is still recorded, with no artifact
// behind it. Dropping it silently would leave a message that makes no sense on
// its own and no way to find out why.
func (i *Ingress) storeAttachments(
	ctx context.Context,
	message InboundMessage,
) ([]domain.Attachment, error) {
	if len(message.Attachments) == 0 {
		return nil, nil
	}

	kept := 0
	stored := make([]domain.Attachment, 0, len(message.Attachments))

	for _, attachment := range message.Attachments {
		recorded := domain.Attachment{
			Name:      attachment.Name,
			MediaType: media.CanonicalMediaType(attachment.ContentType),
			Size:      attachment.Size,
		}

		switch {
		case len(attachment.Data) == 0 || i.Artifacts == nil:
			// The adapter did not fetch it, or there is nowhere to put it.

		case kept >= media.MaxImagesPerMessage:
			i.Logger.Info("not keeping any more attachments from one message",
				"name", attachment.Name, "limit", media.MaxImagesPerMessage)

		default:
			// The declared type is not believed: it came from the platform,
			// which got it from whoever uploaded the file.
			mediaType, err := media.CheckImage(attachment.ContentType, attachment.Data)
			if err != nil {
				i.Logger.Info("not keeping an attachment",
					"name", attachment.Name, "reason", err)
				break
			}

			// The bytes are made durable before the event that refers to them.
			// A crash between the two leaves an artifact nobody points at,
			// which is litter; the other order leaves a conversation that
			// cannot be replayed, which is a broken promise.
			ref, err := i.Artifacts.PutBytes(ctx, attachment.Data, mediaType)
			if err != nil {
				// Keeping the record without the bytes is better than refusing
				// the whole message: somebody asked a question, and the part of
				// it that is text still works.
				i.Logger.Warn("could not keep an attachment",
					"name", attachment.Name, "error", err)
				break
			}

			recorded.ArtifactID = ref.ID
			recorded.MediaType = mediaType
			recorded.Size = ref.Size
			kept++
		}

		stored = append(stored, recorded)
	}

	return stored, nil
}

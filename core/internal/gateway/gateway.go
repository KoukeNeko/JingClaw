// Package gateway defines the boundary between JingClaw and external
// messaging platforms.
//
// This is a third plane, distinct from the control plane the GUIs and the CLI
// speak. A control client is an operator at their own machine; a gateway
// carries traffic from Discord, Slack or email, where identity belongs to
// somebody else's service and the text arrives from anyone who can type into a
// channel. Treating the two the same would mean a chat message and a
// deliberate command from the machine's owner carried equal weight.
//
// Nothing here decides anything. A gateway asserts "this platform account said
// this"; whether that account may touch a workspace is settled by the policy
// engine, from a binding an operator wrote down.
package gateway

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// Platform names an external service.
type Platform string

const (
	PlatformDiscord Platform = "discord"

	// Declared to fix the vocabulary, not because adapters exist. The
	// abstraction is worth nothing if it was shaped by exactly one platform.
	PlatformSlack    Platform = "slack"
	PlatformTelegram Platform = "telegram"
	PlatformLINE     Platform = "line"
	PlatformGitHub   Platform = "github"
	PlatformEmail    Platform = "email"
)

// Principal is an identity owned by an external platform.
//
// Authorization reads ID and nothing else. DisplayName can be changed by its
// owner at any moment, so a rule written against it is a rule anybody can
// satisfy by renaming themselves.
type Principal struct {
	Platform  Platform
	AccountID string
	TenantID  string

	// ID is the platform's stable identifier: a Discord snowflake, a Slack
	// user ID. Never a name.
	ID string

	DisplayName string

	// IsBot marks automation. Acting on it is how two bots talk each other
	// into an unbounded loop.
	IsBot bool

	// Claims are platform-specific assertions such as roles, kept opaque
	// because a Discord role and a GitHub collaborator status are not the same
	// kind of thing and flattening them would invent a hierarchy nobody meant.
	Claims []Claim
}

type Claim struct {
	// Namespace identifies the meaning, e.g. "discord.role".
	Namespace string
	Value     string
}

// HasClaim reports whether the principal carries a claim.
func (p Principal) HasClaim(namespace, value string) bool {
	for _, claim := range p.Claims {
		if claim.Namespace == namespace && claim.Value == value {
			return true
		}
	}
	return false
}

// ConversationRef locates a conversation on a platform.
//
// Thread is what usually maps to a session: it is the finest-grained container
// most platforms offer, and coarser mappings mix unrelated work into one
// history.
type ConversationRef struct {
	Platform  Platform
	AccountID string
	TenantID  string
	ChannelID string
	ThreadID  string

	// SourceMessageID identifies the platform message that started the run.
	// It is presentation metadata for acknowledgements and is not part of the
	// conversation key.
	SourceMessageID string

	// RootMessageID identifies a conversation on a platform that has no
	// threads and where a reply chain is the unit instead — email, say.
	//
	// It must never be set to the id of the message being delivered. Doing so
	// gives every message a key of its own, which is not a conversation: it is
	// an agent that has never heard of you, once per message.
	//
	// Kept here rather than deleted because a platform that works that way is
	// the reason the field exists, and the next adapter will need it.
	RootMessageID string
}

// Key returns a stable identifier for the conversation.
func (c ConversationRef) Key() string {
	thread := c.ThreadID
	if thread == "" {
		thread = c.RootMessageID
	}
	return string(c.Platform) + ":" + c.AccountID + ":" + c.TenantID + ":" + c.ChannelID + ":" + thread
}

// Attachment is a file that arrived with a message. The bytes are fetched by
// the adapter on demand; a reference is enough to decide whether they are
// wanted at all.
type Attachment struct {
	ID          string
	Name        string
	ContentType string
	Size        int64

	// Data is the file itself, fetched by the adapter before the message was
	// handed inward.
	//
	// The bytes travel rather than a link, because a link to somebody else's
	// service expires and is signed for a client this daemon is not. A
	// conversation that cannot be replayed next month is not a durable one,
	// and the adapter is the only part of this system that is supposed to talk
	// to the platform at all.
	//
	// Empty when the adapter declined to fetch it — too large, or a kind of
	// file this agent does not keep. The rest of the fields still arrive, so
	// the model can be told something was sent rather than being left to
	// wonder why a message makes no sense on its own.
	Data []byte
}

// InboundMessage is a normalized message from a platform.
type InboundMessage struct {
	PlatformMessageID string

	// IdempotencyKey deduplicates redelivery. Platforms resend on reconnect,
	// and without this a dropped connection turns one request into several
	// runs.
	IdempotencyKey string

	Principal    Principal
	Conversation ConversationRef

	Text        string
	Attachments []Attachment

	// Trigger records why this arrived, which is the difference between a
	// deliberate request and a message that merely happened nearby.
	Trigger Trigger

	OccurredAt time.Time
}

type Trigger string

const (
	// TriggerMention: the bot was named.
	TriggerMention Trigger = "mention"

	// TriggerCommand: an explicit command such as a slash command.
	TriggerCommand Trigger = "command"

	// TriggerDirect: a direct message.
	TriggerDirect Trigger = "direct"

	// TriggerAmbient: overheard in a channel. Never a reason to act on its own.
	TriggerAmbient Trigger = "ambient"
)

// IsExplicit reports whether the message was addressed to the agent.
//
// Ambient traffic is not a request. Acting on it would mean anything typed
// near the bot could start work.
func (t Trigger) IsExplicit() bool {
	return t == TriggerMention || t == TriggerCommand || t == TriggerDirect
}

// Capability describes what a platform can actually do.
//
// Adapters report this rather than the runtime assuming, because the
// differences are real: Discord has no true streaming and edits a message
// instead, Telegram has a native draft, email has neither. Pretending they are
// alike produces output that reads badly everywhere.
type Capability struct {
	MaxTextLength int

	CanEdit        bool
	CanAttachFiles bool
	CanThread      bool

	CanButtons   bool
	CanEphemeral bool

	// Incremental is how partial output should be shown, if at all.
	Incremental Incremental

	MaxAttachmentBytes int64
}

type Incremental string

const (
	// IncrementalNone: post only finished output.
	IncrementalNone Incremental = "none"

	// IncrementalEdit: post once, then edit as more arrives.
	IncrementalEdit Incremental = "edit"

	// IncrementalNative: the platform has its own draft mechanism.
	IncrementalNative Incremental = "native"
)

// Binding is an operator's decision that a conversation may reach a workspace.
//
// Nothing is inferred. A channel named after a project does not grant access
// to it: the mapping is written down, or the conversation cannot start work.
type Binding struct {
	ID string

	Platform  Platform
	AccountID string
	TenantID  string
	ChannelID string

	WorkspaceID string

	// PermissionProfile names the policy applied to runs from here. Gateway
	// traffic gets a stricter one than a local operator.
	PermissionProfile string

	// AllowedPrincipals and AllowedClaims decide who may trigger work. Empty
	// means nobody: an unconfigured binding must not be an open door.
	AllowedPrincipals []string
	AllowedClaims     []Claim

	CreatedAt time.Time
}

// Permits reports whether a principal may trigger work through this binding.
func (b Binding) Permits(principal Principal) bool {
	// A bot triggering the agent is how two automations talk each other into
	// an unbounded loop, so it is refused regardless of any allowlist.
	if principal.IsBot {
		return false
	}

	for _, allowed := range b.AllowedPrincipals {
		if allowed == principal.ID {
			return true
		}
	}
	for _, claim := range b.AllowedClaims {
		if principal.HasClaim(claim.Namespace, claim.Value) {
			return true
		}
	}
	return false
}

// Origin converts a principal into the runtime's record of who started a run.
func (p Principal) Origin() domain.RunOrigin {
	return domain.RunOrigin{
		Kind: domain.OriginGateway,
		Principal: &domain.ExternalPrincipal{
			Platform:    string(p.Platform),
			AccountID:   p.AccountID,
			TenantID:    p.TenantID,
			PrincipalID: p.ID,
			DisplayName: p.DisplayName,
		},
	}
}

// DeliveryTarget converts a conversation into the runtime's record of where a
// run's output should go.
func (c ConversationRef) DeliveryTarget() domain.DeliveryTarget {
	encoded, err := json.Marshal(c)
	if err != nil {
		// The struct is plain data, so this cannot fail in practice; the key
		// alone is still enough to identify the conversation.
		return domain.DeliveryTarget{Kind: string(c.Platform), Ref: c.Key()}
	}
	return domain.DeliveryTarget{Kind: string(c.Platform), Ref: string(encoded)}
}

// ConversationFromTarget recovers a conversation from a delivery target.
func ConversationFromTarget(target domain.DeliveryTarget) (ConversationRef, bool) {
	var ref ConversationRef
	if err := json.Unmarshal([]byte(target.Ref), &ref); err != nil {
		return ConversationRef{}, false
	}
	return ref, ref.Platform != ""
}

// Plane is both halves of the gateway: messages coming in and replies going
// out.
//
// They are constructed together because a system with only one half is broken
// in a way nothing catches. An ingress without a projector accepts requests
// and answers into the void; a projector without an ingress has nothing to
// project. Both compile perfectly well alone, and the failure only shows up
// when somebody is waiting in a channel for a reply that will never arrive.
type Plane struct {
	Ingress   *Ingress
	Projector *Projector
}

// NewPlane wires the gateway.
//
// console may be nil, in which case a binding naming the console profile still
// runs under it but cannot decide approvals from the channel. That is a
// deployment choice rather than a mistake: a console channel that can only
// look is a useful thing to have.
func NewPlane(
	store Store,
	rt Runtime,
	binder ProfileBinder,
	artifacts ArtifactStore,
	console ConsoleRuntime,
	newID func() string,
	now func() time.Time,
	logger *slog.Logger,
) *Plane {
	if now == nil {
		now = time.Now
	}

	return &Plane{
		Ingress: &Ingress{
			Store:         store,
			Runtime:       rt,
			Binder:        binder,
			Artifacts:     artifacts,
			Console:       console,
			NewDispatchID: newID,
			Now:           now,
			Logger:        logger,
		},
		Projector: NewProjector(store, newID, now),
	}
}

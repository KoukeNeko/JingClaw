package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

var (
	ErrBindingNotFound  = errors.New("gateway: no binding for this channel")
	ErrDispatchNotFound = errors.New("gateway: dispatch not found")
	ErrAlreadyDelivered = errors.New("gateway: dispatch was already delivered")
	ErrNotPermitted     = errors.New("gateway: this principal may not trigger work here")
	ErrNotExplicit      = errors.New("gateway: this message was not addressed to the agent")
	ErrAlreadyProcessed = errors.New("gateway: this message has already been handled")
)

// DispatchSeq orders deliveries for one gateway account.
//
// It is not a session's event sequence and not a platform's own sequence.
// Keeping the three apart means a number in a log or a resume request can only
// mean one thing.
type DispatchSeq uint64

type DispatchKind string

const (
	// DispatchMessage is agent output to post.
	DispatchMessage DispatchKind = "message"

	// DispatchApproval asks the conversation to decide about a tool call.
	DispatchApproval DispatchKind = "approval"

	// DispatchQuestion asks the conversation something the agent needs to
	// know. Distinct from an approval because the two want different
	// answers: an approval is allowed or denied, and this is answered with
	// words or a choice.
	DispatchQuestion DispatchKind = "question"

	// DispatchStatus reports a run beginning or ending, so a channel is not
	// left wondering whether anything is happening.
	DispatchStatus DispatchKind = "status"

	// DispatchLog is one thing that happened, for a console channel.
	//
	// Distinct from a status line because it accumulates rather than being
	// rewritten: a status answers "what now", and a log answers "what
	// happened", and a channel showing one as the other loses whichever it
	// overwrote.
	DispatchLog DispatchKind = "log"
)

// AllDispatchKinds is every kind a dispatch can have.
//
// A kind has to be understood in more than one place — encoded onto the wire
// by the daemon, decoded by the gateway, rendered by the adapter — and the
// failure when one of them is missed is not a compile error. It is a dispatch
// that reaches the far side as an empty kind, cannot be rendered, and is
// offered again on every reconnect for the rest of the deployment's life.
// Two kinds spent weeks that way. This list is what the tests check against.
func AllDispatchKinds() []DispatchKind {
	return []DispatchKind{
		DispatchMessage, DispatchApproval, DispatchQuestion, DispatchStatus, DispatchLog,
	}
}

// Dispatch is one thing to deliver to a platform.
type Dispatch struct {
	ID  string
	Seq DispatchSeq

	AccountID string
	SessionID domain.SessionID
	RunID     domain.RunID

	Target ConversationRef

	Kind    DispatchKind
	Payload string

	CreatedAt   time.Time
	DeliveredAt *time.Time

	// PlatformMessageIDs are what the platform called the messages this
	// created, recorded so a later edit can find them.
	PlatformMessageIDs []string
}

func (d Dispatch) IsDelivered() bool { return d.DeliveredAt != nil }

// Store is the gateway plane's persistence.
type Store interface {
	// Bindings

	UpsertBinding(ctx context.Context, binding Binding) error
	Binding(ctx context.Context, platform Platform, accountID, tenantID, channelID string) (Binding, error)
	ListBindings(ctx context.Context) ([]Binding, error)
	DeleteBinding(ctx context.Context, id string) error

	// Conversations

	// LinkConversation records that a conversation maps to a session. It fails
	// rather than overwrites, so two messages arriving at once cannot each
	// create a session for the same thread.
	LinkConversation(ctx context.Context, key string, session domain.SessionID, bindingID string, at time.Time) error
	SessionForConversation(ctx context.Context, key string) (domain.SessionID, bool, error)

	// Inbound deduplication

	// ClaimInbound records a message as handled. It returns false when the key
	// was already present, which is how a redelivered message is recognised
	// without doing its work twice.
	ClaimInbound(ctx context.Context, key, accountID string, session domain.SessionID, run domain.RunID, at time.Time) (bool, error)

	// Outbox

	EnqueueDispatch(ctx context.Context, dispatch Dispatch) (DispatchSeq, error)

	// DispatchesAfter returns undelivered dispatches after a cursor, which is
	// how a gateway resumes without replaying what it already posted.
	DispatchesAfter(ctx context.Context, accountID string, after DispatchSeq, limit int) ([]Dispatch, error)

	// MarkDelivered settles a dispatch. It must refuse a second attempt, so a
	// duplicate acknowledgement cannot make a reply appear twice.
	MarkDelivered(ctx context.Context, id string, platformMessageIDs []string, at time.Time) error

	// Sessions

	// Session is read for one thing here: which model a session answered
	// with, so a run summary names the one that actually did rather than the
	// daemon's default.
	Session(ctx context.Context, id domain.SessionID) (domain.Session, error)
}

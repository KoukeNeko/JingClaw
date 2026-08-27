package runtime

import (
	"context"
	"encoding/json"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// buildConversation reconstructs the message list for a session from its event
// log.
//
// The log is the source of truth, so the conversation is derived from it
// rather than held in memory alongside it. That is what lets a session survive
// a restart with its history intact, and it means there is exactly one place
// where "what the model has seen" is defined.
func (r *Runtime) buildConversation(ctx context.Context, sessionID domain.SessionID) ([]provider.Message, error) {
	bounded, err := r.buildBoundedConversation(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return plainMessages(bounded), nil
}

// boundedMessage pairs a message with the last event folded into it.
//
// Compaction has to name a point in the log, not a point in a message list,
// because the message list is derived and the log is not. Carrying the
// sequence alongside each message is what lets a cut chosen by size be
// recorded as something a later replay can act on.
type boundedMessage struct {
	Message provider.Message
	LastSeq domain.Seq
}

func plainMessages(bounded []boundedMessage) []provider.Message {
	messages := make([]provider.Message, 0, len(bounded))
	for _, item := range bounded {
		messages = append(messages, item.Message)
	}
	return messages
}

// buildBoundedConversation replays the log, starting from the most recent
// compaction if there is one.
//
// Only the most recent matters: each summary already accounts for everything
// before it, earlier summaries included. Events older than its ThroughSeq are
// still in the log and still visible to clients; they are simply no longer
// part of what the model sees.
func (r *Runtime) buildBoundedConversation(
	ctx context.Context,
	sessionID domain.SessionID,
) ([]boundedMessage, error) {
	events, err := r.opts.Store.ListAfter(ctx, sessionID, 0, 0)
	if err != nil {
		return nil, err
	}

	var (
		summary    string
		throughSeq domain.Seq
	)
	for _, event := range events {
		if compacted, ok := event.Payload.(domain.ConversationCompacted); ok {
			summary, throughSeq = compacted.Summary, compacted.ThroughSeq
		}
	}

	builder := &conversationBuilder{}
	if summary != "" {
		builder.seed(summary, throughSeq)
	}

	for _, event := range events {
		if event.Seq <= throughSeq {
			continue
		}
		if _, ok := event.Payload.(domain.ConversationCompacted); ok {
			// The record of a compaction is not itself part of the
			// conversation; its effect was applied above.
			continue
		}
		builder.apply(event)
	}

	return builder.finish(), nil
}

// summaryPreamble frames the summary as what it is. A model handed a condensed
// history with no explanation tends to treat it as something the user just
// said.
const summaryPreamble = "Summary of the earlier part of this conversation, " +
	"which is no longer included in full:\n\n"

// conversationBuilder folds events into messages.
//
// Assistant text arrives as several coalesced deltas per message, and tool
// results arrive interleaved with them, so the builder accumulates until the
// shape of the turn changes rather than emitting a message per event.
//
// Each pending buffer carries the sequence of the last event put into it, so
// the message it eventually becomes can say where in the log it ends. Taking
// the sequence at flush time instead would attribute a message to whatever
// event happened to trigger the flush.
type conversationBuilder struct {
	messages []boundedMessage

	pendingText      string
	pendingToolCalls []provider.ContentBlock
	pendingResults   []provider.ContentBlock

	assistantSeq domain.Seq
	resultsSeq   domain.Seq
}

// seed starts the conversation from a summary rather than from the beginning.
//
// Its sequence is the one the summary replaces, not the sequence of the event
// that recorded it, so that a later compaction folding this message reports a
// point the replay can act on.
func (b *conversationBuilder) seed(summary string, throughSeq domain.Seq) {
	b.messages = append(b.messages, boundedMessage{
		Message: provider.Message{
			Role:    provider.RoleUser,
			Content: provider.Text(summaryPreamble + summary),
		},
		LastSeq: throughSeq,
	})
}

func (b *conversationBuilder) apply(event domain.Event) {
	switch payload := event.Payload.(type) {
	case domain.UserMessageAdded:
		b.flushAssistant()
		b.flushResults()
		b.messages = append(b.messages, boundedMessage{
			Message: provider.Message{
				Role:    provider.RoleUser,
				Content: provider.Text(payload.Text),
			},
			LastSeq: event.Seq,
		})

	case domain.AssistantTextDelta:
		// Results already gathered belong before the reply they informed.
		b.flushResults()
		b.pendingText += payload.Text
		b.assistantSeq = event.Seq

	case domain.ToolCallRequested:
		b.flushResults()
		b.pendingToolCalls = append(b.pendingToolCalls, provider.ToolUseBlock{
			ID:     string(payload.CallID),
			Name:   payload.Name,
			Args:   json.RawMessage(payload.Arguments),
			Opaque: json.RawMessage(payload.ProviderMetadata),
		})
		b.assistantSeq = event.Seq

	case domain.ToolCallCompleted:
		// The assistant turn that asked for this has to close before its
		// observation can follow.
		b.flushAssistant()
		b.pendingResults = append(b.pendingResults, provider.ToolResultBlock{
			ToolUseID: string(payload.CallID),
			Name:      payload.Name,
			Content:   payload.Content,
			IsError:   payload.IsError,
		})
		b.resultsSeq = event.Seq

	case domain.AssistantMessageCompleted:
		b.assistantSeq = event.Seq
		b.flushAssistant()
	}
}

func (b *conversationBuilder) flushAssistant() {
	if b.pendingText == "" && len(b.pendingToolCalls) == 0 {
		return
	}

	content := make([]provider.ContentBlock, 0, len(b.pendingToolCalls)+1)
	if b.pendingText != "" {
		content = append(content, provider.TextBlock{Text: b.pendingText})
	}
	content = append(content, b.pendingToolCalls...)

	b.messages = append(b.messages, boundedMessage{
		Message: provider.Message{
			Role:    provider.RoleAssistant,
			Content: content,
		},
		LastSeq: b.assistantSeq,
	})

	b.pendingText = ""
	b.pendingToolCalls = nil
}

func (b *conversationBuilder) flushResults() {
	if len(b.pendingResults) == 0 {
		return
	}

	b.messages = append(b.messages, boundedMessage{
		Message: provider.Message{
			Role:    provider.RoleTool,
			Content: b.pendingResults,
		},
		LastSeq: b.resultsSeq,
	})
	b.pendingResults = nil
}

func (b *conversationBuilder) finish() []boundedMessage {
	b.flushAssistant()
	b.flushResults()
	return b.messages
}

// pendingCall is a tool the model asked for that has no result yet.
type pendingCall struct {
	CallID    domain.ToolCallID
	Name      string
	Arguments []byte
}

// outstandingCalls finds tool calls with no recorded result.
//
// Derived from the log rather than tracked in memory, so a run resumed in a
// different process sees exactly what the one that requested them saw. A call
// with no result is also precisely what a crash leaves behind, which makes
// this the natural resume point.
func (r *Runtime) outstandingCalls(ctx context.Context, run domain.Run) ([]pendingCall, error) {
	events, err := r.opts.Store.ListAfter(ctx, run.SessionID, 0, 0)
	if err != nil {
		return nil, err
	}

	var (
		requested []pendingCall
		completed = make(map[domain.ToolCallID]bool)
	)

	for _, event := range events {
		// Scoped to this run: an unfinished call from an earlier run belongs
		// to that run's history, not to this one's work.
		if event.RunID != run.ID {
			continue
		}

		switch payload := event.Payload.(type) {
		case domain.ToolCallRequested:
			requested = append(requested, pendingCall{
				CallID:    payload.CallID,
				Name:      payload.Name,
				Arguments: []byte(payload.Arguments),
			})
		case domain.ToolCallCompleted:
			completed[payload.CallID] = true
		}
	}

	outstanding := make([]pendingCall, 0, len(requested))
	for _, call := range requested {
		if !completed[call.CallID] {
			outstanding = append(outstanding, call)
		}
	}
	return outstanding, nil
}

// modelTurns counts completed model turns in this run.
//
// The budget is measured against the log rather than a loop counter so a run
// resumed in a new process cannot silently start its allowance over.
func (r *Runtime) modelTurns(ctx context.Context, run domain.Run) (int, error) {
	events, err := r.opts.Store.ListAfter(ctx, run.SessionID, 0, 0)
	if err != nil {
		return 0, err
	}

	turns := 0
	for _, event := range events {
		if event.RunID == run.ID && event.Kind == domain.EventAssistantMessageCompleted {
			turns++
		}
	}
	return turns, nil
}

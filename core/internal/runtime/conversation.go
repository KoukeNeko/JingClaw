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
//
// This is not yet context engineering: everything is replayed. Compaction, a
// token budget and retrieval belong on top of this, and will replace the
// wholesale replay rather than change where history comes from.
func (r *Runtime) buildConversation(ctx context.Context, sessionID domain.SessionID) ([]provider.Message, error) {
	events, err := r.opts.Store.ListAfter(ctx, sessionID, 0, 0)
	if err != nil {
		return nil, err
	}

	builder := &conversationBuilder{}
	for _, event := range events {
		builder.apply(event)
	}

	return builder.finish(), nil
}

// conversationBuilder folds events into messages.
//
// Assistant text arrives as several coalesced deltas per message, and tool
// results arrive interleaved with them, so the builder accumulates until the
// shape of the turn changes rather than emitting a message per event.
type conversationBuilder struct {
	messages []provider.Message

	pendingText      string
	pendingToolCalls []provider.ContentBlock
	pendingResults   []provider.ContentBlock
}

func (b *conversationBuilder) apply(event domain.Event) {
	switch payload := event.Payload.(type) {
	case domain.UserMessageAdded:
		b.flushAssistant()
		b.flushResults()
		b.messages = append(b.messages, provider.Message{
			Role:    provider.RoleUser,
			Content: provider.Text(payload.Text),
		})

	case domain.AssistantTextDelta:
		// Results already gathered belong before the reply they informed.
		b.flushResults()
		b.pendingText += payload.Text

	case domain.ToolCallRequested:
		b.flushResults()
		b.pendingToolCalls = append(b.pendingToolCalls, provider.ToolUseBlock{
			ID:   string(payload.CallID),
			Name: payload.Name,
			Args: json.RawMessage(payload.Arguments),
		})

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

	case domain.AssistantMessageCompleted:
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

	b.messages = append(b.messages, provider.Message{
		Role:    provider.RoleAssistant,
		Content: content,
	})

	b.pendingText = ""
	b.pendingToolCalls = nil
}

func (b *conversationBuilder) flushResults() {
	if len(b.pendingResults) == 0 {
		return
	}

	b.messages = append(b.messages, provider.Message{
		Role:    provider.RoleTool,
		Content: b.pendingResults,
	})
	b.pendingResults = nil
}

func (b *conversationBuilder) finish() []provider.Message {
	b.flushAssistant()
	b.flushResults()
	return b.messages
}

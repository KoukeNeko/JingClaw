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

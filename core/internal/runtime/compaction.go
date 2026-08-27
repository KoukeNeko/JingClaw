package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// ContextBudget bounds how much of a session is sent to the model.
//
// Replaying the whole log is what makes a session survive a restart, but it is
// also unbounded: left alone, a session that goes on long enough stops working
// entirely, and it stops working at the moment somebody is in the middle of
// something. Compaction is what keeps that from being the end of the session.
type ContextBudget struct {
	// Window is the model's context window in tokens.
	//
	// Zero disables compaction. That is deliberate: a guessed window would
	// either summarise a conversation that had room to grow or fail to
	// summarise one that did not, and both are worse than leaving history
	// alone and letting the provider say the request is too large.
	Window int64

	// CompactAt is the fraction of the window at which history is summarised.
	// It is a fraction rather than the whole window because the estimate is
	// approximate and has to be allowed to be wrong.
	CompactAt float64

	// KeepFraction is how much of the window the verbatim tail may occupy once
	// the older part has been summarised.
	KeepFraction float64

	// SummaryTokens caps the summary itself. A summary that may grow without
	// limit is not a smaller conversation.
	SummaryTokens int
}

const (
	defaultCompactAt     = 0.7
	defaultKeepFraction  = 0.3
	defaultSummaryTokens = 1024
)

func (b ContextBudget) withDefaults() ContextBudget {
	if b.CompactAt <= 0 {
		b.CompactAt = defaultCompactAt
	}
	if b.KeepFraction <= 0 {
		b.KeepFraction = defaultKeepFraction
	}
	if b.SummaryTokens <= 0 {
		b.SummaryTokens = defaultSummaryTokens
	}
	return b
}

// compactIfNeeded summarises the older part of a session when the next request
// would be too large.
//
// It is called at the top of the tool loop with nothing outstanding, which is
// the one point where every requested call has a recorded result. Cutting
// anywhere else could separate a call from its result, and a conversation with
// an unanswered call in it is one most providers refuse outright.
//
// A failure to summarise is logged and the run continues. The alternative is
// ending a run over a transient provider error while the request may still
// have fitted; if it does not fit, the provider says so in terms an operator
// can act on.
func (r *Runtime) compactIfNeeded(ctx context.Context, run domain.Run, overhead int64) {
	budget := r.opts.ContextBudget.withDefaults()
	if budget.Window <= 0 {
		return
	}

	messages, err := r.buildBoundedConversation(ctx, run.SessionID)
	if err != nil {
		r.opts.Logger.Warn("could not measure the conversation",
			"run_id", string(run.ID), "error", err)
		return
	}

	before := overhead + estimateMessages(messages)
	if before <= int64(float64(budget.Window)*budget.CompactAt) {
		return
	}

	keep := int64(float64(budget.Window)*budget.KeepFraction) - overhead
	cut := chooseCut(messages, keep)
	if cut <= 0 {
		// Nothing can be folded without leaving the conversation malformed,
		// which happens when a single recent turn is itself over the budget.
		r.opts.Logger.Warn("conversation is over budget but cannot be compacted",
			"run_id", string(run.ID), "estimated_tokens", before)
		return
	}

	summary, err := r.summarise(ctx, messages[:cut], budget.SummaryTokens)
	if err != nil {
		r.opts.Logger.Warn("could not summarise the conversation",
			"run_id", string(run.ID), "error", err)
		return
	}

	after := overhead + estimateSummary(summary) + estimateMessages(messages[cut:])

	if err := r.append(ctx, run.SessionID, run.ID, domain.EventConversationCompacted,
		domain.ConversationCompacted{
			Summary:        summary,
			ThroughSeq:     messages[cut-1].LastSeq,
			MessagesFolded: cut,
			TokensBefore:   before,
			TokensAfter:    after,
		}); err != nil {
		r.opts.Logger.Warn("could not record the compaction",
			"run_id", string(run.ID), "error", err)
		return
	}

	r.opts.Logger.Info("compacted the conversation",
		"run_id", string(run.ID),
		"messages_folded", cut,
		"tokens_before", before,
		"tokens_after", after,
	)
}

// chooseCut decides how much history to fold, returning the index the verbatim
// tail begins at.
//
// It walks backwards so the most recent turns are the ones kept, then moves
// the cut forward off any tool results whose call would have been folded. A
// tool result with no call in front of it is not a smaller conversation; it is
// an invalid one.
func chooseCut(messages []boundedMessage, keep int64) int {
	cut := len(messages)

	var size int64
	for i := len(messages) - 1; i > 0; i-- {
		size += estimateMessage(messages[i].Message)
		if size > keep {
			break
		}
		cut = i
	}

	for cut < len(messages) && messages[cut].Message.Role == provider.RoleTool {
		cut++
	}

	if cut >= len(messages) {
		// Everything would go. Keep the most recent turn anyway: a model given
		// only a summary has no idea what it was just asked.
		cut = len(messages) - 1
		for cut > 0 && messages[cut].Message.Role == provider.RoleTool {
			cut--
		}
	}

	return cut
}

// summaryInstruction is written for the next turn, not for a reader. What the
// model needs from a summary is what it would otherwise have had to scroll
// back for.
const summaryInstruction = `You are compacting a working session so it can continue in a smaller context.

Write a summary that another instance of you could pick the work up from. Cover:
- what the user asked for, in their own terms, including anything they corrected
- decisions taken and the reasons, so they are not relitigated
- what has been done: files changed, commands run, what they showed
- what is still outstanding, and anything known to be broken
- exact identifiers that matter: paths, symbols, error strings, IDs

Prefer specifics over characterisation. "Renamed Handler.Serve to Handler.Run in
internal/control/server.go" is worth more than "refactored the server". Do not
address the user, do not offer to help, and do not describe the summary itself.`

// summarise asks the model to condense the part of the conversation being
// dropped.
//
// The messages are rendered into one transcript rather than replayed as a
// conversation. A replay would carry tool calls and their results as
// structured turns, and the head being summarised can legitimately end in the
// middle of one; a transcript has no pairing to get wrong.
func (r *Runtime) summarise(ctx context.Context, messages []boundedMessage, maxTokens int) (string, error) {
	stream, err := r.opts.Provider.Generate(ctx, provider.Request{
		Model:  r.opts.Model,
		System: provider.Text(summaryInstruction),
		Messages: []provider.Message{{
			Role:    provider.RoleUser,
			Content: provider.Text(renderTranscript(messages, int(maxTokens)*bytesPerToken*8)),
		}},
		MaxOutputTokens: maxTokens,
	})
	if err != nil {
		return "", err
	}
	defer func() { _ = stream.Close() }()

	var summary strings.Builder
	for {
		event, err := stream.Recv(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if delta, ok := event.(provider.TextDelta); ok {
			summary.WriteString(delta.Text)
		}
	}

	text := strings.TrimSpace(summary.String())
	if text == "" {
		return "", fmt.Errorf("runtime: the model returned an empty summary")
	}
	return text, nil
}

// renderTranscript flattens messages into labelled text, bounded so that the
// request asking for a summary cannot itself be too large.
func renderTranscript(messages []boundedMessage, maxBytes int) string {
	var transcript strings.Builder

	for _, item := range messages {
		fmt.Fprintf(&transcript, "\n[%s]\n", item.Message.Role)
		for _, block := range item.Message.Content {
			switch content := block.(type) {
			case provider.TextBlock:
				transcript.WriteString(content.Text)
				transcript.WriteString("\n")
			case provider.ToolUseBlock:
				fmt.Fprintf(&transcript, "called %s with %s\n", content.Name, content.Args)
			case provider.ToolResultBlock:
				outcome := "result"
				if content.IsError {
					outcome = "failed"
				}
				fmt.Fprintf(&transcript, "%s of %s: %s\n", outcome, content.Name, content.Content)
			}
		}
	}

	return boundMiddle(transcript.String(), maxBytes)
}

// boundMiddle drops the centre of an over-long transcript.
//
// The beginning holds what was originally asked and the end holds where the
// work got to; the middle of a long session is where the least information per
// byte lives.
func boundMiddle(text string, maxBytes int) string {
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text
	}

	half := maxBytes / 2
	return text[:half] +
		fmt.Sprintf("\n\n[... %d characters omitted ...]\n\n", len(text)-2*half) +
		text[len(text)-half:]
}

// bytesPerToken and messageOverheadTokens are a crude approximation, and are
// meant to be.
//
// A real tokenizer would have to be the provider's own, which means shipping
// one per vendor and keeping each in step with a model catalogue that changes
// under us. The decision to compact has to be made before the request is sent,
// so an exact count is not available at the moment it is needed anyway. Being
// approximately right and conservative about the threshold is worth more than
// being exactly right about a number that will be stale next month.
const (
	bytesPerToken         = 4
	messageOverheadTokens = 4
)

func estimateMessages(messages []boundedMessage) int64 {
	var total int64
	for _, item := range messages {
		total += estimateMessage(item.Message)
	}
	return total
}

func estimateMessage(message provider.Message) int64 {
	total := int64(messageOverheadTokens)

	for _, block := range message.Content {
		switch content := block.(type) {
		case provider.TextBlock:
			total += estimateText(content.Text)
		case provider.ToolUseBlock:
			total += estimateText(content.Name) + estimateText(string(content.Args))
		case provider.ToolResultBlock:
			total += estimateText(content.Name) + estimateText(content.Content)
		}
	}

	return total
}

func estimateSummary(summary string) int64 {
	return int64(messageOverheadTokens) + estimateText(summaryPreamble+summary)
}

func estimateText(text string) int64 {
	return int64(len(text)+bytesPerToken-1) / bytesPerToken
}

// estimateRequestOverhead counts everything sent on every turn that is not the
// conversation: the system prompt and the tool declarations.
//
// It is fixed for the life of a run but it is not small — a dozen tool schemas
// is real weight — and leaving it out would let the conversation grow into
// space that was never free.
func estimateRequestOverhead(system []provider.ContentBlock, tools []provider.ToolDeclaration) int64 {
	var total int64

	for _, block := range system {
		if text, ok := block.(provider.TextBlock); ok {
			total += estimateText(text.Text)
		}
	}
	for _, tool := range tools {
		total += estimateText(tool.Name) +
			estimateText(tool.Description) +
			estimateText(string(tool.InputSchema))
	}

	return total
}

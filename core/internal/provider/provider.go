// Package provider is the seam between the runtime and any language model.
//
// The interface lives here rather than in internal/runtime so that adapters
// depend on the contract instead of on the runtime. That direction matters:
// adding a provider must never be able to reach into run lifecycle, storage or
// permissions.
//
// The types are a canonical intermediate representation, not the lowest common
// denominator of every vendor. Where a provider has something genuinely its
// own, it goes in ProviderOptions rather than being flattened away or leaking
// vendor structs upward.
package provider

import (
	"context"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// Provider generates model responses and reports what it can do.
type Provider interface {
	// Name identifies the provider in configuration and telemetry.
	Name() string

	// Models lists what this provider can currently serve. The daemon is the
	// authority on the model catalog; clients ask rather than hardcode.
	Models(ctx context.Context) ([]ModelInfo, error)

	// Generate starts a streaming completion. The returned stream must be
	// closed by the caller.
	Generate(ctx context.Context, req Request) (Stream, error)
}

// ModelInfo describes one model. Capabilities are discovered rather than
// assumed, so the runtime can degrade deliberately instead of a provider
// adapter quietly changing behaviour behind its back.
type ModelInfo struct {
	ID          string
	DisplayName string
	Description string

	ContextWindow   int64
	MaxOutputTokens int64

	Capabilities Capabilities
}

type Capabilities struct {
	Streaming bool

	// Reserved for M1: the tool loop, structured output and multimodal input
	// all key off these rather than off the model name.
	Tools            bool
	StructuredOutput bool
	Vision           bool
	PromptCaching    bool
}

// Request is the canonical model request.
type Request struct {
	Model string

	// System instructions, kept separate from the conversation because
	// providers treat them differently and because prompt caching depends on
	// a stable prefix.
	System []ContentBlock

	Messages []Message

	MaxOutputTokens int
	Temperature     *float64

	// ProviderOptions is the escape hatch for genuinely provider-specific
	// settings. Flattening those away would force every provider down to the
	// weakest one's feature set.
	ProviderOptions map[string]any
}

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

type Message struct {
	Role    Role
	Content []ContentBlock
}

// ContentBlock is a closed set today and will grow to cover images and tool
// results. The marker method keeps adapters from inventing cases the runtime
// would not know how to persist.
type ContentBlock interface {
	contentBlock()
}

type TextBlock struct {
	Text string
}

func (TextBlock) contentBlock() {}

// Text is a shorthand for the common single-text-block message.
func Text(text string) []ContentBlock {
	return []ContentBlock{TextBlock{Text: text}}
}

// Event is one item from a provider stream, already normalized. No adapter may
// surface its SDK's own event types: a UI must never need to know whether the
// bytes came from Gemini, Anthropic or a local model.
type Event interface {
	isEvent()
}

// TextDelta is a chunk of assistant text.
type TextDelta struct {
	Text string
}

// UsageDelta reports token accounting. Providers send it at different points,
// so the runtime treats each one as the latest known total rather than
// something to accumulate.
type UsageDelta struct {
	Usage domain.Usage
}

// Completed marks the end of a generation and why it stopped.
type Completed struct {
	StopReason domain.StopReason
}

func (TextDelta) isEvent()  {}
func (UsageDelta) isEvent() {}
func (Completed) isEvent()  {}

// Stream yields Events until the generation ends or ctx is cancelled.
type Stream interface {
	// Recv returns the next event, or io.EOF once the stream is exhausted.
	Recv(ctx context.Context) (Event, error)

	// Close releases the stream. Callers must always call it.
	Close() error
}

// Func adapts a plain function to Provider, for tests.
type Func func(ctx context.Context, req Request) (Stream, error)

func (f Func) Name() string { return "func" }

func (f Func) Models(context.Context) ([]ModelInfo, error) { return nil, nil }

func (f Func) Generate(ctx context.Context, req Request) (Stream, error) { return f(ctx, req) }

// LastUserText returns the text of the most recent user message, which is all
// the M0-era fake provider needs.
func (r Request) LastUserText() string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role != RoleUser {
			continue
		}
		for _, block := range r.Messages[i].Content {
			if text, ok := block.(TextBlock); ok {
				return text.Text
			}
		}
	}
	return ""
}

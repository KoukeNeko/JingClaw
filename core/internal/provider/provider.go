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
	"encoding/json"

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

	// Tools the model may call this turn. Declared per request rather than per
	// provider, because which tools are available depends on the session's
	// policy, not on the vendor.
	Tools []ToolDeclaration

	// ProviderOptions is the escape hatch for genuinely provider-specific
	// settings. Flattening those away would force every provider down to the
	// weakest one's feature set.
	ProviderOptions map[string]any
}

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"

	// RoleTool carries observations back. Providers model this differently —
	// some as a distinct role, some as a user turn — which is precisely why
	// the runtime uses one vocabulary and lets adapters translate.
	RoleTool Role = "tool"
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

// ToolUseBlock records a tool the model asked for. It goes back into the
// conversation on the next turn so the model can see what it already decided.
type ToolUseBlock struct {
	ID   string
	Name string
	Args json.RawMessage

	// Opaque carries provider continuity metadata verbatim.
	//
	// Some providers attach state to a tool call that must be echoed back on
	// the following turn — Gemini's thought signatures are one — and that
	// state has no meaning outside the adapter that produced it. Translating
	// it into canonical types would be guesswork; dropping it makes the next
	// request fail. It is therefore carried through untouched, and only the
	// adapter that wrote it ever reads it.
	Opaque json.RawMessage
}

// ImageBlock carries an image the model is meant to look at.
//
// The bytes travel inline rather than as a reference, because a provider is a
// remote service and cannot read this machine's disk. They are read out of the
// artifact store at the moment a request is assembled and are not kept in the
// conversation between turns: an image is large, and the event log holds a
// reference to it rather than a copy.
type ImageBlock struct {
	// MediaType is the IANA type, which providers require and do not sniff.
	MediaType string

	Data []byte
}

// ToolResultBlock carries an observation back to the model. A failed tool is
// still a result: the model needs to read the error to do something different.
type ToolResultBlock struct {
	ToolUseID string
	Name      string
	Content   string
	IsError   bool
}

func (TextBlock) contentBlock()       {}
func (ToolUseBlock) contentBlock()    {}
func (ImageBlock) contentBlock()      {}
func (ToolResultBlock) contentBlock() {}

// ToolDeclaration is a tool as the model sees it.
type ToolDeclaration struct {
	Name        string
	Description string

	// InputSchema is a JSON Schema object, passed through rather than
	// translated: the schema the model is shown must be the same one its
	// arguments are validated against.
	InputSchema json.RawMessage
}

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

// ToolCallRequested is the model asking for a tool. Arguments arrive complete
// rather than streamed: a half-parsed call cannot be validated, let alone run.
type ToolCallRequested struct {
	ID   string
	Name string
	Args json.RawMessage

	// Opaque is provider continuity metadata to be returned with this call on
	// the next turn. See ToolUseBlock.Opaque.
	Opaque json.RawMessage
}

// ReasoningDelta is a chunk of the model's own thinking, where a provider
// exposes it.
//
// A separate event from TextDelta, and the separation is the point: this is
// not the answer. Backends disagree about almost everything here — some send
// it as "reasoning", some as "reasoning_content", some inline in the content
// wrapped in tags, and some let the caller choose — but they agree that it is
// working-out rather than what the model is telling anybody.
//
// Folded into TextDelta it would be indistinguishable from the answer by the
// time anything downstream saw it, and would be posted wherever the answer
// goes. Nothing consumes this yet; it exists so that the first backend to
// produce reasoning cannot leak it by default.
type ReasoningDelta struct {
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

	// RawReason is what the provider actually said, kept whether or not it
	// mapped onto anything known.
	//
	// The set of stop reasons is not closed in practice — one gateway
	// normalizes several upstream vocabularies and still passes the original
	// through, because they do not agree. An adapter that has to choose
	// between the known values will pick one, and "it stopped normally" is a
	// plausible, wrong, and unfalsifiable answer for a generation that was
	// actually cut off.
	RawReason string
}

func (TextDelta) isEvent()         {}
func (ReasoningDelta) isEvent()    {}
func (ToolCallRequested) isEvent() {}
func (UsageDelta) isEvent()        {}
func (Completed) isEvent()         {}

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

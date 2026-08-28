// Package openaicompat serves models through an endpoint that speaks the
// OpenAI chat protocol.
//
// "OpenAI-compatible" is a claim about a request shape, not about behaviour.
// Servers making it disagree on whether usage is reported at all, on which
// field carries a model's reasoning, on what a finish reason may say, and on
// what an HTTP status means — one of them returns 403 for a prompt that is too
// long. The protocol is shared; the semantics are not.
//
// So there is one decoder, written to be tolerant, and a Profile carrying what
// a particular server does differently. A profile is named in configuration
// rather than guessed from the address, because a reverse proxy makes the
// address say nothing.
package openaicompat

import "encoding/json"

// chatRequest is POST /v1/chat/completions.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
	Stream   bool          `json:"stream"`

	Tools      []wireTool `json:"tools,omitempty"`
	ToolChoice string     `json:"tool_choice,omitempty"`

	MaxTokens   int      `json:"max_tokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`

	// StreamOptions asks for a usage report on a stream. Servers that do not
	// know the field ignore it; servers that do send one extra frame at the
	// end. Asking is free and not asking means never being told.
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content,omitempty"`

	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

// contentPart is the array form of content, used when a message carries an
// image. A plain string is sent when it does not, because some servers accept
// only that.
type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type wireTool struct {
	Type     string           `json:"type"`
	Function wireToolFunction `json:"function"`
}

type wireToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type wireToolCall struct {
	// Index is what identifies a call across fragments. The id and the name
	// arrive once, on the first fragment; everything after it carries only
	// this and a piece of the arguments.
	Index *int `json:"index,omitempty"`

	ID       string               `json:"id,omitempty"`
	Type     string               `json:"type,omitempty"`
	Function wireToolCallFragment `json:"function"`
}

type wireToolCallFragment struct {
	Name string `json:"name,omitempty"`

	// Arguments is a string holding JSON, delivered in pieces that are not
	// individually valid.
	Arguments string `json:"arguments,omitempty"`
}

// chatChunk is one streamed frame.
type chatChunk struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []wireChoice `json:"choices"`
	Usage   *wireUsage   `json:"usage"`

	// Error appears inside a stream that already returned HTTP 200, when
	// something upstream of the endpoint failed after the headers went out.
	Error *wireError `json:"error"`
}

type wireChoice struct {
	Index int       `json:"index"`
	Delta wireDelta `json:"delta"`

	// FinishReason is a pointer because absent and empty mean different
	// things: still generating, versus finished with nothing to say about why.
	FinishReason *string `json:"finish_reason"`
}

type wireDelta struct {
	Role    string `json:"role"`
	Content string `json:"content"`

	// Reasoning and ReasoningContent are the same idea under two names. One
	// server renamed the field and warns that clients reading the old one now
	// silently receive nothing.
	Reasoning        string `json:"reasoning"`
	ReasoningContent string `json:"reasoning_content"`

	ToolCalls []wireToolCall `json:"tool_calls"`
}

type wireUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`

	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`

	CompletionTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type wireError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`

	// Metadata is where one gateway puts a normalized classification of
	// whatever the upstream provider said, which is more reliable than the
	// status it chose to return.
	Metadata *struct {
		ErrorType    string `json:"error_type"`
		ProviderCode string `json:"provider_code"`
	} `json:"metadata"`
}

// errorEnvelope is a failed request's body.
type errorEnvelope struct {
	Error *wireError `json:"error"`

	// Some servers put the message at the top level instead.
	Message string `json:"message"`
	Detail  any    `json:"detail"`
}

// modelsResponse is GET /v1/models.
//
// The context length is the reason for asking, and no two servers spell it the
// same way, so every spelling seen in the wild is accepted and the first one
// present wins.
type modelsResponse struct {
	Data []modelCard `json:"data"`
}

type modelCard struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by"`

	ContextLength *int64 `json:"context_length"`
	ContextWindow *int64 `json:"context_window"`
	MaxModelLen   *int64 `json:"max_model_len"`

	MaxCompletionTokens *int64 `json:"max_completion_tokens"`

	Meta *struct {
		NCtxTrain *int64 `json:"n_ctx_train"`
	} `json:"meta"`

	TopProvider *struct {
		ContextLength       *int64 `json:"context_length"`
		MaxCompletionTokens *int64 `json:"max_completion_tokens"`
	} `json:"top_provider"`
}

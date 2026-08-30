// Package anthropic talks to Anthropic's Messages API.
//
// Its own adapter rather than the OpenAI-compatible one, because the wire
// format is genuinely different rather than a dialect of the same thing:
// content is a list of typed blocks in both directions, tool results are
// blocks in a user message rather than a role of their own, and the stream is
// a sequence of named events with indices rather than a chain of deltas.
package anthropic

import "encoding/json"

// Version is the API revision this adapter was written against. Sent on every
// request, because the service requires it and because a response shaped by a
// revision nobody here has read is worse than a refusal.
const Version = "2023-06-01"

type request struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	System    []systemBlock `json:"system,omitempty"`
	Messages  []message     `json:"messages"`
	Tools     []toolSpec    `json:"tools,omitempty"`

	Temperature *float64 `json:"temperature,omitempty"`
	Stream      bool     `json:"stream"`
}

type systemBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type message struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}

// block is every content block in both directions.
//
// One struct rather than a type per shape: the fields are disjoint, the wire
// format distinguishes them by Type, and a family of types would need a
// custom unmarshaller to do what a tag already does.
type block struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`

	// image
	Source *imageSource `json:"source,omitempty"`
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type toolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// streamEvent is one line of the response stream.
//
// The shape is a state machine rather than a sequence of deltas: a block is
// opened, added to, and stopped, and the index says which one is being talked
// about. A tool call's arguments arrive as fragments of JSON across several
// deltas, which is why they are assembled before anything sees them.
type streamEvent struct {
	Type string `json:"type"`

	Index int `json:"index"`

	Message *struct {
		Usage *usage `json:"usage"`
	} `json:"message,omitempty"`

	ContentBlock *block `json:"content_block,omitempty"`

	Delta *struct {
		Type string `json:"type"`

		Text        string `json:"text,omitempty"`
		PartialJSON string `json:"partial_json,omitempty"`
		Thinking    string `json:"thinking,omitempty"`

		StopReason string `json:"stop_reason,omitempty"`
	} `json:"delta,omitempty"`

	Usage *usage `json:"usage,omitempty"`

	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

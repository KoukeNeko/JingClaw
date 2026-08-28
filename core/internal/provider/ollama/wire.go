// Package ollama serves models through Ollama's own API.
//
// Ollama also speaks a subset of the OpenAI protocol, and this deliberately
// does not use it. The native API is where the things a runtime needs live:
// how much context the server actually allocated, whether the model is even
// loaded, how long to keep it resident, and the model's thinking as a field of
// its own rather than mixed into the answer. Ollama's own documentation notes
// that its OpenAI surface has no way to set a context size at all — doing that
// through the compatibility layer means building a second model with a
// different name.
//
// One adapter covers both a local daemon and Ollama Cloud. They are the same
// API at a different address, with a credential added.
package ollama

import (
	"encoding/json"
	"time"
)

// chatRequest is POST /api/chat.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
	Tools    []wireTool    `json:"tools,omitempty"`
	Stream   bool          `json:"stream"`

	// Think asks for the model's reasoning as a separate field. Sent only
	// when a caller wants it: on a model that does not think it is an error,
	// not a no-op.
	Think any `json:"think,omitempty"`

	// KeepAlive is how long to leave the model resident after this request.
	// Loading one takes seconds that a conversation notices.
	KeepAlive string `json:"keep_alive,omitempty"`

	Options map[string]any `json:"options,omitempty"`

	// Format carries a JSON Schema for structured output, or the string
	// "json". Ollama Cloud does not support it, which is why it is a
	// capability rather than an assumption.
	Format json.RawMessage `json:"format,omitempty"`
}

type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`

	// Thinking is the model's working-out, which the API keeps out of Content.
	Thinking string `json:"thinking,omitempty"`

	// Images are raw base64, without a data: prefix.
	Images []string `json:"images,omitempty"`

	ToolCalls []wireToolCall `json:"tool_calls,omitempty"`

	// ToolName pairs a tool result with the call it answers. Ollama does not
	// use an id for this the way most APIs do.
	ToolName string `json:"tool_name,omitempty"`
}

type wireToolCall struct {
	// ID is sent by current daemons and was not by older ones. Used when
	// present rather than replaced: the runtime can mint an id, but the
	// server's own is the one it will recognise if it ever needs it back.
	ID string `json:"id,omitempty"`

	Function wireToolCallFunction `json:"function"`
}

type wireToolCallFunction struct {
	Name string `json:"name"`

	// Index orders calls within one turn.
	Index int `json:"index,omitempty"`

	// Arguments arrive as a JSON object, not as a string holding one. Most
	// APIs send the string; assuming this is one produces a call whose
	// arguments are the four characters of a quoted brace.
	Arguments json.RawMessage `json:"arguments"`
}

type wireTool struct {
	Type     string           `json:"type"`
	Function wireToolFunction `json:"function"`
}

type wireToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// chatChunk is one NDJSON line of a streaming response.
type chatChunk struct {
	Model   string      `json:"model"`
	Message wireMessage `json:"message"`

	Done       bool   `json:"done"`
	DoneReason string `json:"done_reason"`

	// Error appears on a line of an otherwise successful response. The HTTP
	// status is already 200 by the time a model fails mid-generation, so this
	// is the only signal that anything went wrong.
	Error string `json:"error"`

	PromptEvalCount int64 `json:"prompt_eval_count"`
	EvalCount       int64 `json:"eval_count"`
}

// errorBody is what a failed request returns.
type errorBody struct {
	Error string `json:"error"`
}

// tagsResponse is GET /api/tags: what is installed.
type tagsResponse struct {
	Models []tagModel `json:"models"`
}

type tagModel struct {
	Model      string       `json:"model"`
	Name       string       `json:"name"`
	Size       int64        `json:"size"`
	Details    modelDetails `json:"details"`
	ModifiedAt time.Time    `json:"modified_at"`

	// Capabilities is carried here as well as by /api/show, so a catalogue
	// can often be built without asking about each model separately.
	Capabilities []string `json:"capabilities"`

	// RemoteHost is set for a model the daemon proxies to the hosted service
	// rather than serving itself.
	RemoteHost string `json:"remote_host"`
}

type modelDetails struct {
	Family        string `json:"family"`
	ParameterSize string `json:"parameter_size"`

	// ContextLength is what the weights allow, and it is here in the listing
	// rather than only in /api/show. Reading it saves a request per model,
	// and means a daemon that will not describe a model individually still
	// yields a usable window.
	ContextLength int64 `json:"context_length"`
}

// psResponse is GET /api/ps: what is loaded right now.
//
// The only place that says how much context a running model actually has, as
// opposed to how much its weights allow.
type psResponse struct {
	Models []psModel `json:"models"`
}

type psModel struct {
	Model         string `json:"model"`
	Name          string `json:"name"`
	ContextLength int64  `json:"context_length"`
}

// showResponse is POST /api/show: what a model is.
type showResponse struct {
	Capabilities []string `json:"capabilities"`

	// ModelInfo is keyed by architecture, so the context length arrives under
	// a name that varies by model: llama.context_length, gemma3.context_length.
	// The key cannot be known in advance, only its suffix.
	ModelInfo map[string]any `json:"model_info"`
}

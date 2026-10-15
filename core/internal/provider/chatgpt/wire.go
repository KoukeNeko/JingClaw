// Package chatgpt serves a model from the person's own ChatGPT plan, through
// the Responses endpoint OpenAI documents for apps signed in with ChatGPT
// (developers.openai.com/siwc/token-sharing-open-source).
//
// Its own adapter: the endpoint is the Responses API with a fixed set of rules
// (nothing stored, always streamed, history resent, system text as
// instructions) that no other backend here shares. Implemented from the
// documentation and tested against fake servers; it has not been run against
// the live service.
package chatgpt

import "encoding/json"

// request is the body of POST /responses.
//
// Fields the documentation says to omit (temperature, top_p, max_output_tokens,
// metadata, user, truncation, previous_response_id, and the others it lists)
// are not in the struct, so they cannot be sent by accident.
type request struct {
	Model        string     `json:"model"`
	Instructions string     `json:"instructions,omitempty"`
	Input        []any      `json:"input"`
	Tools        []toolSpec `json:"tools,omitempty"`
	Reasoning    *reasoning `json:"reasoning,omitempty"`
	Store        bool       `json:"store"`
	Stream       bool       `json:"stream"`
}

type reasoning struct {
	Effort string `json:"effort"`
}

// toolSpec is a function tool. The documentation says function tools are
// supported and, on its limitations page, that they are to be grouped in
// namespaces or supplied as additional_tools input items; it shows no example
// and does not say whether a plain top-level entry is accepted. This sends the
// plain form, and a refusal arrives as unsupported_capability naming the param.
type toolSpec struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type part struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type messageItem struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []part `json:"content"`
}

type functionCallItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type functionOutputItem struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

// streamEvent is one server-sent event. The Responses stream names its type in
// the payload.
type streamEvent struct {
	Type string `json:"type"`

	Delta string `json:"delta"`

	Item *struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"item"`

	Response *struct {
		Status string `json:"status"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Param   string `json:"param"`
		} `json:"error"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Usage *struct {
			InputTokens       int64 `json:"input_tokens"`
			OutputTokens      int64 `json:"output_tokens"`
			InputTokenDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputTokenDetails struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	} `json:"response"`

	// An "error" event carries these at the top level.
	Code    string `json:"code"`
	Message string `json:"message"`
	Param   string `json:"param"`
}

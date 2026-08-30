package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// serving answers one generation with the given stream, and records what it
// was asked.
func serving(t *testing.T, events string) (*Provider, *request) {
	t.Helper()

	var asked request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("anthropic-version") == "" {
			t.Error("the request carries no API version")
		}
		if r.Header.Get("x-api-key") == "" {
			t.Error("the request carries no key")
		}

		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &asked); err != nil {
			t.Errorf("the request is not readable: %v", err)
		}

		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, events)
	}))
	t.Cleanup(server.Close)

	made, err := New(Config{APIKey: "k", Model: "claude", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return made, &asked
}

// drain reads a whole stream.
func drain(t *testing.T, p *Provider, req provider.Request) []provider.Event {
	t.Helper()

	stream, err := p.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	defer func() { _ = stream.Close() }()

	var events []provider.Event
	for {
		event, err := stream.Recv(context.Background())
		if err == io.EOF {
			return events
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		events = append(events, event)
	}
}

const answered = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":12}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" there"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}

`

func TestTextArrivesAsDeltas(t *testing.T) {
	p, _ := serving(t, answered)
	events := drain(t, p, provider.Request{Model: "claude"})

	var said strings.Builder
	completed := false
	for _, event := range events {
		switch held := event.(type) {
		case provider.TextDelta:
			said.WriteString(held.Text)
		case provider.Completed:
			completed = true
			if held.StopReason != domain.StopEndTurn {
				t.Errorf("stopped for %q", held.StopReason)
			}
			if held.RawReason != "end_turn" {
				t.Errorf("the raw reason is %q", held.RawReason)
			}
		}
	}

	if said.String() != "Hello there" {
		t.Errorf("the answer is %q", said.String())
	}
	if !completed {
		t.Error("the stream never said it had finished")
	}
}

// A tool call's arguments arrive as fragments of JSON. Passed on in pieces
// they could not be validated, let alone run.
func TestAToolCallIsAssembledBeforeItIsHandedOver(t *testing.T) {
	const calling = `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"read_file"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"pa"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"th\":\"main.go\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

`
	p, _ := serving(t, calling)
	events := drain(t, p, provider.Request{})

	for _, event := range events {
		called, ok := event.(provider.ToolCallRequested)
		if !ok {
			continue
		}
		if called.ID != "call_1" || called.Name != "read_file" {
			t.Errorf("the call is %+v", called)
		}

		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(called.Args, &args); err != nil {
			t.Fatalf("the arguments are not valid JSON: %v (%s)", err, called.Args)
		}
		if args.Path != "main.go" {
			t.Errorf("the arguments are %s", called.Args)
		}
		return
	}
	t.Fatal("no tool call arrived")
}

// A call with no arguments sends nothing at all rather than an empty object,
// and an empty string is not a document.
func TestAToolCallWithNoArgumentsIsStillValidJSON(t *testing.T) {
	const calling = `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c","name":"git_status"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

`
	p, _ := serving(t, calling)

	for _, event := range drain(t, p, provider.Request{}) {
		if called, ok := event.(provider.ToolCallRequested); ok {
			var into map[string]any
			if err := json.Unmarshal(called.Args, &into); err != nil {
				t.Errorf("the arguments are not valid JSON: %q", called.Args)
			}
			return
		}
	}
	t.Fatal("no tool call arrived")
}

// Thinking is its own event. Folded into the answer it would be posted
// wherever the answer goes.
func TestThinkingIsNotTheAnswer(t *testing.T) {
	const thinking = `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"working it out"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"the answer"}}

`
	p, _ := serving(t, thinking)

	var said, thought strings.Builder
	for _, event := range drain(t, p, provider.Request{}) {
		switch held := event.(type) {
		case provider.TextDelta:
			said.WriteString(held.Text)
		case provider.ReasoningDelta:
			thought.WriteString(held.Text)
		}
	}

	if said.String() != "the answer" {
		t.Errorf("the answer is %q", said.String())
	}
	if thought.String() != "working it out" {
		t.Errorf("the thinking is %q", thought.String())
	}
}

// A tool result is a block in a user message here, rather than a role of its
// own — the one place the two shapes genuinely disagree.
func TestAToolResultIsSentAsABlockInAUserMessage(t *testing.T) {
	p, asked := serving(t, answered)

	drain(t, p, provider.Request{
		System: []provider.ContentBlock{provider.TextBlock{Text: "be brief"}},
		Messages: []provider.Message{{
			Role: provider.RoleUser,
			Content: []provider.ContentBlock{provider.ToolResultBlock{
				ToolUseID: "call_1", Name: "read_file", Content: "package main",
			}},
		}},
	})

	if len(asked.System) != 1 || asked.System[0].Text != "be brief" {
		t.Errorf("the system prompt is %+v", asked.System)
	}
	if len(asked.Messages) != 1 {
		t.Fatalf("sent %d messages", len(asked.Messages))
	}
	sent := asked.Messages[0]
	if sent.Role != "user" {
		t.Errorf("a tool result was sent as %q", sent.Role)
	}
	if len(sent.Content) != 1 || sent.Content[0].Type != "tool_result" {
		t.Fatalf("the content is %+v", sent.Content)
	}
	if sent.Content[0].ToolUseID != "call_1" {
		t.Errorf("the result does not name the call: %+v", sent.Content[0])
	}
}

// The service requires a token ceiling, unlike every other backend here.
func TestAMaxIsAlwaysSent(t *testing.T) {
	p, asked := serving(t, answered)
	drain(t, p, provider.Request{})

	if asked.MaxTokens <= 0 {
		t.Errorf("no token ceiling was sent: %d", asked.MaxTokens)
	}
}

// A failure the runtime can act on, rather than a status code.
func TestAContextOverflowIsSaidToBeOne(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w,
			`{"error":{"type":"invalid_request_error","message":"prompt is too long: 300000 tokens"}}`)
	}))
	defer server.Close()

	p, err := New(Config{APIKey: "k", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}

	_, err = p.Generate(context.Background(), provider.Request{})
	if err == nil {
		t.Fatal("a refused request looked like a success")
	}

	var reported *provider.Error
	if !asProviderError(err, &reported) {
		t.Fatalf("the failure is not a provider error: %v", err)
	}
	if reported.Kind != provider.KindContextOverflow {
		t.Errorf("a prompt too long was reported as %q", reported.Kind)
	}
}

func asProviderError(err error, into **provider.Error) bool {
	reported, ok := err.(*provider.Error)
	if ok {
		*into = reported
	}
	return ok
}

func TestNoKeyIsRefused(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("a provider was built with no key")
	}
}

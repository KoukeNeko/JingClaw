package openaicompat_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/provider/openaicompat"
)

func serve(t *testing.T, profile string, handler http.HandlerFunc) *openaicompat.Provider {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	p, err := openaicompat.New(openaicompat.Config{BaseURL: server.URL + "/v1", Profile: profile})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return p
}

// sse writes frames in the shape a server does, blank line and all.
func sse(frames ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range frames {
			_, _ = io.WriteString(w, "data: "+frame+"\n\n")
		}
	}
}

func drain(t *testing.T, p *openaicompat.Provider) ([]provider.Event, error) {
	t.Helper()

	stream, err := p.Generate(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()

	var events []provider.Event
	for {
		event, err := stream.Recv(context.Background())
		if errors.Is(err, io.EOF) {
			return events, nil
		}
		if err != nil {
			return events, err
		}
		events = append(events, event)
	}
}

// The first landmine: a call arrives across frames, and only the first names
// it. Every frame after carries an index and a slice of the arguments that is
// not valid JSON on its own.
func TestAToolCallIsAssembledFromItsFragments(t *testing.T) {
	p := serve(t, "generic", sse(
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"read_file","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"notes.md\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	))

	events, err := drain(t, p)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	var calls []provider.ToolCallRequested
	for _, event := range events {
		if call, ok := event.(provider.ToolCallRequested); ok {
			calls = append(calls, call)
		}
	}

	if len(calls) != 1 {
		t.Fatalf("got %d calls, want one assembled from three frames", len(calls))
	}
	if calls[0].ID != "call_abc" || calls[0].Name != "read_file" {
		t.Errorf("the id or name was lost in reassembly: %+v", calls[0])
	}

	var args map[string]string
	if err := json.Unmarshal(calls[0].Args, &args); err != nil {
		t.Fatalf("the arguments do not parse: %v (%s)", err, calls[0].Args)
	}
	if args["path"] != "notes.md" {
		t.Errorf("arguments are %v", args)
	}
}

// Calls interleave when a model asks for several at once. Assuming one
// finishes before the next begins concatenates two models' arguments into one.
func TestInterleavedParallelCallsDoNotContaminateEachOther(t *testing.T) {
	p := serve(t, "generic", sse(
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"grep","arguments":"{\"query\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.go\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"\"needle\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	))

	events, err := drain(t, p)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	got := map[string]string{}
	for _, event := range events {
		if call, ok := event.(provider.ToolCallRequested); ok {
			got[call.Name] = string(call.Args)
		}
	}

	if len(got) != 2 {
		t.Fatalf("got %d calls, want 2: %v", len(got), got)
	}
	if got["read_file"] != `{"path":"a.go"}` {
		t.Errorf("read_file arguments are %s", got["read_file"])
	}
	if got["grep"] != `{"query":"needle"}` {
		t.Errorf("grep arguments are %s", got["grep"])
	}
}

// The usage frame comes after a finish reason, and sometimes carries no
// choices at all. Stopping at the first finish reason means never being told
// what anything cost.
func TestUsageAfterTheFinishReasonIsStillRead(t *testing.T) {
	p := serve(t, "generic", sse(
		`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":8}}}`,
		`[DONE]`,
	))

	events, err := drain(t, p)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	var usage *domain.Usage
	for _, event := range events {
		if delta, ok := event.(provider.UsageDelta); ok {
			usage = &delta.Usage
		}
	}

	if usage == nil {
		t.Fatal("the usage frame after the finish reason was never read")
	}
	if usage.InputTokens != 11 || usage.OutputTokens != 2 || usage.CachedInputTokens != 8 {
		t.Errorf("usage is %+v", *usage)
	}
}

// A server that reports no usage at all is not a broken server.
func TestNoUsageIsNotAFailure(t *testing.T) {
	p := serve(t, "generic", sse(
		`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`,
		`[DONE]`,
	))

	events, err := drain(t, p)
	if err != nil {
		t.Fatalf("a stream without usage was treated as a protocol failure: %v", err)
	}
	for _, event := range events {
		if _, ok := event.(provider.UsageDelta); ok {
			t.Error("usage was invented")
		}
	}
}

// Reasoning arrives under two different names and must not become the answer
// under either.
func TestReasoningUnderEitherNameStaysOutOfTheAnswer(t *testing.T) {
	for _, field := range []string{"reasoning", "reasoning_content"} {
		t.Run(field, func(t *testing.T) {
			p := serve(t, "generic", sse(
				`{"choices":[{"index":0,"delta":{"`+field+`":"working it out"}}]}`,
				`{"choices":[{"index":0,"delta":{"content":"the answer"},"finish_reason":"stop"}]}`,
				`[DONE]`,
			))

			events, err := drain(t, p)
			if err != nil {
				t.Fatalf("drain: %v", err)
			}

			var text, reasoning strings.Builder
			for _, event := range events {
				switch e := event.(type) {
				case provider.TextDelta:
					text.WriteString(e.Text)
				case provider.ReasoningDelta:
					reasoning.WriteString(e.Text)
				}
			}

			if text.String() != "the answer" {
				t.Errorf("the answer is %q; reasoning leaked in", text.String())
			}
			if reasoning.String() != "working it out" {
				t.Errorf("reasoning is %q", reasoning.String())
			}
		})
	}
}

// An unfamiliar finish reason is recorded as unrecognised with the original
// kept, rather than forced onto the nearest known value.
func TestAnUnknownFinishReasonIsNotGuessedAt(t *testing.T) {
	p := serve(t, "generic", sse(
		`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"something_new"}]}`,
		`[DONE]`,
	))

	events, err := drain(t, p)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	var completed *provider.Completed
	for _, event := range events {
		if done, ok := event.(provider.Completed); ok {
			completed = &done
		}
	}

	if completed == nil {
		t.Fatal("no completion")
	}
	if completed.StopReason != domain.StopUnknown {
		t.Errorf("stop reason is %q, want unknown", completed.StopReason)
	}
	if completed.RawReason != "something_new" {
		t.Errorf("the original reason was not kept: %q", completed.RawReason)
	}
}

// Upstream failing after the headers went out leaves an error inside a 200.
func TestAnErrorInsideASuccessfulStreamIsAFailure(t *testing.T) {
	p := serve(t, "openrouter", sse(
		`{"choices":[{"index":0,"delta":{"content":"partial"}}]}`,
		`{"error":{"code":429,"message":"upstream is rate limited","metadata":{"error_type":"rate_limit_exceeded"}}}`,
	))

	_, err := drain(t, p)
	if err == nil {
		t.Fatal("an error inside the stream was reported as a completed generation")
	}
	if kind := provider.KindOf(err); kind != provider.KindRateLimited {
		t.Errorf("classified as %s, want rate_limited", kind)
	}
}

// A stream that simply stops, without [DONE] and without a finish reason.
// Whatever was assembled still belongs to the caller.
func TestAnAbruptEndStillDeliversTheAssembledCall(t *testing.T) {
	p := serve(t, "generic", sse(
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"grep","arguments":"{\"query\":\"x\"}"}}]}}]}`,
	))

	events, err := drain(t, p)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	var found bool
	for _, event := range events {
		if call, ok := event.(provider.ToolCallRequested); ok && call.Name == "grep" {
			found = true
		}
	}
	if !found {
		t.Error("a call assembled before the stream ended was dropped")
	}
}

// A model taking no arguments sends nothing rather than an empty object, and a
// tool handed empty bytes fails for a reason unrelated to what it was asked.
func TestACallWithNoArgumentsGetsAnEmptyObject(t *testing.T) {
	p := serve(t, "generic", sse(
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"now","arguments":""}}]},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	))

	events, err := drain(t, p)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	for _, event := range events {
		if call, ok := event.(provider.ToolCallRequested); ok {
			if string(call.Args) != "{}" {
				t.Errorf("arguments are %q, want an empty object", call.Args)
			}
			return
		}
	}
	t.Fatal("no call was produced")
}

// The model listing is where portability runs out entirely: four spellings for
// the context window, and one of them a level down. Inferring it from a
// model's name instead would tell compaction that llama-3.1-8b has 128k when
// the operator gave it 8k.
func TestTheContextWindowIsFoundUnderEverySpelling(t *testing.T) {
	tests := []struct {
		name       string
		card       string
		want       int64
		wantSource provider.ContextSource
	}{
		{
			// A server reporting what it resolved for itself, which beats any
			// catalogue figure.
			name:       "max_model_len",
			card:       `{"id":"m","max_model_len":131072}`,
			want:       131072,
			wantSource: provider.ContextRuntime,
		},
		{
			name:       "context_window",
			card:       `{"id":"m","context_window":32768}`,
			want:       32768,
			wantSource: provider.ContextCatalog,
		},
		{
			name:       "context_length",
			card:       `{"id":"m","context_length":8192}`,
			want:       8192,
			wantSource: provider.ContextCatalog,
		},
		{
			name:       "a gateway describing where it will route",
			card:       `{"id":"m","context_length":8192,"top_provider":{"context_length":4096}}`,
			want:       4096,
			wantSource: provider.ContextCatalog,
		},
		{
			// The training context, which this server may not have allocated.
			name:       "buried in meta",
			card:       `{"id":"m","meta":{"n_ctx_train":131072}}`,
			want:       131072,
			wantSource: provider.ContextTrained,
		},
		{
			// The common case: an id and nothing else. Nothing is invented.
			name:       "nothing but an id",
			card:       `{"id":"m"}`,
			want:       0,
			wantSource: provider.ContextUnknown,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := serve(t, "generic", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"data":[`+test.card+`]}`)
			})

			models, err := p.Models(context.Background())
			if err != nil {
				t.Fatalf("models: %v", err)
			}
			if len(models) != 1 {
				t.Fatalf("got %d models", len(models))
			}
			if models[0].ContextWindow != test.want {
				t.Errorf("window is %d, want %d", models[0].ContextWindow, test.want)
			}
			if models[0].ContextSource != test.wantSource {
				t.Errorf("source is %q, want %q", models[0].ContextSource, test.wantSource)
			}
		})
	}
}

func TestAnUnusableEndpointFailsAtStartup(t *testing.T) {
	for _, cfg := range []openaicompat.Config{
		{BaseURL: ""},
		{BaseURL: "not a url"},
		{BaseURL: "ftp://example.com/v1"},
		{BaseURL: "http://localhost:8000/v1", Profile: "vlm"},
	} {
		if _, err := openaicompat.New(cfg); err == nil {
			t.Errorf("%+v was accepted", cfg)
		}
	}
}

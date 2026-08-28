package ollama_test

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
	"github.com/KoukeNeko/JingClaw/core/internal/provider/ollama"
)

// serve stands up a fake daemon that answers /api/chat with the given lines.
func serve(t *testing.T, handler http.HandlerFunc) *ollama.Provider {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	p, err := ollama.New(ollama.Config{BaseURL: server.URL})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return p
}

func ndjson(t *testing.T, lines ...string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, line := range lines {
			_, _ = io.WriteString(w, line+"\n")
		}
	}
}

func drain(t *testing.T, stream provider.Stream) ([]provider.Event, error) {
	t.Helper()

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

func TestAnAnswerArrivesAsTextAndUsage(t *testing.T) {
	p := serve(t, ndjson(t,
		`{"message":{"role":"assistant","content":"Hello"},"done":false}`,
		`{"message":{"role":"assistant","content":" there"},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done":true,"done_reason":"stop",`+
			`"prompt_eval_count":26,"eval_count":298}`,
	))

	stream, err := p.Generate(context.Background(), provider.Request{Model: "qwen3"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	defer func() { _ = stream.Close() }()

	events, err := drain(t, stream)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	var text strings.Builder
	var usage domain.Usage
	var completed *provider.Completed
	for _, event := range events {
		switch e := event.(type) {
		case provider.TextDelta:
			text.WriteString(e.Text)
		case provider.UsageDelta:
			usage = e.Usage
		case provider.Completed:
			completed = &e
		}
	}

	if text.String() != "Hello there" {
		t.Errorf("text is %q", text.String())
	}
	if usage.InputTokens != 26 || usage.OutputTokens != 298 {
		t.Errorf("usage is %+v", usage)
	}
	if completed == nil || completed.StopReason != domain.StopEndTurn {
		t.Errorf("completed is %+v", completed)
	}
}

// Thinking is a field of its own here, and must stay one. Folded into the
// answer it would be posted wherever the answer goes.
func TestThinkingDoesNotBecomeTheAnswer(t *testing.T) {
	p := serve(t, ndjson(t,
		`{"message":{"role":"assistant","thinking":"the user probably means X","content":""},"done":false}`,
		`{"message":{"role":"assistant","content":"X."},"done":true,"done_reason":"stop"}`,
	))

	stream, err := p.Generate(context.Background(), provider.Request{Model: "qwen3"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	defer func() { _ = stream.Close() }()

	events, err := drain(t, stream)
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

	if text.String() != "X." {
		t.Errorf("the answer is %q; thinking leaked into it", text.String())
	}
	if reasoning.String() != "the user probably means X" {
		t.Errorf("thinking is %q", reasoning.String())
	}
}

// Arguments arrive as a JSON object, not as a string holding one. Treating
// this like every other API produces a call whose arguments are a quoted
// brace.
func TestToolArgumentsArriveAsAnObject(t *testing.T) {
	p := serve(t, ndjson(t,
		`{"message":{"role":"assistant","content":"","tool_calls":[`+
			`{"function":{"name":"read_file","arguments":{"path":"notes.md"}}}]},`+
			`"done":true,"done_reason":"stop"}`,
	))

	stream, err := p.Generate(context.Background(), provider.Request{Model: "qwen3"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	defer func() { _ = stream.Close() }()

	events, err := drain(t, stream)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	var call *provider.ToolCallRequested
	var completed *provider.Completed
	for _, event := range events {
		switch e := event.(type) {
		case provider.ToolCallRequested:
			call = &e
		case provider.Completed:
			completed = &e
		}
	}

	if call == nil {
		t.Fatal("no tool call was produced")
	}
	if call.Name != "read_file" {
		t.Errorf("name is %q", call.Name)
	}

	var args map[string]string
	if err := json.Unmarshal(call.Args, &args); err != nil {
		t.Fatalf("arguments are not a usable object: %v (%s)", err, call.Args)
	}
	if args["path"] != "notes.md" {
		t.Errorf("arguments are %v", args)
	}
	// A turn that asked for a tool stopped to use one, whatever done_reason
	// happened to say.
	if completed == nil || completed.StopReason != domain.StopToolUse {
		t.Errorf("completed is %+v", completed)
	}
}

// The failure that a status check cannot see: the headers were written before
// the model broke, so the response is a 200 that ends in an error.
func TestAFailureAfterTheHeadersIsStillAFailure(t *testing.T) {
	p := serve(t, ndjson(t,
		`{"message":{"role":"assistant","content":"partial"},"done":false}`,
		`{"error":"an error was encountered while running the model"}`,
	))

	stream, err := p.Generate(context.Background(), provider.Request{Model: "qwen3"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	defer func() { _ = stream.Close() }()

	_, err = drain(t, stream)
	if err == nil {
		t.Fatal("a mid-stream failure was reported as a completed generation")
	}
	if !strings.Contains(err.Error(), "error was encountered") {
		t.Errorf("the failure does not say what happened: %v", err)
	}
}

// A line larger than the read buffer, which one substantial tool call
// produces. Truncating it would turn valid JSON into a parse error.
func TestAVeryLongLineIsNotTruncated(t *testing.T) {
	long := strings.Repeat("a", 300*1024)
	line, err := json.Marshal(map[string]any{
		"message": map[string]any{"role": "assistant", "content": long},
		"done":    true, "done_reason": "stop",
	})
	if err != nil {
		t.Fatal(err)
	}

	p := serve(t, ndjson(t, string(line)))

	stream, err := p.Generate(context.Background(), provider.Request{Model: "qwen3"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	defer func() { _ = stream.Close() }()

	events, err := drain(t, stream)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	var text strings.Builder
	for _, event := range events {
		if delta, ok := event.(provider.TextDelta); ok {
			text.WriteString(delta.Text)
		}
	}
	if text.Len() != len(long) {
		t.Errorf("got %d characters, want %d", text.Len(), len(long))
	}
}

// Local servers fail for reasons a hosted API does not, and the status alone
// does not separate them.
func TestLocalFailuresAreClassifiedByWhatTheySay(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   provider.ErrorKind
	}{
		{
			// Waiting fixes this: the queue drains.
			name: "the queue is full", status: 503,
			body: `{"error":"server busy, please try again"}`,
			want: provider.KindOverloaded,
		},
		{
			// Waiting does not fix this, and neither does resending.
			name: "the model will not fit", status: 500,
			body: `{"error":"model requires more system memory than is available"}`,
			want: provider.KindResourceExhausted,
		},
		{
			name: "no such model", status: 404,
			body: `{"error":"model 'nope' not found, try pulling it first"}`,
			want: provider.KindNotFound,
		},
		{
			name: "the cloud rejected the credential", status: 401,
			body: `{"error":"unauthorized"}`,
			want: provider.KindAuth,
		},
		{
			// A proxy in front of the daemon answers in HTML. Insisting on
			// JSON would report a parse failure instead of the outage.
			name: "something in front of it answered in html", status: 502,
			body: `<html><body>Bad Gateway</body></html>`,
			want: provider.KindTransient,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			})

			_, err := p.Generate(context.Background(), provider.Request{Model: "qwen3"})
			if err == nil {
				t.Fatal("the failure was accepted")
			}
			if kind := provider.KindOf(err); kind != test.want {
				t.Errorf("classified as %s, want %s (%v)", kind, test.want, err)
			}
		})
	}
}

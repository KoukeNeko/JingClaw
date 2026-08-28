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

// The shape a real daemon actually sends, which a hand-written stand-in got
// wrong: the tool call is in one line, and the line saying the turn is over is
// a later, empty one that reports "stop". A reason derived from that last line
// alone records a turn that used tools as one that simply finished.
func TestATurnThatUsedToolsSaysSoEvenWhenTheLastLineDoesNot(t *testing.T) {
	p := serve(t, ndjson(t,
		`{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_kpo73sbh",`+
			`"function":{"index":0,"name":"read_file","arguments":{"path":"notes.md"}}}]},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done":true,"done_reason":"stop",`+
			`"prompt_eval_count":65,"eval_count":17}`,
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
		t.Fatal("no tool call")
	}
	// The daemon does send an id, and its own is the one it would recognise.
	if call.ID != "call_kpo73sbh" {
		t.Errorf("the server's tool call id was discarded: %q", call.ID)
	}
	if completed == nil || completed.StopReason != domain.StopToolUse {
		t.Errorf("stop reason is %+v, want tool_use", completed)
	}
	// The original is kept, so the log does not claim the server said
	// something it did not.
	if completed.RawReason != "stop" {
		t.Errorf("raw reason is %q, want what the server actually said", completed.RawReason)
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

// Asking a model that cannot think is an error rather than something ignored,
// so what is asked for follows what the model said it can do.
func TestThinkingFollowsWhatTheModelSaidItCanDo(t *testing.T) {
	var sent []map[string]any
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = io.WriteString(w, `{"models":[
				{"model":"thinker","name":"thinker","capabilities":["completion","thinking"],
				 "details":{"context_length":8192}},
				{"model":"plain","name":"plain","capabilities":["completion"],
				 "details":{"context_length":8192}}]}`)
		case "/api/ps":
			_, _ = io.WriteString(w, `{"models":[]}`)
		case "/api/chat":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			sent = append(sent, body)
			_, _ = io.WriteString(w, `{"message":{"content":"ok"},"done":true,"done_reason":"stop"}`+"\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}

	ask := func(t *testing.T, think, model string) map[string]any {
		t.Helper()
		sent = nil

		server := httptest.NewServer(http.HandlerFunc(handler))
		defer server.Close()

		p, err := ollama.New(ollama.Config{BaseURL: server.URL, Think: think})
		if err != nil {
			t.Fatal(err)
		}
		// The daemon lists the catalogue at startup, which is what teaches the
		// adapter which models think.
		if _, err := p.Models(context.Background()); err != nil {
			t.Fatalf("models: %v", err)
		}
		stream, err := p.Generate(context.Background(), provider.Request{Model: model})
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		_, _ = drain(t, stream)
		_ = stream.Close()

		if len(sent) != 1 {
			t.Fatalf("sent %d requests", len(sent))
		}
		return sent[0]
	}

	t.Run("a model that thinks is asked to", func(t *testing.T) {
		if got := ask(t, "", "thinker")["think"]; got != true {
			t.Errorf("think is %#v, want true", got)
		}
	})

	t.Run("a model that does not is not asked", func(t *testing.T) {
		if _, present := ask(t, "", "plain")["think"]; present {
			t.Error("a model that cannot think was asked to, which is an error rather than a no-op")
		}
	})

	t.Run("a depth is passed through", func(t *testing.T) {
		if got := ask(t, "high", "thinker")["think"]; got != "high" {
			t.Errorf("think is %#v, want \"high\"", got)
		}
	})

	t.Run("off is explicit rather than absent", func(t *testing.T) {
		// A server whose default is to think needs to be told no, and an
		// absent field would leave it thinking.
		if got := ask(t, "off", "thinker")["think"]; got != false {
			t.Errorf("think is %#v, want false", got)
		}
	})

	t.Run("on overrides what the model claimed", func(t *testing.T) {
		if got := ask(t, "on", "plain")["think"]; got != true {
			t.Errorf("think is %#v, want true", got)
		}
	})
}

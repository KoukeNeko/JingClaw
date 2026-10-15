package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/chatgptauth"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// tokens is a signed-in account that hands out numbered tokens.
type tokens struct {
	mu        sync.Mutex
	current   int
	refreshes int
	signedOut error // returned by Refresh, when set
	recorded  []string
}

func (k *tokens) AccessToken(context.Context) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.name(), nil
}

func (k *tokens) name() string { return "token-" + string(rune('0'+k.current)) }

func (k *tokens) Refresh(_ context.Context, rejected string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.signedOut != nil {
		return "", k.signedOut
	}
	if rejected == k.name() {
		k.current++
		k.refreshes++
	}
	return k.name(), nil
}

func (k *tokens) RecordError(code, _ string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.recorded = append(k.recorded, code)
}

// responses is a Responses server that answers each request with the next
// scripted reply and records what it was sent.
type reply struct {
	status  int
	headers map[string]string
	body    string
	// onlyFor makes the reply apply to one token, and 401 to every other.
	onlyFor string
}

type responses struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	replies  []reply
	bodies   [][]byte
	headers  []http.Header
	requests int
}

func newResponses(t *testing.T, replies ...reply) *responses {
	t.Helper()
	r := &responses{t: t, replies: replies}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		body, _ := io.ReadAll(req.Body)
		r.bodies = append(r.bodies, body)
		r.headers = append(r.headers, req.Header.Clone())

		next := reply{status: 200}
		if r.requests < len(r.replies) {
			next = r.replies[r.requests]
		} else if len(r.replies) > 0 {
			next = r.replies[len(r.replies)-1]
		}
		r.requests++

		if next.onlyFor != "" && req.Header.Get("Authorization") != "Bearer "+next.onlyFor {
			w.WriteHeader(401)
			return
		}
		for key, value := range next.headers {
			w.Header().Set(key, value)
		}
		if next.status == 0 {
			next.status = 200
		}
		w.WriteHeader(next.status)
		_, _ = io.WriteString(w, next.body)
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *responses) provider(k TokenSource) *Provider {
	return New(Config{Tokens: k, Model: "gpt-test", BaseURL: r.server.URL + "/v1"})
}

func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("event: x\ndata: " + e + "\n\n")
	}
	return b.String()
}

// drain reads a stream to its end and returns the events and the error that
// ended it (nil for a clean end).
func drain(t *testing.T, p provider.Provider, req provider.Request) ([]provider.Event, error) {
	t.Helper()
	stream, err := p.Generate(context.Background(), req)
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

const completedText = `{"type":"response.output_text.delta","delta":"Hello"}`

var completed = sse(
	completedText,
	`{"type":"response.output_text.delta","delta":" there"}`,
	`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":12,"output_tokens":3,"input_tokens_details":{"cached_tokens":4},"output_tokens_details":{"reasoning_tokens":1}}}}`,
)

func TestTheRequestIsStoredFalseStreamedAndCarriesNoFieldTheServiceRejects(t *testing.T) {
	server := newResponses(t, reply{body: completed})
	p := server.provider(&tokens{})

	args := json.RawMessage(`{"path":"a.txt"}`)
	_, err := drain(t, p, provider.Request{
		Model:           "gpt-model",
		System:          []provider.ContentBlock{provider.TextBlock{Text: "You are JingClaw."}, provider.TextBlock{Text: "Be brief."}},
		MaxOutputTokens: 100,
		Temperature:     new(0.2),
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: provider.Text("read a.txt")},
			{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
				provider.TextBlock{Text: "Reading."},
				provider.ToolUseBlock{ID: "call_1", Name: "read_file", Args: args},
			}},
			{Role: provider.RoleTool, Content: []provider.ContentBlock{
				provider.ToolResultBlock{ToolUseID: "call_1", Name: "read_file", Content: "file text"},
			}},
			{Role: provider.RoleUser, Content: provider.Text("thanks")},
		},
		Tools: []provider.ToolDeclaration{{
			Name: "read_file", Description: "Reads a file.", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	var sent map[string]any
	if err := json.Unmarshal(server.bodies[0], &sent); err != nil {
		t.Fatal(err)
	}
	if sent["store"] != false || sent["stream"] != true || sent["model"] != "gpt-model" {
		t.Errorf("store/stream/model are %v/%v/%v", sent["store"], sent["stream"], sent["model"])
	}
	for _, field := range []string{
		"temperature", "top_p", "max_output_tokens", "metadata", "user", "truncation", "previous_response_id",
		"background", "conversation", "max_tool_calls", "moderation", "prompt", "safety_identifier", "reasoning",
	} {
		if _, present := sent[field]; present {
			t.Errorf("the request carries %s", field)
		}
	}
	if sent["instructions"] != "You are JingClaw.\n\nBe brief." {
		t.Errorf("instructions are %q", sent["instructions"])
	}

	input, _ := sent["input"].([]any)
	var shapes []string
	for _, item := range input {
		one := item.(map[string]any)
		if one["role"] == "system" {
			t.Errorf("a system item is sent: %v", one)
		}
		shapes = append(shapes, one["type"].(string)+"/"+str(one["role"]))
	}
	want := "message/user message/assistant function_call/ function_call_output/ message/user"
	if strings.Join(shapes, " ") != want {
		t.Errorf("the history is %v, want %s (all of it, resent)", shapes, want)
	}
	call := input[2].(map[string]any)
	if call["call_id"] != "call_1" || call["name"] != "read_file" || call["arguments"] != `{"path":"a.txt"}` {
		t.Errorf("the function call is %v", call)
	}
	output := input[3].(map[string]any)
	if output["call_id"] != "call_1" || output["output"] != "file text" {
		t.Errorf("the function output is %v", output)
	}

	tools := sent["tools"].([]any)
	tool := tools[0].(map[string]any)
	if len(tools) != 1 || tool["type"] != "function" || tool["name"] != "read_file" || tool["parameters"] == nil {
		t.Errorf("the tools are %v", tools)
	}
	if got := server.headers[0].Get("Authorization"); got != "Bearer token-0" {
		t.Errorf("authorization is %q", got)
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func TestReasoningEffortIsSentWhenSetAndOmittedWhenNot(t *testing.T) {
	server := newResponses(t, reply{body: completed})
	p := New(Config{Tokens: &tokens{}, BaseURL: server.server.URL + "/v1", Model: "m", ReasoningEffort: "low"})
	if _, err := drain(t, p, provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: provider.Text("hi")}}}); err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Reasoning struct{ Effort string } `json:"reasoning"`
	}
	_ = json.Unmarshal(server.bodies[0], &sent)
	if sent.Reasoning.Effort != "low" {
		t.Errorf("reasoning is %+v", sent.Reasoning)
	}
}

func TestAnImageIsSentAsAnInputImageAndNoSystemItemIsMadeWithoutSystemText(t *testing.T) {
	server := newResponses(t, reply{body: completed})
	_, err := drain(t, server.provider(&tokens{}), provider.Request{Messages: []provider.Message{{
		Role: provider.RoleUser, Content: []provider.ContentBlock{
			provider.TextBlock{Text: "what is this"}, provider.ImageBlock{MediaType: "image/png", Data: []byte{1, 2, 3}},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	body := string(server.bodies[0])
	if !strings.Contains(body, `"type":"input_image"`) || !strings.Contains(body, "data:image/png;base64,AQID") {
		t.Errorf("the image is not sent: %s", body)
	}
	if strings.Contains(body, "instructions") {
		t.Errorf("empty instructions are sent: %s", body)
	}
}

func TestTextToolCallsUsageAndTheEndOfTheTurnAreReadFromTheStream(t *testing.T) {
	server := newResponses(t, reply{body: sse(
		`{"type":"response.created","response":{"status":"in_progress"}}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`,
		`{"type":"response.output_text.delta","delta":"Let me look."}`,
		`{"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_9","name":"grep","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","delta":"{\"q\":"}`,
		`{"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_9","name":"grep","arguments":"{\"q\":\"x\"}"}}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":20,"output_tokens":5,"input_tokens_details":{"cached_tokens":8},"output_tokens_details":{"reasoning_tokens":2}}}}`,
	)})
	keys := &tokens{}
	events, err := drain(t, server.provider(keys), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, event := range events {
		switch e := event.(type) {
		case provider.ReasoningDelta:
			got = append(got, "reasoning:"+e.Text)
		case provider.TextDelta:
			got = append(got, "text:"+e.Text)
		case provider.ToolCallRequested:
			got = append(got, "call:"+e.ID+":"+e.Name+":"+string(e.Args))
		case provider.UsageDelta:
			if e.Usage != (domain.Usage{InputTokens: 20, CachedInputTokens: 8, OutputTokens: 5, ReasoningTokens: 2}) {
				t.Errorf("usage is %+v", e.Usage)
			}
			got = append(got, "usage")
		case provider.Completed:
			if e.StopReason != domain.StopToolUse {
				t.Errorf("stop reason is %s", e.StopReason)
			}
			got = append(got, "completed")
		}
	}
	want := `reasoning:thinking text:Let me look. call:call_9:grep:{"q":"x"} usage completed`
	if strings.Join(got, " ") != want {
		t.Errorf("events are\n %v\nwant\n %s", got, want)
	}
}

func TestOnlyResponseCompletedIsSuccess(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		kind provider.ErrorKind
		in   string
	}{
		"an interrupted stream": {
			body: sse(completedText), kind: provider.KindTransient, in: "ended before",
		},
		"an incomplete response": {
			body: sse(completedText, `{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"}}}`),
			kind: provider.KindUnknown, in: "max_output_tokens",
		},
		"an incomplete response filtered": {
			body: sse(`{"type":"response.incomplete","response":{"incomplete_details":{"reason":"content_filter"}}}`),
			kind: provider.KindContentFiltered, in: "content_filter",
		},
		"a failed response": {
			body: sse(`{"type":"response.failed","response":{"error":{"code":"server_error","message":"boom"}}}`),
			kind: provider.KindUnknown, in: "boom",
		},
		"an error event": {
			body: sse(`{"type":"error","code":"subscription_sharing_usage_limit_exceeded","message":"x"}`),
			kind: provider.KindQuotaExhausted, in: "ChatGPT Settings",
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := newResponses(t, reply{body: tc.body})
			events, err := drain(t, server.provider(&tokens{}), provider.Request{})
			if err == nil {
				t.Fatalf("this ended as a success: %v", events)
			}
			for _, event := range events {
				if _, done := event.(provider.Completed); done {
					t.Errorf("a Completed event was produced")
				}
			}
			if provider.KindOf(err) != tc.kind || !strings.Contains(err.Error(), tc.in) {
				t.Errorf("the error is %v (kind %s), want kind %s containing %q", err, provider.KindOf(err), tc.kind, tc.in)
			}
		})
	}
}

func TestEveryDocumentedErrorCodeIsMappedToTheRightKindAndRetryBehaviour(t *testing.T) {
	for _, tc := range []struct {
		code      string
		status    int
		param     string
		kind      provider.ErrorKind
		retryable bool
		in        string
	}{
		{"subscription_sharing_usage_limit_exceeded", 429, "", provider.KindQuotaExhausted, false, "ChatGPT Settings, Usage"},
		{"subscription_sharing_usage_unavailable", 503, "", provider.KindOverloaded, true, "retrying"},
		{"subscription_sharing_user_unavailable", 503, "", provider.KindOverloaded, true, "retrying"},
		{"subscription_sharing_user_not_eligible", 403, "", provider.KindAuth, false, "not available for this account"},
		{"subscription_sharing_unsupported_capability", 400, "tools[0]", provider.KindInvalidRequest, false, "(tools[0])"},
		{"subscription_sharing_route_not_supported", 403, "", provider.KindInvalidRequest, false, "route"},
		{"subscription_sharing_invalid_user", 401, "", provider.KindAuth, false, "could not validate"},
		{"chatpass_v2_scope_not_authorized", 403, "", provider.KindAuth, false, "sign in again"},
		{"chatpass_v2_invalid_authorization_context", 403, "", provider.KindAuth, false, "sign in again"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			body := `{"error":{"code":"` + tc.code + `","param":"` + tc.param + `","message":"words"}}`
			server := newResponses(t, reply{status: tc.status, body: body, headers: map[string]string{"x-request-id": "req_1"}})
			keys := &tokens{}
			_, err := drain(t, server.provider(keys), provider.Request{})
			var perr *provider.Error
			if !errors.As(err, &perr) {
				t.Fatalf("got %v", err)
			}
			if perr.Kind != tc.kind || perr.Retryable() != tc.retryable || !strings.Contains(perr.Message, tc.in) || perr.RequestID != "req_1" {
				t.Errorf("got %+v, want kind %s retryable %v containing %q", perr, tc.kind, tc.retryable, tc.in)
			}
			if tc.code == "subscription_sharing_usage_limit_exceeded" {
				if perr.RetryAfter != nil || strings.Contains(strings.ToLower(perr.Message), "resets at") {
					t.Errorf("a reset time was guessed: %+v", perr)
				}
			}
			if len(keys.recorded) != 1 || keys.recorded[0] != tc.code {
				t.Errorf("the code was recorded as %v", keys.recorded)
			}
		})
	}
}

func TestAUsageLimitIsNeverRetriedButAnUnavailableUsageCheckIsRetriedWithinTheBudget(t *testing.T) {
	policy := provider.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}

	limit := newResponses(t, reply{status: 429, body: `{"error":{"code":"subscription_sharing_usage_limit_exceeded"}}`})
	_, err := drain(t, provider.WithRetry(limit.provider(&tokens{}), policy), provider.Request{})
	if err == nil || limit.requests != 1 {
		t.Errorf("a usage limit was sent %d times (%v)", limit.requests, err)
	}

	unavailable := newResponses(t,
		reply{status: 503, body: `{"error":{"code":"subscription_sharing_usage_unavailable"}}`},
		reply{body: completed})
	if _, err := drain(t, provider.WithRetry(unavailable.provider(&tokens{}), policy), provider.Request{}); err != nil || unavailable.requests != 2 {
		t.Errorf("an unavailable usage check was sent %d times (%v)", unavailable.requests, err)
	}

	forever := newResponses(t, reply{status: 503, body: `{"error":{"code":"subscription_sharing_usage_unavailable"}}`})
	if _, err := drain(t, provider.WithRetry(forever.provider(&tokens{}), policy), provider.Request{}); err == nil || forever.requests != 3 {
		t.Errorf("the retries were not bounded: %d requests (%v)", forever.requests, err)
	}
}

func TestADetailBodyIsReadAsDiagnosticTextAndAPlainStatusStillMapsToAKind(t *testing.T) {
	server := newResponses(t, reply{status: 503, body: `{"detail":"direct routing is not enabled"}`})
	_, err := drain(t, server.provider(&tokens{}), provider.Request{})
	if provider.KindOf(err) != provider.KindOverloaded || !strings.Contains(err.Error(), "direct routing is not enabled") {
		t.Errorf("got %v", err)
	}
}

func TestARefusedTokenIsRefreshedOnceAndTheRequestSentAgain(t *testing.T) {
	server := newResponses(t, reply{body: completed, onlyFor: "token-1"})
	keys := &tokens{}
	if _, err := drain(t, server.provider(keys), provider.Request{}); err != nil {
		t.Fatal(err)
	}
	if keys.refreshes != 1 || server.requests != 2 {
		t.Errorf("%d refreshes and %d requests, want 1 and 2", keys.refreshes, server.requests)
	}
}

func TestATokenRefusedAgainAfterTheRefreshIsNotRetriedAndNeverFallsBack(t *testing.T) {
	server := newResponses(t, reply{status: 401, body: `{"error":{"code":"subscription_sharing_invalid_user"}}`})
	keys := &tokens{}
	_, err := drain(t, server.provider(keys), provider.Request{})
	if provider.KindOf(err) != provider.KindAuth || server.requests != 2 || keys.refreshes != 1 {
		t.Errorf("got %v after %d requests and %d refreshes", err, server.requests, keys.refreshes)
	}
}

func TestSigningInAgainIsAskedForOnlyWhenTheRefreshItselfSaysTheSignInIsOver(t *testing.T) {
	server := newResponses(t, reply{status: 401})

	over := &tokens{signedOut: chatgptauth.ErrSignInAgain}
	_, err := drain(t, server.provider(over), provider.Request{})
	if provider.KindOf(err) != provider.KindAuth || !strings.Contains(err.Error(), "sign in with ChatGPT again") {
		t.Errorf("a terminal refresh gave %v", err)
	}

	flaky := &tokens{signedOut: errors.New("could not reach the sign-in service")}
	_, err = drain(t, server.provider(flaky), provider.Request{})
	if provider.KindOf(err) != provider.KindTransient || strings.Contains(err.Error(), "sign in with ChatGPT again") {
		t.Errorf("a refresh that could not be made gave %v", err)
	}
}

func TestNoSignInAndNoPlanUsageAreRefusedBeforeAnythingIsSent(t *testing.T) {
	for _, cause := range []error{chatgptauth.ErrNotSignedIn, chatgptauth.ErrPlanUsageNotGranted} {
		server := newResponses(t, reply{body: completed})
		p := server.provider(refusing{cause})
		_, err := drain(t, p, provider.Request{})
		if provider.KindOf(err) != provider.KindAuth || !errors.Is(err, cause) && !strings.Contains(err.Error(), strings.TrimPrefix(cause.Error(), "chatgpt: ")) {
			t.Errorf("%v gave %v", cause, err)
		}
		if server.requests != 0 {
			t.Errorf("a request was sent with no usable sign-in")
		}
	}
}

type refusing struct{ err error }

func (r refusing) AccessToken(context.Context) (string, error)     { return "", r.err }
func (r refusing) Refresh(context.Context, string) (string, error) { return "", r.err }
func (r refusing) RecordError(string, string)                      {}

func TestTheModelListKeepsTheEntriesMarkedForDisplayAndUsesTheSlug(t *testing.T) {
	server := newResponses(t, reply{body: `{"models":[
		{"slug":"gpt-a","display_name":"GPT A","visibility":"list"},
		{"slug":"gpt-hidden","display_name":"Hidden","visibility":"hide"},
		{"slug":"gpt-b","visibility":"list"},
		{"slug":"","display_name":"No slug","visibility":"list"}]}`})
	models, err := server.provider(&tokens{}).Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "gpt-a" || models[0].DisplayName != "GPT A" || models[1].ID != "gpt-b" || models[1].DisplayName != "gpt-b" {
		t.Errorf("models are %+v", models)
	}
}

func TestTheModelListAFailedRequestIsAMappedError(t *testing.T) {
	server := newResponses(t, reply{status: 403, body: `{"error":{"code":"subscription_sharing_user_not_eligible"}}`})
	_, err := server.provider(&tokens{}).Models(context.Background())
	if provider.KindOf(err) != provider.KindAuth || !strings.Contains(err.Error(), "not available for this account") {
		t.Errorf("got %v", err)
	}
}

func TestAStreamStoppedByTheCallerEndsAsCancelledNotAsAFailedConnection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	p := New(Config{Tokens: &tokens{}, Model: "gpt-test", BaseURL: server.URL + "/v1"})

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := p.Generate(ctx, provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if _, err := stream.Recv(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestAnUnsupportedToolsRefusalSaysTheDeclarationMayBeTheCause(t *testing.T) {
	perr := failure(codeUnsupported, "tools[0]", "words", 400)
	if !strings.Contains(perr.Message, "declares tools") || !strings.Contains(perr.Message, "docs/using-it.md") {
		t.Errorf("got %q", perr.Message)
	}
}

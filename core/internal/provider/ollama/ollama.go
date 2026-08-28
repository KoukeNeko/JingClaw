package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// DefaultBaseURL is a daemon on this machine.
const DefaultBaseURL = "http://localhost:11434"

// CloudBaseURL is Ollama's hosted service, which speaks the same API with a
// credential attached.
const CloudBaseURL = "https://ollama.com"

// Config describes where to find Ollama and how to talk to it.
type Config struct {
	// BaseURL defaults to the local daemon. Point it at CloudBaseURL to use
	// the hosted service.
	BaseURL string

	// APIKey is required by the hosted service and meaningless locally.
	APIKey string

	// KeepAlive is how long a model stays resident after a request. Empty
	// leaves the server's own default, which is what an operator running
	// other workloads on the same machine usually wants.
	KeepAlive string

	// NumCtx asks the server to load the model with this much context.
	// Ollama otherwise sizes it against available memory, which on a busy
	// machine can be a small fraction of what the model supports.
	NumCtx int

	// Think asks for the model's reasoning separately. Sending it to a model
	// that does not think is an error, so it is off unless asked for.
	Think bool

	HTTPClient *http.Client
}

// Provider serves models through an Ollama daemon.
type Provider struct {
	baseURL string
	apiKey  string
	config  Config
	client  *http.Client
}

// New builds a provider. The address is validated now rather than on the first
// request, so a typo is a startup failure and not a mysterious turn.
func New(cfg Config) (*Provider, error) {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}

	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("ollama: %q is not a usable base URL", cfg.BaseURL)
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return nil, fmt.Errorf("ollama: %q is not http or https", parsed.Scheme)
	}

	client := cfg.HTTPClient
	if client == nil {
		// No overall timeout: a long generation is a normal request here, and
		// the caller's context is what bounds it. A client timeout would cut
		// off an answer that was still arriving.
		client = &http.Client{}
	}

	return &Provider{baseURL: base, apiKey: cfg.APIKey, config: cfg, client: client}, nil
}

func (p *Provider) Name() string { return providerName }

// Generate starts a streaming completion.
func (p *Provider) Generate(ctx context.Context, req provider.Request) (provider.Stream, error) {
	body, err := p.buildRequest(req)
	if err != nil {
		return nil, err
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	response, err := p.post(ctx, "/api/chat", encoded)
	if err != nil {
		return nil, err
	}

	if response.StatusCode != http.StatusOK {
		defer func() { _ = response.Body.Close() }()
		failure, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
		return nil, classify(response.StatusCode, failure, req.Model)
	}

	return newStream(response.Body, req.Model), nil
}

func (p *Provider) buildRequest(req provider.Request) (chatRequest, error) {
	messages, err := p.messages(req)
	if err != nil {
		return chatRequest{}, err
	}

	out := chatRequest{
		Model:     req.Model,
		Messages:  messages,
		Stream:    true,
		KeepAlive: p.config.KeepAlive,
	}
	if p.config.Think {
		out.Think = true
	}

	options := map[string]any{}
	if p.config.NumCtx > 0 {
		options["num_ctx"] = p.config.NumCtx
	}
	if req.Temperature != nil {
		options["temperature"] = *req.Temperature
	}
	if req.MaxOutputTokens > 0 {
		// Ollama spells the output bound differently from everyone else.
		options["num_predict"] = req.MaxOutputTokens
	}
	if len(options) > 0 {
		out.Options = options
	}

	for _, declaration := range req.Tools {
		out.Tools = append(out.Tools, wireTool{
			Type: "function",
			Function: wireToolFunction{
				Name:        declaration.Name,
				Description: declaration.Description,
				Parameters:  declaration.InputSchema,
			},
		})
	}

	return out, nil
}

// messages translates the canonical conversation.
//
// System instructions become a leading system message rather than a field of
// their own, which is what this API expects.
func (p *Provider) messages(req provider.Request) ([]wireMessage, error) {
	var messages []wireMessage

	if system := blocksToText(req.System); system != "" {
		messages = append(messages, wireMessage{Role: "system", Content: system})
	}

	for _, message := range req.Messages {
		converted, err := convertMessage(message)
		if err != nil {
			return nil, err
		}
		messages = append(messages, converted...)
	}

	return messages, nil
}

func convertMessage(message provider.Message) ([]wireMessage, error) {
	// A tool result is its own message here, not a block inside a user turn,
	// so one canonical message can become several.
	var (
		out     []wireMessage
		current = wireMessage{Role: string(message.Role)}
		text    strings.Builder
	)

	for _, block := range message.Content {
		switch content := block.(type) {
		case provider.TextBlock:
			text.WriteString(content.Text)

		case provider.ImageBlock:
			// Raw base64, with no data: prefix and no media type: the server
			// sniffs it itself.
			current.Images = append(current.Images,
				base64.StdEncoding.EncodeToString(content.Data))

		case provider.ToolUseBlock:
			current.ToolCalls = append(current.ToolCalls, wireToolCall{
				Function: wireToolCallFunction{
					Name:      content.Name,
					Arguments: content.Args,
				},
			})

		case provider.ToolResultBlock:
			out = append(out, wireMessage{
				Role:     "tool",
				ToolName: content.Name,
				Content:  content.Content,
			})

		default:
			return nil, fmt.Errorf("ollama: cannot send a %T", block)
		}
	}

	current.Content = text.String()
	if current.Content != "" || len(current.Images) > 0 || len(current.ToolCalls) > 0 {
		// The assistant turn goes before any tool results it produced, which
		// is the order the model wrote them in.
		out = append([]wireMessage{current}, out...)
	}

	return out, nil
}

func blocksToText(blocks []provider.ContentBlock) string {
	var text strings.Builder
	for _, block := range blocks {
		if content, ok := block.(provider.TextBlock); ok {
			text.WriteString(content.Text)
		}
	}
	return text.String()
}

func (p *Provider) post(ctx context.Context, path string, body []byte) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	p.authorize(request)

	response, err := p.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Nothing was served, so nothing is known about why. Transient is the
		// right guess for a connection that did not complete: a daemon
		// restarting is the common case.
		return nil, &apiError{
			kind:    provider.KindTransient,
			message: err.Error(),
		}
	}
	return response, nil
}

func (p *Provider) get(ctx context.Context, path string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	p.authorize(request)

	response, err := p.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &apiError{kind: provider.KindTransient, message: err.Error()}
	}
	return response, nil
}

func (p *Provider) authorize(request *http.Request) {
	if p.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
}

// stream reads the NDJSON body of a chat response.
//
// One JSON object per line, rather than server-sent events. The important
// difference from a failing HTTP request is that a generation which breaks
// after it has begun still has status 200: the failure arrives as a field on
// a line, and a reader that only checks the status never sees it.
type stream struct {
	body    io.ReadCloser
	reader  *bufio.Reader
	model   string
	pending []provider.Event
	done    bool
}

func newStream(body io.ReadCloser, model string) *stream {
	return &stream{
		body: body,
		// A generous buffer: a single line carries a whole tool call, and tool
		// arguments are routinely larger than a default scanner will accept.
		reader: bufio.NewReaderSize(body, 64*1024),
		model:  model,
	}
}

func (s *stream) Recv(ctx context.Context) (provider.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	for {
		if len(s.pending) > 0 {
			next := s.pending[0]
			s.pending = s.pending[1:]
			return next, nil
		}
		if s.done {
			return nil, io.EOF
		}

		line, err := s.readLine()
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		var chunk chatChunk
		if err := json.Unmarshal(line, &chunk); err != nil {
			return nil, &apiError{
				kind:    provider.KindTransient,
				model:   s.model,
				message: fmt.Sprintf("unreadable response line: %v", err),
			}
		}

		if chunk.Error != "" {
			// The status said 200 because it was 200 when the headers were
			// written. This is the only place the failure appears.
			return nil, classify(http.StatusOK, line, s.model)
		}

		s.pending = append(s.pending, eventsFrom(chunk)...)
	}
}

// readLine reads one NDJSON line without the 64KiB ceiling a scanner imposes.
func (s *stream) readLine() ([]byte, error) {
	var full []byte
	for {
		chunk, err := s.reader.ReadSlice('\n')
		full = append(full, chunk...)

		switch err {
		case nil:
			return full, nil
		case bufio.ErrBufferFull:
			// A line longer than the buffer, which a large tool call
			// produces. Keep going rather than truncating it into invalid
			// JSON.
			continue
		case io.EOF:
			if len(bytes.TrimSpace(full)) > 0 {
				return full, nil
			}
			return nil, io.EOF
		default:
			return nil, err
		}
	}
}

func eventsFrom(chunk chatChunk) []provider.Event {
	var events []provider.Event

	if chunk.Message.Thinking != "" {
		events = append(events, provider.ReasoningDelta{Text: chunk.Message.Thinking})
	}
	if chunk.Message.Content != "" {
		events = append(events, provider.TextDelta{Text: chunk.Message.Content})
	}

	for _, call := range chunk.Message.ToolCalls {
		// No id is sent. The runtime assigns one, which is what pairs the
		// eventual result with this request.
		events = append(events, provider.ToolCallRequested{
			Name: call.Function.Name,
			Args: call.Function.Arguments,
		})
	}

	if !chunk.Done {
		return events
	}

	if chunk.PromptEvalCount > 0 || chunk.EvalCount > 0 {
		events = append(events, provider.UsageDelta{Usage: domain.Usage{
			InputTokens:  chunk.PromptEvalCount,
			OutputTokens: chunk.EvalCount,
		}})
	}

	events = append(events, provider.Completed{
		StopReason: stopReasonFor(chunk),
		RawReason:  chunk.DoneReason,
	})
	return events
}

// stopReasonFor maps a done_reason, keeping the original either way.
func stopReasonFor(chunk chatChunk) domain.StopReason {
	if len(chunk.Message.ToolCalls) > 0 {
		return domain.StopToolUse
	}

	switch chunk.DoneReason {
	case "stop", "":
		return domain.StopEndTurn
	case "length":
		return domain.StopMaxTokens
	case "load", "unload":
		// Not a generation at all: the server reporting that it moved a model
		// in or out of memory.
		return domain.StopEndTurn
	default:
		return domain.StopUnknown
	}
}

func (s *stream) Close() error { return s.body.Close() }

// jsonRequest issues a request and decodes a JSON body into out.
func (p *Provider) jsonRequest(ctx context.Context, method, path string, body, out any) error {
	var (
		response *http.Response
		err      error
	)

	switch method {
	case http.MethodGet:
		response, err = p.get(ctx, path)
	default:
		encoded, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			return marshalErr
		}
		response, err = p.post(ctx, path, encoded)
	}
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(response.Body, 8*1024*1024))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return classify(response.StatusCode, payload, "")
	}

	return json.Unmarshal(payload, out)
}

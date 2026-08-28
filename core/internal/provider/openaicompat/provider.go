package openaicompat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// Config describes one endpoint.
type Config struct {
	// BaseURL is the root the chat path hangs off, usually ending in /v1.
	BaseURL string

	// APIKey is sent as a bearer token. Local servers usually need none.
	APIKey string

	// Profile names what this server does differently. Empty means generic.
	Profile string

	// Name identifies this endpoint in logs, so two configured endpoints can
	// be told apart when one of them is failing.
	Name string

	HTTPClient *http.Client
}

// Provider serves models through an OpenAI-compatible endpoint.
type Provider struct {
	baseURL string
	apiKey  string
	name    string
	profile Profile
	client  *http.Client
}

func New(cfg Config) (*Provider, error) {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		return nil, errors.New("openaicompat: no base URL")
	}

	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("openaicompat: %q is not a usable base URL", cfg.BaseURL)
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return nil, fmt.Errorf("openaicompat: %q is not http or https", parsed.Scheme)
	}

	profile, ok := ProfileByName(cfg.Profile)
	if !ok {
		return nil, fmt.Errorf("openaicompat: no profile named %q; known profiles are %s",
			cfg.Profile, strings.Join(ProfileNames(), ", "))
	}

	name := cfg.Name
	if name == "" {
		name = profile.Name
	}

	client := cfg.HTTPClient
	if client == nil {
		// No client-wide timeout: a long generation is an ordinary request,
		// and the caller's context is what bounds it.
		client = &http.Client{}
	}

	return &Provider{
		baseURL: base,
		apiKey:  cfg.APIKey,
		name:    name,
		profile: profile,
		client:  client,
	}, nil
}

func (p *Provider) Name() string { return "openai_compat/" + p.name }

// Profile reports which dialect this endpoint was configured as.
func (p *Provider) Profile() string { return p.profile.Name }

func (p *Provider) Generate(ctx context.Context, req provider.Request) (provider.Stream, error) {
	body, err := p.buildRequest(req)
	if err != nil {
		return nil, err
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	response, err := p.send(ctx, http.MethodPost, "/chat/completions", encoded)
	if err != nil {
		return nil, err
	}

	if response.StatusCode != http.StatusOK {
		defer func() { _ = response.Body.Close() }()
		failure, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
		return nil, classify(p.profile, response.StatusCode, response.Header, failure, req.Model)
	}

	return newStream(response.Body, p.profile, req.Model), nil
}

func (p *Provider) buildRequest(req provider.Request) (chatRequest, error) {
	messages, err := p.messages(req)
	if err != nil {
		return chatRequest{}, err
	}

	out := chatRequest{
		Model:       req.Model,
		Messages:    messages,
		Stream:      true,
		MaxTokens:   req.MaxOutputTokens,
		Temperature: req.Temperature,
	}
	if p.profile.AsksForUsage {
		out.StreamOptions = &streamOptions{IncludeUsage: true}
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

func (p *Provider) messages(req provider.Request) ([]wireMessage, error) {
	var messages []wireMessage

	if system := textOf(req.System); system != "" {
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
	var (
		out     []wireMessage
		current = wireMessage{Role: string(message.Role)}
		text    strings.Builder
		parts   []contentPart
		images  bool
	)

	for _, block := range message.Content {
		switch content := block.(type) {
		case provider.TextBlock:
			text.WriteString(content.Text)

		case provider.ImageBlock:
			images = true
			parts = append(parts, contentPart{
				Type: "image_url",
				ImageURL: &imageURL{
					URL: "data:" + content.MediaType + ";base64," +
						base64.StdEncoding.EncodeToString(content.Data),
				},
			})

		case provider.ToolUseBlock:
			index := len(current.ToolCalls)
			current.ToolCalls = append(current.ToolCalls, wireToolCall{
				Index: &index,
				ID:    content.ID,
				Type:  "function",
				Function: wireToolCallFragment{
					Name:      content.Name,
					Arguments: string(content.Args),
				},
			})

		case provider.ToolResultBlock:
			// A result is its own message, paired to the call by id.
			out = append(out, wireMessage{
				Role:       "tool",
				ToolCallID: content.ToolUseID,
				Name:       content.Name,
				Content:    content.Content,
			})

		default:
			return nil, fmt.Errorf("openaicompat: cannot send a %T", block)
		}
	}

	if images {
		// The array form, which is required once there is an image. A plain
		// string is used otherwise, because some servers accept only that.
		if text.Len() > 0 {
			parts = append([]contentPart{{Type: "text", Text: text.String()}}, parts...)
		}
		current.Content = parts
	} else if text.Len() > 0 {
		current.Content = text.String()
	}

	if current.Content != nil || len(current.ToolCalls) > 0 {
		out = append([]wireMessage{current}, out...)
	}
	return out, nil
}

func textOf(blocks []provider.ContentBlock) string {
	var text strings.Builder
	for _, block := range blocks {
		if content, ok := block.(provider.TextBlock); ok {
			text.WriteString(content.Text)
		}
	}
	return text.String()
}

func (p *Provider) send(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	request, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Accept", "text/event-stream")
	if p.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	response, err := p.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &apiError{
			kind:     provider.KindTransient,
			provider: p.profile.Name,
			message:  err.Error(),
		}
	}
	return response, nil
}

// stream decodes a chat completion.
type stream struct {
	body    io.ReadCloser
	events  *sseReader
	profile Profile
	model   string

	tools   *toolAccumulator
	pending []provider.Event
	done    bool
}

func newStream(body io.ReadCloser, profile Profile, model string) *stream {
	return &stream{
		body:    body,
		events:  newSSEReader(body),
		profile: profile,
		model:   model,
		tools:   newToolAccumulator(),
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

		event, err := s.events.Next()
		if errors.Is(err, io.EOF) {
			// A stream that ended without saying so. Anything assembled still
			// belongs to the caller: dropping it would turn a completed tool
			// call into a turn that did nothing.
			s.done = true
			s.pending = append(s.pending, callEvents(s.tools.takeAll())...)
			continue
		}
		if err != nil {
			return nil, err
		}

		data := strings.TrimSpace(event.Data)
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			s.done = true
			s.pending = append(s.pending, callEvents(s.tools.takeAll())...)
			continue
		}

		produced, err := s.decode([]byte(data))
		if err != nil {
			return nil, err
		}
		s.pending = append(s.pending, produced...)
	}
}

func (s *stream) decode(data []byte) ([]provider.Event, error) {
	var chunk chatChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		// A frame that is not JSON. Not fatal on its own: some servers emit
		// keep-alive text, and abandoning the generation for one unreadable
		// frame loses an answer that was otherwise fine.
		return nil, nil
	}

	if chunk.Error != nil {
		// The status was 200 because it was 200 when the headers went out.
		// This is the only place a failure that began mid-generation appears.
		encoded, _ := json.Marshal(errorEnvelope{Error: chunk.Error})
		return nil, classify(s.profile, http.StatusOK, nil, encoded, s.model)
	}

	var events []provider.Event

	for _, choice := range chunk.Choices {
		if reasoning := reasoningOf(choice.Delta); reasoning != "" {
			events = append(events, provider.ReasoningDelta{Text: reasoning})
		}
		if choice.Delta.Content != "" {
			events = append(events, provider.TextDelta{Text: choice.Delta.Content})
		}
		for _, fragment := range choice.Delta.ToolCalls {
			s.tools.add(choice.Index, fragment)
		}

		if choice.FinishReason == nil {
			continue
		}

		// Finished. Now, and not before, the assembled arguments are complete
		// enough to be worth handing anybody.
		events = append(events, callEvents(s.tools.take(choice.Index))...)
		events = append(events, completedFor(*choice.FinishReason, s.tools.empty()))
	}

	// The usage frame arrives after the last choice, sometimes carrying no
	// choices at all. A reader that stops at the first finish reason never
	// sees it.
	if chunk.Usage != nil {
		events = append(events, provider.UsageDelta{Usage: usageOf(*chunk.Usage)})
	}

	return events, nil
}

// reasoningOf reads whichever of the two field names this server uses.
func reasoningOf(delta wireDelta) string {
	if delta.Reasoning != "" {
		return delta.Reasoning
	}
	return delta.ReasoningContent
}

func callEvents(calls []assembledCall) []provider.Event {
	events := make([]provider.Event, 0, len(calls))
	for _, call := range calls {
		if call.Name == "" {
			// A fragment stream that never named the function. Emitting it
			// would produce a call to nothing.
			continue
		}
		events = append(events, provider.ToolCallRequested{
			ID:   call.ID,
			Name: call.Name,
			Args: call.Arguments,
		})
	}
	return events
}

func completedFor(raw string, noCalls bool) provider.Completed {
	completed := provider.Completed{RawReason: raw}

	switch raw {
	case "stop":
		completed.StopReason = domain.StopEndTurn
	case "length", "max_tokens":
		completed.StopReason = domain.StopMaxTokens
	case "tool_calls", "function_call":
		completed.StopReason = domain.StopToolUse
	case "content_filter":
		completed.StopReason = domain.StopContentFilter
	case "error":
		completed.StopReason = domain.StopError
	default:
		// Kept as unknown rather than forced onto the nearest value. Reporting
		// a truncated answer as a normal ending is worse than admitting the
		// reason was not recognised.
		completed.StopReason = domain.StopUnknown
	}

	if !noCalls && completed.StopReason == domain.StopEndTurn {
		completed.StopReason = domain.StopToolUse
	}
	return completed
}

func usageOf(usage wireUsage) domain.Usage {
	out := domain.Usage{
		InputTokens:  usage.PromptTokens,
		OutputTokens: usage.CompletionTokens,
	}
	if usage.PromptTokensDetails != nil {
		out.CachedInputTokens = usage.PromptTokensDetails.CachedTokens
	}
	if usage.CompletionTokensDetails != nil {
		out.ReasoningTokens = usage.CompletionTokensDetails.ReasoningTokens
	}
	return out
}

func (s *stream) Close() error { return s.body.Close() }

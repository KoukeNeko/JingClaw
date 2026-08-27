// Package gemini adapts Google's Gemini API to the provider contract.
//
// Everything vendor-specific stops here. The runtime above sees only
// provider.Event values, so swapping this out for another model changes
// nothing about persistence, cancellation or the control protocol.
package gemini

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
	"sync"

	"google.golang.org/genai"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

const providerName = "gemini"

// Provider talks to the Gemini API with an API key from Google AI Studio.
type Provider struct {
	client *genai.Client
}

var _ provider.Provider = (*Provider)(nil)

// Config carries what the adapter needs to connect.
type Config struct {
	// APIKey is required. It is never logged, never echoed and never included
	// in an error message.
	APIKey string
}

func New(ctx context.Context, cfg Config) (*Provider, error) {
	if cfg.APIKey == "" {
		return nil, provider.NewError(provider.KindAuth, providerName, "",
			"no API key configured", nil)
	}

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  cfg.APIKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		// The SDK error can echo configuration back; classify it rather than
		// pass it through, so a key can never reach a log by accident.
		return nil, provider.NewError(provider.KindAuth, providerName, "",
			"could not create client", redact(err, cfg.APIKey))
	}

	return &Provider{client: client}, nil
}

func (p *Provider) Name() string { return providerName }

// Models asks the API what it can serve rather than shipping a hardcoded list
// that goes stale. Clients then ask the daemon, so a model catalog exists in
// exactly one place.
func (p *Provider) Models(ctx context.Context) ([]provider.ModelInfo, error) {
	var models []provider.ModelInfo

	for model, err := range p.client.Models.All(ctx) {
		if err != nil {
			return nil, p.classify(err, "")
		}
		if model == nil || !supportsGeneration(model) {
			continue
		}

		models = append(models, provider.ModelInfo{
			ID:              strings.TrimPrefix(model.Name, "models/"),
			DisplayName:     model.DisplayName,
			Description:     model.Description,
			ContextWindow:   int64(model.InputTokenLimit),
			MaxOutputTokens: int64(model.OutputTokenLimit),
			Capabilities: provider.Capabilities{
				Streaming: true,
				// Tool calling and structured output arrive with the tool
				// loop; claiming them before the runtime can honour them
				// would make capability discovery a lie.
				Vision: true,
			},
		})
	}

	return models, nil
}

// supportsGeneration filters out embedding-only and other non-chat models.
func supportsGeneration(model *genai.Model) bool {
	for _, action := range model.SupportedActions {
		if action == "generateContent" || action == "streamGenerateContent" {
			return true
		}
	}
	// An empty action list means the API did not say; assume usable rather
	// than hide a model the user explicitly asked for.
	return len(model.SupportedActions) == 0
}

func (p *Provider) Generate(ctx context.Context, req provider.Request) (provider.Stream, error) {
	if req.Model == "" {
		return nil, provider.NewError(provider.KindInvalidRequest, providerName, "",
			"no model specified", nil)
	}

	contents, err := toContents(req.Messages)
	if err != nil {
		return nil, provider.NewError(provider.KindInvalidRequest, providerName, req.Model,
			err.Error(), err)
	}

	config := &genai.GenerateContentConfig{}
	if len(req.System) > 0 {
		system, err := toParts(req.System)
		if err != nil {
			return nil, provider.NewError(provider.KindInvalidRequest, providerName, req.Model,
				err.Error(), err)
		}
		config.SystemInstruction = &genai.Content{Parts: system}
	}
	if req.MaxOutputTokens > 0 {
		config.MaxOutputTokens = int32(req.MaxOutputTokens)
	}
	if req.Temperature != nil {
		temperature := float32(*req.Temperature)
		config.Temperature = &temperature
	}

	// The SDK returns a pull-style iterator. Errors surface per item, so the
	// request has not actually been made yet at this point; a failure to
	// connect appears on the first Recv.
	seq := p.client.Models.GenerateContentStream(ctx, req.Model, contents, config)

	return newStream(p, req.Model, seq), nil
}

// classify turns an SDK error into something the runtime can act on without
// knowing anything about Gemini.
func (p *Provider) classify(err error, model string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	var apiErr genai.APIError
	if !errors.As(err, &apiErr) {
		return &providerError{
			kind: provider.KindTransient, model: model, message: err.Error(), cause: err,
		}
	}

	kind := provider.KindUnknown
	switch apiErr.Code {
	case http.StatusTooManyRequests:
		kind = provider.KindRateLimited
	case http.StatusUnauthorized, http.StatusForbidden:
		kind = provider.KindAuth
	case http.StatusNotFound:
		kind = provider.KindNotFound
	case http.StatusBadRequest:
		// Gemini reports an oversized prompt as a 400, so the message is the
		// only way to tell "fix the request" from "compact the context".
		if mentionsTokenLimit(apiErr.Message) {
			kind = provider.KindContextOverflow
		} else {
			kind = provider.KindInvalidRequest
		}
	case http.StatusServiceUnavailable:
		kind = provider.KindOverloaded
	default:
		if apiErr.Code >= 500 {
			kind = provider.KindTransient
		} else if apiErr.Code >= 400 {
			kind = provider.KindInvalidRequest
		}
	}

	return &providerError{
		kind:       kind,
		model:      model,
		statusCode: apiErr.Code,
		message:    apiErr.Message,
		cause:      err,
	}
}

func mentionsTokenLimit(message string) bool {
	lowered := strings.ToLower(message)
	return strings.Contains(lowered, "token") &&
		(strings.Contains(lowered, "exceed") || strings.Contains(lowered, "too long") ||
			strings.Contains(lowered, "limit"))
}

// providerError adapts to provider.Error without exporting a second error type.
type providerError struct {
	kind       provider.ErrorKind
	model      string
	statusCode int
	message    string
	cause      error
}

func (e *providerError) Error() string {
	base := fmt.Sprintf("provider %s (%s): %s", providerName, e.model, e.kind)
	if e.message != "" {
		base += ": " + e.message
	}
	if e.statusCode != 0 {
		base += fmt.Sprintf(" [status %d]", e.statusCode)
	}
	return base
}

func (e *providerError) Unwrap() error { return e.cause }

func (e *providerError) As(target any) bool {
	perr, ok := target.(**provider.Error)
	if !ok {
		return false
	}
	*perr = &provider.Error{
		Kind:       e.kind,
		Provider:   providerName,
		Model:      e.model,
		StatusCode: e.statusCode,
		Message:    e.message,
	}
	return true
}

// redact removes a secret from an error message. Credentials must not reach a
// log even when an SDK decides to include them in its own error text.
func redact(err error, secret string) error {
	if err == nil || secret == "" {
		return err
	}
	message := strings.ReplaceAll(err.Error(), secret, "[redacted]")
	return errors.New(message)
}

func toContents(messages []provider.Message) ([]*genai.Content, error) {
	contents := make([]*genai.Content, 0, len(messages))

	for _, message := range messages {
		parts, err := toParts(message.Content)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 {
			continue
		}

		contents = append(contents, &genai.Content{
			Role:  geminiRole(message.Role),
			Parts: parts,
		})
	}

	if len(contents) == 0 {
		return nil, errors.New("request has no content")
	}
	return contents, nil
}

func toParts(blocks []provider.ContentBlock) ([]*genai.Part, error) {
	parts := make([]*genai.Part, 0, len(blocks))

	for _, block := range blocks {
		switch b := block.(type) {
		case provider.TextBlock:
			if b.Text == "" {
				continue
			}
			parts = append(parts, genai.NewPartFromText(b.Text))
		default:
			return nil, fmt.Errorf("gemini: unsupported content block %T", block)
		}
	}

	return parts, nil
}

// geminiRole maps the canonical roles. Gemini calls the assistant "model".
func geminiRole(role provider.Role) string {
	if role == provider.RoleAssistant {
		return genai.RoleModel
	}
	return genai.RoleUser
}

func toStopReason(reason genai.FinishReason) domain.StopReason {
	switch reason {
	case genai.FinishReasonStop:
		return domain.StopEndTurn
	case genai.FinishReasonMaxTokens:
		return domain.StopMaxTokens
	case genai.FinishReasonSafety, genai.FinishReasonRecitation, genai.FinishReasonBlocklist,
		genai.FinishReasonProhibitedContent, genai.FinishReasonSPII:
		return domain.StopContentFilter
	case "":
		return domain.StopEndTurn
	default:
		return domain.StopError
	}
}

func toUsage(metadata *genai.GenerateContentResponseUsageMetadata) domain.Usage {
	if metadata == nil {
		return domain.Usage{}
	}
	return domain.Usage{
		InputTokens:       int64(metadata.PromptTokenCount),
		CachedInputTokens: int64(metadata.CachedContentTokenCount),
		OutputTokens:      int64(metadata.CandidatesTokenCount),
		ReasoningTokens:   int64(metadata.ThoughtsTokenCount),
	}
}

// stream converts the SDK's push iterator into the pull interface the runtime
// expects, buffering the events produced by each response chunk.
type stream struct {
	provider *Provider
	model    string

	once sync.Once
	next func() (*genai.GenerateContentResponse, error, bool)
	stop func()

	pending  []provider.Event
	finished bool
}

func newStream(p *Provider, model string, seq iter.Seq2[*genai.GenerateContentResponse, error]) *stream {
	next, stop := iter.Pull2(seq)
	return &stream{provider: p, model: model, next: next, stop: stop}
}

func (s *stream) Recv(ctx context.Context) (provider.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	for {
		if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return ev, nil
		}
		if s.finished {
			return nil, io.EOF
		}

		resp, err := s.pullNext(ctx)
		if err != nil {
			return nil, err
		}
		if resp == nil {
			s.finished = true
			continue
		}

		s.pending = append(s.pending, eventsFrom(resp)...)
	}
}

// pullNext advances the SDK iterator. It returns (nil, nil) at end of stream.
func (s *stream) pullNext(ctx context.Context) (*genai.GenerateContentResponse, error) {
	resp, err, ok := s.next()
	if !ok {
		return nil, nil
	}
	if err != nil {
		// Prefer the caller's cancellation over whatever the SDK reported, so
		// an interrupt is not misfiled as a provider failure.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, s.provider.classify(err, s.model)
	}
	return resp, nil
}

// eventsFrom flattens one response chunk into normalized events.
func eventsFrom(resp *genai.GenerateContentResponse) []provider.Event {
	var events []provider.Event

	for _, candidate := range resp.Candidates {
		if candidate == nil {
			continue
		}

		if candidate.Content != nil {
			for _, part := range candidate.Content.Parts {
				// Thought parts are billed reasoning, not answer text; passing
				// them through would put the model's scratchpad in the reply.
				if part == nil || part.Text == "" || part.Thought {
					continue
				}
				events = append(events, provider.TextDelta{Text: part.Text})
			}
		}

		if candidate.FinishReason != "" {
			events = append(events, provider.Completed{
				StopReason: toStopReason(candidate.FinishReason),
			})
		}
	}

	// Usage arrives alongside content and is cumulative, so it is emitted
	// after the text it accounts for.
	if resp.UsageMetadata != nil {
		events = append(events, provider.UsageDelta{Usage: toUsage(resp.UsageMetadata)})
	}

	return events
}

func (s *stream) Close() error {
	s.once.Do(func() {
		if s.stop != nil {
			s.stop()
		}
	})
	return nil
}

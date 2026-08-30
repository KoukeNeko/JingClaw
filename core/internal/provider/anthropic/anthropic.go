package anthropic

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// DefaultBaseURL is the service. Overridable so a test can be the service.
const DefaultBaseURL = "https://api.anthropic.com/v1"

// defaultMaxTokens is what a request asks for when nothing said.
//
// The API requires the field, unlike every other backend here, so there has
// to be an answer. Large enough not to cut an ordinary answer in half.
const defaultMaxTokens = 8192

// Config is what the adapter needs.
type Config struct {
	APIKey  string
	Model   string
	BaseURL string

	HTTP *http.Client
}

// Provider talks to the Messages API.
type Provider struct {
	config Config
	client *http.Client
	base   string
}

func New(cfg Config) (*Provider, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("anthropic: no API key")
	}

	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}

	client := cfg.HTTP
	if client == nil {
		// No overall timeout: a long generation is not a stalled one, and the
		// request's context is what ends it.
		client = &http.Client{}
	}

	return &Provider{config: cfg, client: client, base: base}, nil
}

func (p *Provider) Name() string { return "anthropic" }

// Models is what this adapter will serve.
//
// Asked of the service rather than listed here, so a model released after
// this was written is usable without changing it.
func (p *Provider) Models(ctx context.Context) ([]provider.ModelInfo, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+"/models", nil)
	if err != nil {
		return nil, err
	}
	p.authenticate(request)

	response, err := p.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("anthropic: list models: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return nil, statusError(response.StatusCode, readBody(response.Body))
	}

	var listed struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&listed); err != nil {
		return nil, fmt.Errorf("anthropic: read the model list: %w", err)
	}

	models := make([]provider.ModelInfo, 0, len(listed.Data))
	for _, one := range listed.Data {
		models = append(models, provider.ModelInfo{
			ID:          one.ID,
			DisplayName: one.DisplayName,
			// Not reported by the service. Left to the catalogue and the
			// operator rather than guessed: a window invented here would be
			// wrong quietly, which is the failure this field exists to stop.
			Capabilities: provider.Capabilities{Streaming: true, Tools: true},
		})
	}
	return models, nil
}

func (p *Provider) authenticate(request *http.Request) {
	request.Header.Set("x-api-key", p.config.APIKey)
	request.Header.Set("anthropic-version", Version)
	request.Header.Set("content-type", "application/json")
}

// Generate starts a streaming completion.
func (p *Provider) Generate(ctx context.Context, req provider.Request) (provider.Stream, error) {
	body, err := json.Marshal(p.encode(req))
	if err != nil {
		return nil, fmt.Errorf("anthropic: encode the request: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.base+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	p.authenticate(request)
	request.Header.Set("accept", "text/event-stream")

	response, err := p.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("anthropic: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		defer func() { _ = response.Body.Close() }()
		return nil, statusError(response.StatusCode, readBody(response.Body))
	}

	return newStream(response.Body), nil
}

// encode turns a canonical request into the wire shape.
func (p *Provider) encode(req provider.Request) request {
	model := req.Model
	if model == "" {
		model = p.config.Model
	}

	maxTokens := req.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	out := request{
		Model:       model,
		MaxTokens:   maxTokens,
		Temperature: req.Temperature,
		Stream:      true,
	}

	for _, block := range req.System {
		if text, ok := block.(provider.TextBlock); ok && text.Text != "" {
			out.System = append(out.System, systemBlock{Type: "text", Text: text.Text})
		}
	}

	for _, one := range req.Messages {
		if encoded, ok := encodeMessage(one); ok {
			out.Messages = append(out.Messages, encoded)
		}
	}

	for _, declared := range req.Tools {
		out.Tools = append(out.Tools, toolSpec{
			Name:        declared.Name,
			Description: declared.Description,
			InputSchema: declared.InputSchema,
		})
	}

	return out
}

// encodeMessage turns one canonical message into the wire shape.
//
// A tool result is a block in a user message here rather than a role of its
// own, which is the one place the two shapes genuinely disagree.
func encodeMessage(one provider.Message) (message, bool) {
	role := "user"
	if one.Role == provider.RoleAssistant {
		role = "assistant"
	}

	out := message{Role: role}
	for _, content := range one.Content {
		switch held := content.(type) {
		case provider.TextBlock:
			if held.Text != "" {
				out.Content = append(out.Content, block{Type: "text", Text: held.Text})
			}

		case provider.ToolUseBlock:
			out.Content = append(out.Content, block{
				Type: "tool_use", ID: held.ID, Name: held.Name, Input: held.Args,
			})

		case provider.ToolResultBlock:
			out.Content = append(out.Content, block{
				Type:      "tool_result",
				ToolUseID: held.ToolUseID,
				Content:   held.Content,
				IsError:   held.IsError,
			})

		case provider.ImageBlock:
			out.Content = append(out.Content, block{
				Type: "image",
				Source: &imageSource{
					Type:      "base64",
					MediaType: held.MediaType,
					Data:      base64.StdEncoding.EncodeToString(held.Data),
				},
			})
		}
	}

	// A message with nothing in it is refused by the service, and sending one
	// would fail the whole turn over a block this adapter chose to drop.
	return out, len(out.Content) > 0
}

func readBody(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, 8<<10))
	if err != nil {
		return ""
	}
	return string(bytes.TrimSpace(raw))
}

// statusError reads an HTTP failure as something the runtime can act on.
func statusError(status int, body string) error {
	kind := provider.KindUnknown
	switch {
	case status == http.StatusTooManyRequests:
		kind = provider.KindRateLimited
	case status == http.StatusRequestEntityTooLarge:
		kind = provider.KindContextOverflow
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		kind = provider.KindAuth
	case status == http.StatusServiceUnavailable:
		kind = provider.KindOverloaded
	case status >= 500:
		kind = provider.KindTransient
	case status == http.StatusNotFound:
		kind = provider.KindNotFound
	case status == http.StatusBadRequest:
		kind = provider.KindInvalidRequest
	}

	said := body
	var reported struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &reported) == nil && reported.Error.Message != "" {
		said = reported.Error.Message

		// The status is not always the whole answer: a prompt longer than the
		// context is a 400, and reading it as "the caller sent nonsense" hides
		// the one thing that would fix it.
		if strings.Contains(strings.ToLower(said), "prompt is too long") ||
			strings.Contains(strings.ToLower(reported.Error.Type), "context") {
			kind = provider.KindContextOverflow
		}
	}

	return &provider.Error{
		Provider:   "anthropic",
		Kind:       kind,
		StatusCode: status,
		Message:    said,
	}
}

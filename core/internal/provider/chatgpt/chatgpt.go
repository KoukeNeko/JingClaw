package chatgpt

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

	"github.com/KoukeNeko/JingClaw/core/internal/chatgptauth"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// TokenSource is the signed-in account, as far as this adapter needs it.
// *chatgptauth.Auth is the implementation.
type TokenSource interface {
	AccessToken(ctx context.Context) (string, error)

	// Refresh replaces a token the service refused and returns the one to use.
	Refresh(ctx context.Context, rejected string) (string, error)

	// RecordError remembers the last failure for the dashboard; an empty code
	// clears it.
	RecordError(code, message string)
}

// Config is what the adapter needs.
type Config struct {
	Tokens TokenSource
	Model  string

	// ReasoningEffort is sent as reasoning.effort when set, and omitted when
	// not. The documentation names no allowed values, so none are checked here:
	// one the service does not accept comes back as unsupported_capability.
	ReasoningEffort string

	BaseURL string
	HTTP    *http.Client
}

// Provider talks to the Responses endpoint with the signed-in account's token.
type Provider struct {
	config Config
	client *http.Client
	base   string
}

func New(cfg Config) *Provider {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = chatgptauth.DefaultAPIBase
	}
	client := cfg.HTTP
	if client == nil {
		// No overall timeout: a long generation is not a stalled one, and the
		// request's context is what ends it.
		client = &http.Client{}
	}
	return &Provider{config: cfg, client: client, base: base}
}

func (p *Provider) Name() string { return "chatgpt" }

// do sends a request with the account's token. A 401 replaces the token once
// and sends the request again; a second refusal is returned as it is.
func (p *Provider) do(ctx context.Context, build func(token string) (*http.Request, error)) (*http.Response, error) {
	token, err := p.config.Tokens.AccessToken(ctx)
	if err != nil {
		return nil, p.signInError(err)
	}

	for refreshed := false; ; refreshed = true {
		request, err := build(token)
		if err != nil {
			return nil, err
		}
		response, err := p.client.Do(request)
		if err != nil {
			return nil, &provider.Error{Provider: providerName, Kind: provider.KindTransient,
				Message: "could not reach ChatGPT: " + unwrapped(err)}
		}
		if response.StatusCode != http.StatusUnauthorized || refreshed {
			return response, nil
		}

		// invalid_user and a plain 401 alike: the token may simply have been
		// replaced under us, so refresh once. Signing in again is asked for
		// only when the refresh itself says the sign-in is over.
		_ = response.Body.Close()
		token, err = p.config.Tokens.Refresh(ctx, token)
		if err != nil {
			return nil, p.signInError(err)
		}
	}
}

func unwrapped(err error) string {
	var inner interface{ Unwrap() error }
	if errors.As(err, &inner) && inner.Unwrap() != nil {
		return inner.Unwrap().Error()
	}
	return err.Error()
}

// signInError turns a failure to get a token into the provider's vocabulary.
// Whether it needs the person to sign in is for the message to say; none of it
// is retried.
func (p *Provider) signInError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	kind := provider.KindAuth
	switch {
	case errors.Is(err, chatgptauth.ErrNotSignedIn),
		errors.Is(err, chatgptauth.ErrPlanUsageNotGranted),
		errors.Is(err, chatgptauth.ErrSignInAgain):
	default:
		// A refresh that could not be completed (the service unreachable, a
		// write that failed) says nothing about the sign-in; try again later.
		kind = provider.KindTransient
	}
	return &provider.Error{Provider: providerName, Kind: kind, Message: err.Error()}
}

// Models lists what the account can use: the entries the service marks as
// meant for display.
func (p *Provider) Models(ctx context.Context) ([]provider.ModelInfo, error) {
	response, err := p.do(ctx, func(token string) (*http.Request, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+"/models", nil)
		if err == nil {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		return request, err
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return nil, p.failed(response)
	}

	var listed struct {
		Models []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"display_name"`
			Visibility  string `json:"visibility"`
		} `json:"models"`
	}
	if err := json.NewDecoder(response.Body).Decode(&listed); err != nil {
		return nil, fmt.Errorf("chatgpt: read the model list: %w", err)
	}

	models := make([]provider.ModelInfo, 0, len(listed.Models))
	for _, one := range listed.Models {
		if one.Visibility != "list" || one.Slug == "" {
			continue
		}
		name := one.DisplayName
		if name == "" {
			name = one.Slug
		}
		// The documentation does not give a context window or capabilities for
		// a model, so none is claimed.
		models = append(models, provider.ModelInfo{
			ID: one.Slug, DisplayName: name,
			Capabilities: provider.Capabilities{Streaming: true, Tools: true},
		})
	}
	return models, nil
}

// failed reads a non-200 response and remembers its code for the dashboard.
func (p *Provider) failed(response *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
	perr := statusError(response, bytes.TrimSpace(raw))
	code, _, _ := errorOf(raw)
	p.config.Tokens.RecordError(code, perr.Message)
	return perr
}

// Generate starts a streaming response.
func (p *Provider) Generate(ctx context.Context, req provider.Request) (provider.Stream, error) {
	body, err := json.Marshal(p.encode(req))
	if err != nil {
		return nil, fmt.Errorf("chatgpt: encode the request: %w", err)
	}

	response, err := p.do(ctx, func(token string) (*http.Request, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+"/responses", bytes.NewReader(body))
		if err == nil {
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "text/event-stream")
		}
		return request, err
	})
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		defer func() { _ = response.Body.Close() }()
		return nil, p.failed(response)
	}
	return newStream(response.Body, p.config.Tokens), nil
}

// encode turns a canonical request into the wire shape.
func (p *Provider) encode(req provider.Request) request {
	model := req.Model
	if model == "" {
		model = p.config.Model
	}

	// Stored false and streamed are required of every request in this flow.
	out := request{Model: model, Input: []any{}, Store: false, Stream: true}

	// System text is instructions. A system message item is rejected.
	var instructions []string
	for _, block := range req.System {
		if text, ok := block.(provider.TextBlock); ok && text.Text != "" {
			instructions = append(instructions, text.Text)
		}
	}
	out.Instructions = strings.Join(instructions, "\n\n")

	// The whole history every time: nothing is stored, and previous_response_id
	// is not allowed.
	for _, one := range req.Messages {
		out.Input = append(out.Input, encodeMessage(one)...)
	}

	for _, declared := range req.Tools {
		parameters := declared.InputSchema
		if len(parameters) == 0 {
			parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, toolSpec{
			Type: "function", Name: declared.Name, Description: declared.Description, Parameters: parameters,
		})
	}

	if effort := strings.TrimSpace(p.config.ReasoningEffort); effort != "" {
		out.Reasoning = &reasoning{Effort: effort}
	}
	return out
}

// encodeMessage turns one canonical message into input items. A message may
// become several: text, then each tool call or result as an item of its own,
// in the order they came.
//
// Reasoning items from earlier turns are not replayed. The documentation does
// not say how to carry them with nothing stored.
func encodeMessage(one provider.Message) []any {
	role, kind := "user", "input_text"
	if one.Role == provider.RoleAssistant {
		role, kind = "assistant", "output_text"
	}

	var (
		items []any
		parts []part
	)
	flush := func() {
		if len(parts) > 0 {
			items = append(items, messageItem{Type: "message", Role: role, Content: parts})
			parts = nil
		}
	}

	for _, content := range one.Content {
		switch held := content.(type) {
		case provider.TextBlock:
			if held.Text != "" {
				parts = append(parts, part{Type: kind, Text: held.Text})
			}

		case provider.ImageBlock:
			// The documentation says images work when the selected model
			// accepts them; a model that does not answers with an error.
			parts = append(parts, part{
				Type:     "input_image",
				ImageURL: "data:" + held.MediaType + ";base64," + base64.StdEncoding.EncodeToString(held.Data),
			})

		case provider.ToolUseBlock:
			flush()
			args := string(held.Args)
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			items = append(items, functionCallItem{
				Type: "function_call", CallID: held.ID, Name: held.Name, Arguments: args,
			})

		case provider.ToolResultBlock:
			flush()
			items = append(items, functionOutputItem{
				Type: "function_call_output", CallID: held.ToolUseID, Output: held.Content,
			})
		}
	}
	flush()
	return items
}

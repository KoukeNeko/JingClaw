package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// Embedder turns text into vectors through an OpenAI-compatible /embeddings
// endpoint.
//
// The one protocol is enough: OpenAI serves it, Ollama serves it under /v1,
// and Gemini serves it under its OpenAI-compatible root. A second client per
// vendor would be the same request spelled differently.
type Embedder struct {
	endpoint *Provider
	model    string
}

// NewEmbedder checks the endpoint the way New does and fixes the model.
func NewEmbedder(cfg Config, model string) (*Embedder, error) {
	if model == "" {
		return nil, errors.New("openaicompat: no embedding model")
	}
	endpoint, err := New(cfg)
	if err != nil {
		return nil, err
	}
	return &Embedder{endpoint: endpoint, model: model}, nil
}

// Model names what made the vectors. Vectors from two models are not
// comparable, so this is part of what a stored vector is.
func (e *Embedder) Model() string { return e.model }

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed returns one vector per text, in the order the texts were given.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(embedRequest{Model: e.model, Input: texts})
	if err != nil {
		return nil, err
	}

	p := e.endpoint
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	response, err := p.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &apiError{kind: provider.KindTransient, provider: p.profile.Name, message: err.Error()}
	}
	defer func() { _ = response.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, classify(p.profile, response.StatusCode, response.Header, raw, e.model)
	}

	var decoded embedResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("openaicompat: read embeddings: %w", err)
	}
	if len(decoded.Data) != len(texts) {
		return nil, fmt.Errorf("openaicompat: asked for %d embeddings, got %d",
			len(texts), len(decoded.Data))
	}

	// By the index each one names, not by position: the response is not
	// promised to keep the order of the request.
	out := make([][]float32, len(texts))
	for _, item := range decoded.Data {
		if item.Index < 0 || item.Index >= len(texts) || out[item.Index] != nil {
			return nil, fmt.Errorf("openaicompat: an embedding came back for input %d", item.Index)
		}
		if len(item.Embedding) == 0 {
			return nil, fmt.Errorf("openaicompat: embedding %d is empty", item.Index)
		}
		out[item.Index] = item.Embedding
	}
	return out, nil
}

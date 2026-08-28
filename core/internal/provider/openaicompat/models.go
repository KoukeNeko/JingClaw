package openaicompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// Models lists what this endpoint serves.
//
// The listing is where portability runs out entirely. Some servers return an
// id and nothing else; others carry the context window under one of four
// different names, and one buries it a level down. Every spelling seen in the
// wild is accepted and the most specific one wins, because the alternative —
// inferring a window from a model's name — means telling compaction that a
// model called llama-3.1-8b has 128k when the operator gave it 8k.
func (p *Provider) Models(ctx context.Context) ([]provider.ModelInfo, error) {
	response, err := p.send(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, 8*1024*1024))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, classify(p.profile, response.StatusCode, response.Header, body, "")
	}

	var listing modelsResponse
	if err := json.Unmarshal(body, &listing); err != nil {
		return nil, &apiError{
			kind:     provider.KindTransient,
			provider: p.profile.Name,
			message:  "the model listing was not readable: " + err.Error(),
		}
	}

	models := make([]provider.ModelInfo, 0, len(listing.Data))
	for _, card := range listing.Data {
		if card.ID == "" {
			continue
		}

		info := provider.ModelInfo{
			ID:          card.ID,
			DisplayName: card.ID,
			Capabilities: provider.Capabilities{
				Streaming: true,
				// Whether a given model can use tools is not in this listing
				// for any server that implements it, and a wrong answer here
				// is worse than none: the runtime asks and finds out.
				Tools: true,
			},
		}

		if window, source := contextOf(card); window > 0 {
			info.ContextWindow = window
			info.ContextSource = source
		}
		if trained := trainedOf(card); trained > 0 {
			info.TrainedContext = trained
		}
		if max := maxOutputOf(card); max > 0 {
			info.MaxOutputTokens = max
		}

		models = append(models, info)
	}

	return models, nil
}

// contextOf finds the window, preferring what the server resolved for itself
// over what a catalog says a model can do.
func contextOf(card modelCard) (int64, provider.ContextSource) {
	// What this server actually loaded, which is the strongest claim
	// available here.
	if card.MaxModelLen != nil && *card.MaxModelLen > 0 {
		return *card.MaxModelLen, provider.ContextRuntime
	}

	// A gateway describing the provider it will route to.
	if card.TopProvider != nil && card.TopProvider.ContextLength != nil &&
		*card.TopProvider.ContextLength > 0 {
		return *card.TopProvider.ContextLength, provider.ContextCatalog
	}

	for _, candidate := range []*int64{card.ContextLength, card.ContextWindow} {
		if candidate != nil && *candidate > 0 {
			return *candidate, provider.ContextCatalog
		}
	}

	// The training context, which a server may not have given the model.
	if card.Meta != nil && card.Meta.NCtxTrain != nil && *card.Meta.NCtxTrain > 0 {
		return *card.Meta.NCtxTrain, provider.ContextTrained
	}

	return 0, provider.ContextUnknown
}

func trainedOf(card modelCard) int64 {
	if card.Meta != nil && card.Meta.NCtxTrain != nil {
		return *card.Meta.NCtxTrain
	}
	return 0
}

func maxOutputOf(card modelCard) int64 {
	if card.MaxCompletionTokens != nil && *card.MaxCompletionTokens > 0 {
		return *card.MaxCompletionTokens
	}
	if card.TopProvider != nil && card.TopProvider.MaxCompletionTokens != nil {
		return *card.TopProvider.MaxCompletionTokens
	}
	return 0
}

package ollama

import (
	"context"
	"net/http"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// Models lists what this server can serve, and how much room each one has.
//
// Three calls rather than one, because they answer different questions.
// /api/tags says what is installed. /api/ps says what is loaded right now and
// — the part that matters — how much context it was actually given, which on
// a machine with other work on it is routinely a fraction of what the model
// supports. /api/show says what the weights allow.
//
// Only the first is required. A server that answers the listing and nothing
// else still produces a usable catalog, with less known about it.
func (p *Provider) Models(ctx context.Context) ([]provider.ModelInfo, error) {
	var tags tagsResponse
	if err := p.jsonRequest(ctx, http.MethodGet, "/api/tags", nil, &tags); err != nil {
		return nil, err
	}

	loaded := p.loadedContexts(ctx)

	models := make([]provider.ModelInfo, 0, len(tags.Models))
	for _, tag := range tags.Models {
		id := tag.Model
		if id == "" {
			id = tag.Name
		}

		info := provider.ModelInfo{
			ID:          id,
			DisplayName: tag.Name,
			Description: describe(tag.Details),
			Capabilities: provider.Capabilities{
				Streaming: true,
			},
		}

		// The listing often carries what the weights allow and what the model
		// can do, in which case there is nothing to ask.
		if tag.Details.ContextLength > 0 {
			info.TrainedContext = tag.Details.ContextLength
			info.ContextWindow = tag.Details.ContextLength
			info.ContextSource = provider.ContextTrained
		}
		applyCapabilities(&info, tag.Capabilities)

		// Only when it did not. A request per model is worth avoiding on a
		// daemon serving a dozen of them.
		if info.TrainedContext == 0 || len(tag.Capabilities) == 0 {
			if trained, capabilities, ok := p.describeModel(ctx, id); ok {
				if trained > 0 && info.TrainedContext == 0 {
					info.TrainedContext = trained
					info.ContextWindow = trained
					info.ContextSource = provider.ContextTrained
				}
				if len(tag.Capabilities) == 0 {
					applyCapabilities(&info, capabilities)
				}
			}
		}

		// What the server actually gave it, which outranks the above because
		// it is a fact about now rather than about the model.
		if allocated, ok := loaded[id]; ok && allocated > 0 {
			info.ContextWindow = allocated
			info.ContextSource = provider.ContextRuntime
		}

		models = append(models, info)
	}

	return models, nil
}

// applyCapabilities accepts either shape the daemon reports them in.
func applyCapabilities[T []string | map[string]bool](info *provider.ModelInfo, capabilities T) {
	has := func(name string) bool {
		switch value := any(capabilities).(type) {
		case []string:
			for _, capability := range value {
				if capability == name {
					return true
				}
			}
		case map[string]bool:
			return value[name]
		}
		return false
	}

	if has("tools") {
		info.Capabilities.Tools = true
	}
	if has("vision") {
		info.Capabilities.Vision = true
	}
	if has("completion") {
		info.Capabilities.StructuredOutput = true
	}
}

// loadedContexts reports the context length of every model currently resident.
//
// Best effort: a server that does not answer this is not broken, it just
// leaves the catalog describing the weights rather than the instance.
func (p *Provider) loadedContexts(ctx context.Context) map[string]int64 {
	var running psResponse
	if err := p.jsonRequest(ctx, http.MethodGet, "/api/ps", nil, &running); err != nil {
		return nil
	}

	contexts := make(map[string]int64, len(running.Models))
	for _, model := range running.Models {
		for _, name := range []string{model.Model, model.Name} {
			if name != "" {
				contexts[name] = model.ContextLength
			}
		}
	}
	return contexts
}

// describeModel asks what a model is, and what its weights allow.
func (p *Provider) describeModel(ctx context.Context, id string) (int64, map[string]bool, bool) {
	var shown showResponse
	body := map[string]string{"model": id}
	if err := p.jsonRequest(ctx, http.MethodPost, "/api/show", body, &shown); err != nil {
		return 0, nil, false
	}

	capabilities := make(map[string]bool, len(shown.Capabilities))
	for _, capability := range shown.Capabilities {
		capabilities[capability] = true
	}

	return trainedContext(shown.ModelInfo), capabilities, true
}

// trainedContext finds the context length in a map keyed by architecture.
//
// The key is the model's own architecture — llama.context_length,
// gemma3.context_length, qwen3.context_length — so it cannot be looked up
// directly, only recognised by its suffix.
func trainedContext(modelInfo map[string]any) int64 {
	for key, value := range modelInfo {
		if !strings.HasSuffix(key, ".context_length") {
			continue
		}
		// JSON numbers decode as float64 through an any.
		if number, ok := value.(float64); ok && number > 0 {
			return int64(number)
		}
	}
	return 0
}

func describe(details modelDetails) string {
	parts := make([]string, 0, 2)
	if details.ParameterSize != "" {
		parts = append(parts, details.ParameterSize)
	}
	if details.Family != "" {
		parts = append(parts, details.Family)
	}
	return strings.Join(parts, " ")
}

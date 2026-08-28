package ollama_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/provider/ollama"
)

// catalogServer answers the three endpoints a catalog is built from.
func catalogServer(t *testing.T, ps string) *ollama.Provider {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tags":
			_, _ = io.WriteString(w, `{"models":[{"model":"qwen3:8b","name":"qwen3:8b",
				"details":{"family":"qwen3","parameter_size":"8.2B"}}]}`)
		case "/api/ps":
			_, _ = io.WriteString(w, ps)
		case "/api/show":
			_, _ = io.WriteString(w, `{"capabilities":["completion","tools"],
				"model_info":{"general.architecture":"qwen3","qwen3.context_length":131072}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	p, err := ollama.New(ollama.Config{BaseURL: server.URL})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return p
}

// The reason this adapter uses the native API at all.
//
// A model trained for 131072 that the server loaded with 4096 has 4096. Plan
// against the larger figure and every request is refused, with compaction
// waiting for a threshold that will never be reached.
func TestWhatTheServerLoadedBeatsWhatTheModelAllows(t *testing.T) {
	p := catalogServer(t, `{"models":[{"model":"qwen3:8b","name":"qwen3:8b","context_length":4096}]}`)

	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("got %d models", len(models))
	}

	model := models[0]
	if model.ContextWindow != 4096 {
		t.Errorf("context window is %d, want the 4096 actually loaded", model.ContextWindow)
	}
	if model.TrainedContext != 131072 {
		t.Errorf("trained context is %d, want 131072", model.TrainedContext)
	}
	if model.ContextSource != provider.ContextRuntime {
		t.Errorf("source is %q, want runtime", model.ContextSource)
	}
	if !model.Capabilities.Tools {
		t.Error("a model reporting the tools capability is listed without it")
	}
}

// Nothing loaded, so the weights are the best available answer — and it says
// so, rather than presenting a guess as a measurement.
func TestWithNothingLoadedTheWeightsAreTheAnswer(t *testing.T) {
	p := catalogServer(t, `{"models":[]}`)

	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("models: %v", err)
	}

	if models[0].ContextWindow != 131072 {
		t.Errorf("context window is %d", models[0].ContextWindow)
	}
	if models[0].ContextSource != provider.ContextTrained {
		t.Errorf("source is %q, want trained", models[0].ContextSource)
	}
}

// A server that answers the listing and nothing else still produces a usable
// catalog. Only /api/tags is required.
func TestACatalogSurvivesTheOptionalEndpointsFailing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"models":[{"model":"llama3:8b","name":"llama3:8b"}]}`)
	}))
	t.Cleanup(server.Close)

	p, err := ollama.New(ollama.Config{BaseURL: server.URL})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("a server answering only /api/tags produced no catalog: %v", err)
	}
	if len(models) != 1 || models[0].ID != "llama3:8b" {
		t.Fatalf("models are %+v", models)
	}
	// Nothing established a window, and nothing pretends otherwise.
	if models[0].ContextWindow != 0 || models[0].ContextSource != provider.ContextUnknown {
		t.Errorf("a window was invented: %d from %q",
			models[0].ContextWindow, models[0].ContextSource)
	}
}

// The hosted service is the same API at another address, with a credential.
func TestTheCloudIsTheSameApiWithACredential(t *testing.T) {
	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"models":[]}`)
	}))
	t.Cleanup(server.Close)

	p, err := ollama.New(ollama.Config{BaseURL: server.URL, APIKey: "secret-key"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := p.Models(context.Background()); err != nil {
		t.Fatalf("models: %v", err)
	}

	if seen != "Bearer secret-key" {
		t.Errorf("the credential was not sent: %q", seen)
	}
}

// A local daemon has no credential, and sending an empty one would be a header
// that means something different from sending none.
func TestNoCredentialIsSentWhenThereIsNone(t *testing.T) {
	var present bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["Authorization"]
		_, _ = io.WriteString(w, `{"models":[]}`)
	}))
	t.Cleanup(server.Close)

	p, err := ollama.New(ollama.Config{BaseURL: server.URL})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := p.Models(context.Background()); err != nil {
		t.Fatalf("models: %v", err)
	}

	if present {
		t.Error("an empty credential header was sent to a local daemon")
	}
}

func TestAnUnusableAddressFailsAtStartup(t *testing.T) {
	for _, base := range []string{"not a url", "ftp://example.com", "://missing-scheme"} {
		if _, err := ollama.New(ollama.Config{BaseURL: base}); err == nil {
			t.Errorf("%q was accepted", base)
		}
	}
}

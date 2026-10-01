package openaicompat_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/provider/openaicompat"
)

// The vectors come back by the index each names, the key goes as a bearer
// token, and the model asked for is the one sent.
func TestEmbedReturnsVectorsInTheOrderAsked(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("asked %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("authorization %q", got)
		}
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "embed-small" || len(body.Input) != 2 {
			t.Errorf("request %+v", body)
		}
		// Out of order on purpose.
		_, _ = w.Write([]byte(`{"data":[
			{"index":1,"embedding":[0,1]},
			{"index":0,"embedding":[1,0]}]}`))
	}))
	defer server.Close()

	embedder, err := openaicompat.NewEmbedder(openaicompat.Config{
		BaseURL: server.URL + "/v1", APIKey: "secret",
	}, "embed-small")
	if err != nil {
		t.Fatal(err)
	}

	vectors, err := embedder.Embed(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(vectors) != 2 || vectors[0][0] != 1 || vectors[1][1] != 1 {
		t.Errorf("vectors %v", vectors)
	}
}

// A refusal is classified the way a chat request's would be, so whoever logs
// it can say why.
func TestEmbedFailureIsClassified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer server.Close()

	embedder, err := openaicompat.NewEmbedder(openaicompat.Config{BaseURL: server.URL}, "m")
	if err != nil {
		t.Fatal(err)
	}
	_, err = embedder.Embed(context.Background(), []string{"x"})
	if provider.KindOf(err) != provider.KindAuth {
		t.Errorf("a 401 was %v (%v)", provider.KindOf(err), err)
	}
}

// A response that does not answer every input is an error, not a short list.
func TestEmbedRejectsAShortAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1]}]}`))
	}))
	defer server.Close()

	embedder, err := openaicompat.NewEmbedder(openaicompat.Config{BaseURL: server.URL}, "m")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := embedder.Embed(context.Background(), []string{"a", "b"}); err == nil {
		t.Error("two inputs and one embedding was accepted")
	}
}

// Against a real endpoint, when there is one:
//
//	JINGCLAW_REAL_EMBED=http://localhost:11434/v1 \
//	JINGCLAW_REAL_EMBED_MODEL=qwen3-embedding:0.6b \
//	go test ./internal/provider/openaicompat/ -run Real
func TestRealEmbedding(t *testing.T) {
	base, model := os.Getenv("JINGCLAW_REAL_EMBED"), os.Getenv("JINGCLAW_REAL_EMBED_MODEL")
	if base == "" || model == "" {
		t.Skip("set JINGCLAW_REAL_EMBED and JINGCLAW_REAL_EMBED_MODEL to run against a real endpoint")
	}
	embedder, err := openaicompat.NewEmbedder(openaicompat.Config{BaseURL: base}, model)
	if err != nil {
		t.Fatal(err)
	}
	vectors, err := embedder.Embed(context.Background(), []string{"部署", "deploy"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(vectors) != 2 || len(vectors[0]) == 0 || len(vectors[0]) != len(vectors[1]) {
		t.Errorf("vectors of lengths %d", len(vectors))
	}
}

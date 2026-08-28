package ollama_test

// These run against a daemon that is actually there, and skip when one is
// not. Everything else in this package tests against a stand-in built from
// the documentation, which is exactly how three defects survived: the real
// daemon sends a tool call id the documentation says it does not, carries the
// context length in the listing rather than only in /api/show, and splits a
// tool call and the line saying the turn is over across separate chunks.
//
// Run them with:
//
//	JINGCLAW_REAL_OLLAMA=1 JINGCLAW_REAL_MODEL=gemma4:31b-cloud go test ./internal/provider/ollama/

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/provider/ollama"
)

func realProvider(t *testing.T) (*ollama.Provider, string) {
	t.Helper()

	if os.Getenv("JINGCLAW_REAL_OLLAMA") == "" {
		t.Skip("set JINGCLAW_REAL_OLLAMA to run against a daemon that is actually there")
	}
	model := os.Getenv("JINGCLAW_REAL_MODEL")
	if model == "" {
		t.Skip("set JINGCLAW_REAL_MODEL to the model to ask")
	}

	p, err := ollama.New(ollama.Config{})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	// The daemon lists the catalogue at startup, and that is what teaches the
	// adapter which models can think. Skipping it here would test a shape the
	// daemon never runs in.
	if _, err := p.Models(context.Background()); err != nil {
		t.Fatalf("models: %v", err)
	}
	return p, model
}

// The catalogue, against whatever this daemon actually has.
func TestRealCatalogue(t *testing.T) {
	p, _ := realProvider(t)

	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if len(models) == 0 {
		t.Skip("the daemon has no models")
	}
	for _, model := range models {
		t.Logf("%-28s window=%-7d trained=%-7d from=%-8s tools=%v vision=%v",
			model.ID, model.ContextWindow, model.TrainedContext, model.ContextSource,
			model.Capabilities.Tools, model.Capabilities.Vision)

		if model.ContextWindow == 0 {
			t.Errorf("%s has no context window, so compaction has nothing to plan against", model.ID)
		}
	}
}

func TestRealGeneration(t *testing.T) {
	p, model := realProvider(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	stream, err := p.Generate(ctx, provider.Request{
		Model:    model,
		System:   provider.Text("Answer in exactly one short sentence."),
		Messages: []provider.Message{{Role: provider.RoleUser, Content: provider.Text("What is 17 times 3?")}},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	defer func() { _ = stream.Close() }()

	var text, reasoning strings.Builder
	var usage domain.Usage
	var completed *provider.Completed

	for {
		event, err := stream.Recv(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		switch e := event.(type) {
		case provider.TextDelta:
			text.WriteString(e.Text)
		case provider.ReasoningDelta:
			reasoning.WriteString(e.Text)
		case provider.UsageDelta:
			usage = e.Usage
		case provider.Completed:
			completed = &e
		}
	}

	t.Logf("answer:    %q", strings.TrimSpace(text.String()))
	t.Logf("reasoning: %d characters (kept out of the answer)", reasoning.Len())
	t.Logf("usage:     %d in / %d out", usage.InputTokens, usage.OutputTokens)
	if completed != nil {
		t.Logf("stopped:   %s (raw %q)", completed.StopReason, completed.RawReason)
	}

	if !strings.Contains(text.String(), "51") {
		t.Errorf("the model did not answer the question: %q", text.String())
	}
	if usage.InputTokens == 0 {
		t.Error("no usage was reported")
	}
	// This model reports being able to think, so it should have been asked,
	// and what it thought must not have reached the answer.
	if reasoning.Len() == 0 {
		t.Error("a model that can think was not asked to")
	}
	if strings.Contains(text.String(), reasoning.String()) {
		t.Error("the working-out was repeated inside the answer")
	}
}

func TestRealToolCall(t *testing.T) {
	p, model := realProvider(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	stream, err := p.Generate(ctx, provider.Request{
		Model:    model,
		System:   provider.Text("Use the tools you are given."),
		Messages: []provider.Message{{Role: provider.RoleUser, Content: provider.Text("Read the file notes.md")}},
		Tools: []provider.ToolDeclaration{{
			Name:        "read_file",
			Description: "Read a file from the workspace.",
			InputSchema: []byte(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	defer func() { _ = stream.Close() }()

	var calls []provider.ToolCallRequested
	var completed *provider.Completed
	for {
		event, err := stream.Recv(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		switch e := event.(type) {
		case provider.ToolCallRequested:
			calls = append(calls, e)
		case provider.Completed:
			completed = &e
		}
	}

	if len(calls) == 0 {
		t.Fatal("the model was given a tool and asked to use it, and no call arrived")
	}
	for _, call := range calls {
		t.Logf("call: %s args=%s", call.Name, call.Args)

		// The difference from every other API: arguments are an object here,
		// not a string holding one.
		var args map[string]any
		if err := json.Unmarshal(call.Args, &args); err != nil {
			t.Fatalf("the arguments are not usable JSON: %v (%s)", err, call.Args)
		}
		if args["path"] == nil {
			t.Errorf("the model's argument did not survive: %v", args)
		}
	}
	if completed != nil {
		t.Logf("stopped: %s (raw %q)", completed.StopReason, completed.RawReason)
		if completed.StopReason != domain.StopToolUse {
			t.Errorf("a turn that asked for a tool stopped as %s", completed.StopReason)
		}
	}
}

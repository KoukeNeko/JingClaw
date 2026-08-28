package openaicompat_test

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
	"github.com/KoukeNeko/JingClaw/core/internal/provider/openaicompat"
)

// These run against an endpoint that is actually there, and skip when one is
// not. Everything else in this package tests against frames written by hand
// from the documentation, which is exactly how three defects survived the
// Ollama adapter: a hand-written fixture tests an understanding of the
// documentation.
//
// Ollama serves an OpenAI-compatible endpoint at /v1, which makes one real
// implementation available without installing anything:
//
//	JINGCLAW_REAL_COMPAT=http://localhost:11434/v1 \
//	JINGCLAW_REAL_MODEL=gemma4:31b-cloud \
//	go test ./internal/provider/openaicompat/
func realEndpoint(t *testing.T) (*openaicompat.Provider, string) {
	t.Helper()

	base := os.Getenv("JINGCLAW_REAL_COMPAT")
	model := os.Getenv("JINGCLAW_REAL_MODEL")
	if base == "" || model == "" {
		t.Skip("set JINGCLAW_REAL_COMPAT and JINGCLAW_REAL_MODEL to run against a real endpoint")
	}

	p, err := openaicompat.New(openaicompat.Config{
		BaseURL: base,
		Profile: os.Getenv("JINGCLAW_REAL_PROFILE"),
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return p, model
}

func TestRealCatalogue(t *testing.T) {
	p, _ := realEndpoint(t)

	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if len(models) == 0 {
		t.Skip("the endpoint serves nothing")
	}
	for _, model := range models {
		t.Logf("%-28s window=%-7d from=%q", model.ID, model.ContextWindow, model.ContextSource)

		// A listing that says nothing about the window must leave it unknown
		// rather than inventing one. Most of these do say nothing.
		if model.ContextWindow == 0 && model.ContextSource != provider.ContextUnknown {
			t.Errorf("%s has no window but claims the source %q", model.ID, model.ContextSource)
		}
	}
}

func TestRealGeneration(t *testing.T) {
	p, model := realEndpoint(t)

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

	var text strings.Builder
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
		case provider.UsageDelta:
			usage = e.Usage
		case provider.Completed:
			completed = &e
		}
	}

	t.Logf("answer:  %q", strings.TrimSpace(text.String()))
	t.Logf("usage:   %d in / %d out", usage.InputTokens, usage.OutputTokens)
	if completed != nil {
		t.Logf("stopped: %s (raw %q)", completed.StopReason, completed.RawReason)
	}

	if !strings.Contains(text.String(), "51") {
		t.Errorf("the model did not answer: %q", text.String())
	}
	// The usage frame arrives after the finish reason and carries no choices.
	// A reader that stops at the first finish reason never sees it.
	if usage.InputTokens == 0 {
		t.Error("the usage frame was not read")
	}
	if completed == nil || completed.StopReason != domain.StopEndTurn {
		t.Errorf("completed is %+v", completed)
	}
}

// The half that a hand-written fixture is least likely to get right: a call
// delivered across frames, by a real server.
func TestRealToolCall(t *testing.T) {
	p, model := realEndpoint(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	stream, err := p.Generate(ctx, provider.Request{
		Model:    model,
		System:   provider.Text("Use the tools you are given."),
		Messages: []provider.Message{{Role: provider.RoleUser, Content: provider.Text("Read the file notes.md")}},
		Tools: []provider.ToolDeclaration{{
			Name:        "read_file",
			Description: "Read a file from the workspace.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
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
		t.Logf("call: %s id=%q args=%s", call.Name, call.ID, call.Args)

		var args map[string]any
		if err := json.Unmarshal(call.Args, &args); err != nil {
			t.Fatalf("the assembled arguments are not usable JSON: %v (%s)", err, call.Args)
		}
		if args["path"] == nil {
			t.Errorf("the model's argument did not survive reassembly: %v", args)
		}
	}
	if completed != nil {
		t.Logf("stopped: %s (raw %q)", completed.StopReason, completed.RawReason)
		if completed.StopReason != domain.StopToolUse {
			t.Errorf("a turn that asked for a tool stopped as %s", completed.StopReason)
		}
	}
}

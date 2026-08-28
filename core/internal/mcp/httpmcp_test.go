package mcp_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/KoukeNeko/JingClaw/core/internal/mcp"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
)

// A real server over a real HTTP connection, so the transport is exercised
// rather than assumed.
func TestConnectsToAServerOverHTTP(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "over-http", Version: "1"}, nil)
	sdk.AddTool(server,
		&sdk.Tool{Name: "shout", Description: "Return the text in capitals."},
		func(ctx context.Context, req *sdk.CallToolRequest, args struct {
			Text string `json:"text" jsonschema:"the text"`
		}) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{
				Content: []sdk.Content{&sdk.TextContent{Text: "!" + args.Text + "!"}},
			}, nil, nil
		})

	handler := sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return server }, nil)
	endpoint := httptest.NewServer(handler)
	defer endpoint.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	connected, err := mcp.Connect(ctx, mcp.ServerConfig{
		Name:    "remote",
		URL:     endpoint.URL,
		Headers: map[string]string{"Authorization": "Bearer test"},
		Level:   tool.LevelExecute,
	}, mcp.Limits{}, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = connected.Close() }()

	tools := connected.Tools()
	if len(tools) != 1 {
		t.Fatalf("got %d tools", len(tools))
	}
	// Prefixed, so a server installed over HTTP cannot shadow a built-in any
	// more than one started here can.
	if name := tools[0].Spec().Name; name != "mcp_remote_shout" {
		t.Errorf("tool is named %q", name)
	}

	result, err := tools[0].Execute(ctx, tool.Call{
		Name:      tools[0].Spec().Name,
		Arguments: []byte(`{"text":"hello"}`),
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Content != "!hello!" {
		t.Errorf("result is %q", result.Content)
	}
}

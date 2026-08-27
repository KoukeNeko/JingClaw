package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// helperEnv makes the test binary run as an MCP server instead of running
// tests.
//
// The server is a real child process speaking real stdio JSON-RPC, which is
// the only way to cover what actually breaks: spawning, the handshake, the
// process dying, and shutting it down again. An in-memory transport would test
// the adapter against a version of the world where none of that happens.
const helperEnv = "JINGCLAW_TEST_MCP_SERVER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "" {
		os.Exit(m.Run())
	}

	// The client closing its end is how this process is meant to finish, so
	// the resulting EOF is a clean exit rather than something to report.
	if err := runHelperServer(); err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintln(os.Stderr, "helper server:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

type echoInput struct {
	Text string `json:"text" jsonschema:"the text to echo back"`
}

type nameInput struct {
	Name string `json:"name" jsonschema:"the environment variable to report"`
}

type emptyInput struct{}

func runHelperServer() error {
	server := sdk.NewServer(&sdk.Implementation{Name: "helper", Version: "1"}, nil)

	sdk.AddTool(server, &sdk.Tool{
		Name:        "echo",
		Description: "Echo the text back.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in echoInput) (*sdk.CallToolResult, any, error) {
		return textResult(false, "you said: "+in.Text), nil, nil
	})

	// A tool that fails on purpose. Under the contract this is an observation
	// the model receives, not an error the runtime reports upward.
	sdk.AddTool(server, &sdk.Tool{
		Name:        "boom",
		Description: "Always fail.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, _ emptyInput) (*sdk.CallToolResult, any, error) {
		return textResult(true, "the thing you asked for does not exist"), nil, nil
	})

	sdk.AddTool(server, &sdk.Tool{
		Name:        "huge",
		Description: "Return more than anyone wants.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, _ emptyInput) (*sdk.CallToolResult, any, error) {
		return textResult(false, "HEAD"+strings.Repeat("x", 200_000)+"TAIL"), nil, nil
	})

	sdk.AddTool(server, &sdk.Tool{
		Name:        "slow",
		Description: "Never answer in time.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, _ emptyInput) (*sdk.CallToolResult, any, error) {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(30 * time.Second):
			return textResult(false, "finally"), nil, nil
		}
	})

	// Reports its own environment, so a test can prove what was and was not
	// handed to it.
	sdk.AddTool(server, &sdk.Tool{
		Name:        "read_env",
		Description: "Report an environment variable.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in nameInput) (*sdk.CallToolResult, any, error) {
		return textResult(false, os.Getenv(in.Name)), nil, nil
	})

	// Stops the process mid-call, so the client's handling of a server that
	// dies can be exercised rather than reasoned about.
	sdk.AddTool(server, &sdk.Tool{
		Name:        "die",
		Description: "Exit without answering.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, _ emptyInput) (*sdk.CallToolResult, any, error) {
		os.Exit(2)
		return nil, nil, nil
	})

	return server.Run(context.Background(), &sdk.StdioTransport{})
}

func textResult(isError bool, text string) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		IsError: isError,
		Content: []sdk.Content{&sdk.TextContent{Text: text}},
	}
}

func arguments(t *testing.T, values map[string]any) json.RawMessage {
	t.Helper()

	encoded, err := json.Marshal(values)
	if err != nil {
		t.Fatalf("encode arguments: %v", err)
	}
	return encoded
}

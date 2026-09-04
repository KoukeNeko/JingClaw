package builtin_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/tool/builtin"
)

type fakeDeferred []tool.Spec

func (f fakeDeferred) DeferredSpecs() []tool.Spec { return f }

var deferredPair = fakeDeferred{
	{Name: "mcp_ssh_upload", Description: "Copy a file to a remote host over SSH.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
	{Name: "mcp_zhtw_zhtw", Description: "Lint zh-TW text.",
		InputSchema: json.RawMessage(`{"type":"object"}`)},
}

func TestSearchFindsByNameOrByWhatItDoes(t *testing.T) {
	search := &builtin.ToolSearch{Deferred: deferredPair}

	byName, _ := search.Execute(context.Background(), mkCall(t, map[string]string{"query": "ZHTW"}))
	if byName.IsError || !strings.Contains(byName.Content, "mcp_zhtw_zhtw") || strings.Contains(byName.Content, "mcp_ssh_upload") {
		t.Errorf("searching by name (case-insensitively) returned: %s", byName.Content)
	}

	byPurpose, _ := search.Execute(context.Background(), mkCall(t, map[string]string{"query": "remote host"}))
	if byPurpose.IsError || !strings.Contains(byPurpose.Content, "mcp_ssh_upload") {
		t.Errorf("searching by description returned: %s", byPurpose.Content)
	}
	// Names and descriptions only: the schema waits for tool_load.
	if strings.Contains(byPurpose.Content, `"properties"`) {
		t.Error("a search result carried a schema")
	}

	none, _ := search.Execute(context.Background(), mkCall(t, map[string]string{"query": "kubernetes"}))
	if none.IsError || !strings.Contains(none.Content, "No deferred tool matches") {
		t.Errorf("a query matching nothing was not said plainly: %+v", none)
	}

	empty, _ := search.Execute(context.Background(), mkCall(t, map[string]string{"query": "  "}))
	if !empty.IsError {
		t.Error("an empty query was accepted")
	}
}

// Loading hands over the schema, and says so in a summary the runtime keys
// off. A name that is not deferred is refused as an error — the runtime
// declares a tool only on a load that succeeded, so this must not pass for
// one.
func TestLoadHandsOverTheSchemaAndRefusesTheUnknown(t *testing.T) {
	load := &builtin.ToolLoad{Deferred: deferredPair}

	loaded, _ := load.Execute(context.Background(), mkCall(t, map[string]string{"name": "mcp_ssh_upload"}))
	if loaded.IsError {
		t.Fatalf("loading a deferred tool was refused: %s", loaded.Content)
	}
	if loaded.Summary != "loaded mcp_ssh_upload" {
		t.Errorf("the summary is %q", loaded.Summary)
	}
	if !strings.Contains(loaded.Content, `"path"`) {
		t.Errorf("the schema did not come back: %s", loaded.Content)
	}

	unknown, _ := load.Execute(context.Background(), mkCall(t, map[string]string{"name": "read_file"}))
	if !unknown.IsError {
		t.Error("a name that is not deferred was reported as loaded")
	}

	blank, _ := load.Execute(context.Background(), mkCall(t, map[string]string{"name": ""}))
	if !blank.IsError {
		t.Error("an empty name was accepted")
	}
}

// Both are internal: they read a registry and touch nothing, so no profile
// stops for them — the tool they lead to is what gets gated.
func TestSearchAndLoadAreInternal(t *testing.T) {
	if got := (&builtin.ToolSearch{}).Spec().Level; got != tool.LevelInternal {
		t.Errorf("tool_search is at %s", got)
	}
	if got := (&builtin.ToolLoad{}).Spec().Level; got != tool.LevelInternal {
		t.Errorf("tool_load is at %s", got)
	}
}

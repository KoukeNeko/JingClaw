package prompt

import (
	"strings"
	"testing"
)

// A deferred server costs the prompt one line, and the line says how to reach
// its tools. Without the line the model has no way to know the server exists;
// with a schema per tool the deferral would have saved nothing.
func TestADeferredServerIsOneLineInThePrompt(t *testing.T) {
	env := Environment{
		WorkspaceRoot: "/w", OS: "linux", Arch: "amd64",
		ToolNames: []string{"read_file", "tool_load", "tool_search"},
		DeferredServers: []DeferredServer{
			{Name: "ssh", Tools: 37, Level: "execute"},
			{Name: "zhtw", Tools: 1, Level: "workspace_read"},
		},
	}
	rendered := environment(env)

	for _, want := range []string{
		"ssh (37 tools, execute)",
		"zhtw (1 tool, workspace_read)",
		"tool_search",
		"tool_load",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the prompt does not say %q:\n%s", want, rendered)
		}
	}
	if strings.Count(rendered, "ssh") != 1 {
		t.Errorf("the server is named more than once — that is a catalogue, not a line:\n%s", rendered)
	}
}

// No deferred servers, nothing said: a deployment with none is byte for byte
// what it was.
func TestNoDeferredServersAddsNothing(t *testing.T) {
	env := Environment{WorkspaceRoot: "/w", OS: "linux", Arch: "amd64", ToolNames: []string{"read_file"}}
	if rendered := environment(env); strings.Contains(rendered, "Tool servers") {
		t.Errorf("a prompt with no deferred servers mentions them:\n%s", rendered)
	}
}

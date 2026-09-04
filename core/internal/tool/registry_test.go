package tool_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/tool"
)

type plainTool struct {
	name     string
	deferred bool
}

func (t plainTool) Spec() tool.Spec {
	return tool.Spec{Name: t.name, Description: "a " + t.name, Deferred: t.deferred,
		InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (t plainTool) Execute(context.Context, tool.Call) (tool.Result, error) {
	return tool.Result{Content: "ran " + t.name}, nil
}

// A deferred tool is registered and not declared: the model is not shown it,
// and it still runs when called by name. That gap is the whole feature — a
// server with forty tools costs the prompt one line rather than forty schemas,
// and any of the forty is a load away.
func TestADeferredToolIsRegisteredButNotDeclared(t *testing.T) {
	registry := tool.NewRegistry()
	registry.MustRegister(plainTool{name: "shown"}, plainTool{name: "hidden", deferred: true})

	shown := names(registry.Specs())
	if len(shown) != 1 || shown[0] != "shown" {
		t.Errorf("Specs declares %v; a deferred tool must not be in it", shown)
	}

	deferred := names(registry.DeferredSpecs())
	if len(deferred) != 1 || deferred[0] != "hidden" {
		t.Errorf("DeferredSpecs lists %v, want the one deferred tool", deferred)
	}

	// Not declared is not the same as not there.
	if _, known := registry.Lookup("hidden"); !known {
		t.Error("a deferred tool cannot be looked up")
	}
	result := registry.Execute(context.Background(), tool.Call{Name: "hidden", Arguments: json.RawMessage(`{}`)})
	if result.IsError || result.Content != "ran hidden" {
		t.Errorf("a deferred tool did not run when called: %+v", result)
	}
}

func names(specs []tool.Spec) []string {
	out := make([]string, 0, len(specs))
	for _, spec := range specs {
		out = append(out, spec.Name)
	}
	return out
}

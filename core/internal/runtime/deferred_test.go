package runtime_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/tool/builtin"
)

// deferredEcho stands in for a tool on a deferred server: registered, callable,
// and not declared until loaded.
type deferredEcho struct{}

func (deferredEcho) Spec() tool.Spec {
	return tool.Spec{
		Name: "mcp_helper_echo", Description: "Echo text back.", Deferred: true,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
		Level:       tool.LevelInternal,
	}
}

func (deferredEcho) Execute(_ context.Context, call tool.Call) (tool.Result, error) {
	var args struct{ Text string }
	_ = json.Unmarshal(call.Arguments, &args)
	return tool.Result{Content: "echo: " + args.Text}, nil
}

func declares(req provider.Request, name string) bool {
	for _, declared := range req.Tools {
		if declared.Name == name {
			return true
		}
	}
	return false
}

// A deferred tool costs nothing until it is loaded, and then it is there:
// absent from the first request, declared on every request after the load,
// and it runs when called. Read from the log, not memory, so the same session
// resumed in another process reaches the same set.
func TestADeferredToolIsDeclaredOnlyOnceLoaded(t *testing.T) {
	rt, store, scripted, registry := newToolHarness(t, [][]provider.Event{
		{toolCall("call_1", "tool_load", map[string]any{"name": "mcp_helper_echo"})},
		{toolCall("call_2", "mcp_helper_echo", map[string]any{"text": "hi"})},
		{provider.TextDelta{Text: "done"}, provider.Completed{StopReason: domain.StopEndTurn}},
	})
	registry.MustRegister(
		&builtin.ToolSearch{Deferred: registry},
		&builtin.ToolLoad{Deferred: registry},
		deferredEcho{},
	)

	session, err := rt.CreateSession(context.Background(), "deferred")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	runTurn(t, rt, session.ID, "echo something")

	if len(scripted.requests) != 3 {
		t.Fatalf("the model was asked %d times, want 3", len(scripted.requests))
	}
	if declares(scripted.requests[0], "mcp_helper_echo") {
		t.Error("the deferred tool was declared before anybody loaded it")
	}
	if !declares(scripted.requests[0], "tool_load") {
		t.Error("tool_load itself is not declared, so nothing could ever be loaded")
	}
	if !declares(scripted.requests[1], "mcp_helper_echo") {
		t.Error("the loaded tool was not declared on the request after the load")
	}
	if !declares(scripted.requests[2], "mcp_helper_echo") {
		t.Error("the loaded tool was forgotten again on the next request")
	}

	// And calling it worked: the load was not for show.
	events, err := store.ListAfter(context.Background(), session.ID, 0, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	ran := false
	for _, event := range events {
		if completed, ok := event.Payload.(domain.ToolCallCompleted); ok &&
			completed.Name == "mcp_helper_echo" && !completed.IsError && completed.Content == "echo: hi" {
			ran = true
		}
	}
	if !ran {
		t.Error("the loaded tool did not run when called")
	}
}

// A load that failed loads nothing. tool_load refuses a name that is not
// deferred, and the runtime must key off that refusal — otherwise asking for
// a tool that does not exist would declare whatever the model typed.
func TestAFailedLoadDeclaresNothing(t *testing.T) {
	rt, _, scripted, registry := newToolHarness(t, [][]provider.Event{
		{toolCall("call_1", "tool_load", map[string]any{"name": "mcp_helper_echo_typo"})},
		{provider.TextDelta{Text: "hm"}, provider.Completed{StopReason: domain.StopEndTurn}},
	})
	registry.MustRegister(&builtin.ToolLoad{Deferred: registry}, deferredEcho{})

	session, err := rt.CreateSession(context.Background(), "typo")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	runTurn(t, rt, session.ID, "try")

	if len(scripted.requests) < 2 {
		t.Fatalf("the model was asked %d times", len(scripted.requests))
	}
	if declares(scripted.requests[1], "mcp_helper_echo") {
		t.Error("a load that was refused still declared the deferred tool")
	}
}

// A load the registry rejected loads nothing. The name is in the request and
// parses fine; what says it did not happen is the completion's error flag,
// and that is the flag the runtime has to key off — otherwise a model that
// fumbled the arguments would have declared the tool anyway.
func TestALoadRejectedByValidationDeclaresNothing(t *testing.T) {
	rt, _, scripted, registry := newToolHarness(t, [][]provider.Event{
		// additionalProperties is false on tool_load, so this is refused
		// before ToolLoad runs — with the name still readable in the log.
		{toolCall("call_1", "tool_load", map[string]any{"name": "mcp_helper_echo", "extra": 1})},
		{provider.TextDelta{Text: "hm"}, provider.Completed{StopReason: domain.StopEndTurn}},
	})
	registry.MustRegister(&builtin.ToolLoad{Deferred: registry}, deferredEcho{})

	session, err := rt.CreateSession(context.Background(), "fumbled")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	runTurn(t, rt, session.ID, "try")

	if len(scripted.requests) < 2 {
		t.Fatalf("the model was asked %d times", len(scripted.requests))
	}
	if declares(scripted.requests[1], "mcp_helper_echo") {
		t.Error("a load the registry rejected still declared the tool")
	}
}

// flippableEcho is deferred until somebody says otherwise — a server whose
// operator turned defer off between one daemon start and the next.
type flippableEcho struct{ deferred *bool }

func (f flippableEcho) Spec() tool.Spec {
	spec := deferredEcho{}.Spec()
	spec.Deferred = *f.deferred
	return spec
}

func (f flippableEcho) Execute(ctx context.Context, call tool.Call) (tool.Result, error) {
	return deferredEcho{}.Execute(ctx, call)
}

// A tool that was loaded while deferred and is no longer deferred is declared
// once, not twice. It is in the base now; declaring it again from the log
// would send the model the same name twice, which providers refuse.
func TestAToolNoLongerDeferredIsNotDeclaredTwice(t *testing.T) {
	deferred := true
	rt, _, scripted, registry := newToolHarness(t, [][]provider.Event{
		{toolCall("call_1", "tool_load", map[string]any{"name": "mcp_helper_echo"})},
		{provider.TextDelta{Text: "ok"}, provider.Completed{StopReason: domain.StopEndTurn}},
		{provider.TextDelta{Text: "and again"}, provider.Completed{StopReason: domain.StopEndTurn}},
	})
	registry.MustRegister(&builtin.ToolLoad{Deferred: registry}, flippableEcho{deferred: &deferred})

	session, err := rt.CreateSession(context.Background(), "flipped")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	runTurn(t, rt, session.ID, "load it")

	// The operator turns defer off; the next turn's base declares it.
	deferred = false
	runTurn(t, rt, session.ID, "use it")

	last := scripted.requests[len(scripted.requests)-1]
	count := 0
	for _, declared := range last.Tools {
		if declared.Name == "mcp_helper_echo" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("the tool is declared %d times, want exactly once", count)
	}
}

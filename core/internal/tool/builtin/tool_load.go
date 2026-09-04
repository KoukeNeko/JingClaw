package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
)

// Deferred is where the tools kept out of the prompt are read from.
//
// An interface so this package keeps not knowing about the registry; the
// registry satisfies it.
type Deferred interface {
	DeferredSpecs() []tool.Spec
}

// maxToolSearchResults bounds what a search returns. Past this the model is
// reading a catalogue, which is what deferring was meant to spare it.
const maxToolSearchResults = 20

// ToolSearch finds a deferred tool by what it is called or what it is for.
//
// Internal: it reads a registry and changes nothing. What it returns is the
// same text the model would have been shown had the server not been deferred,
// so it is not foreign in any sense the undeferred tool was not.
type ToolSearch struct {
	Deferred Deferred
}

func (t *ToolSearch) Spec() tool.Spec {
	return tool.Spec{
		Name: "tool_search",
		Description: "Find a tool that is not listed above. Some tool servers keep their tools " +
			"out of the list until asked for, and this searches those by name or by what they " +
			"do. It returns names and descriptions only; call tool_load with a name before " +
			"using it.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "Words from the tool's name or from what it does."
    }
  },
  "required": ["query"],
  "additionalProperties": false
}`),
		Level: tool.LevelInternal,
		Capabilities: tool.Capabilities{
			Provenance:   domain.ProvenanceLocalUnknown,
			Idempotent:   true,
			ParallelSafe: true,
		},
	}
}

type toolSearchArgs struct {
	Query string `json:"query"`
}

func (t *ToolSearch) Execute(_ context.Context, call tool.Call) (tool.Result, error) {
	var args toolSearchArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return tool.Result{}, fmt.Errorf("tool_search: unusable arguments: %w", err)
	}

	query := strings.ToLower(strings.TrimSpace(args.Query))
	if query == "" {
		return refusal("tool_search needs a word or two to look for."), nil
	}

	var found []tool.Spec
	for _, spec := range t.Deferred.DeferredSpecs() {
		if strings.Contains(strings.ToLower(spec.Name), query) ||
			strings.Contains(strings.ToLower(spec.Description), query) {
			found = append(found, spec)
		}
	}
	sort.Slice(found, func(a, b int) bool { return found[a].Name < found[b].Name })

	if len(found) == 0 {
		return tool.Result{
			Summary: "no tool matches " + query,
			Content: fmt.Sprintf("No deferred tool matches %q. The tools listed in this conversation are all there is otherwise.", args.Query),
		}, nil
	}

	truncated := false
	if len(found) > maxToolSearchResults {
		found = found[:maxToolSearchResults]
		truncated = true
	}

	var out strings.Builder
	fmt.Fprintf(&out, "Tools matching %q. Call tool_load with a name to use one.\n", args.Query)
	for _, spec := range found {
		fmt.Fprintf(&out, "\n%s — %s", spec.Name, spec.Description)
	}
	if truncated {
		fmt.Fprintf(&out, "\n\n(more than %d matched; narrow the query)", maxToolSearchResults)
	}

	return tool.Result{
		Summary: fmt.Sprintf("%d tools match %s", len(found), query),
		Content: out.String(),
	}, nil
}

// ToolLoad hands the model one deferred tool's full description and schema,
// and from this turn on that tool is declared.
//
// The declaring is not done here. The runtime reads this call out of the log
// — a tool_load that completed without error — and declares the tool it
// named on every later request in the session. Recorded that way rather than
// held in memory, like everything else, so a daemon restarted mid-session
// reaches the same set.
type ToolLoad struct {
	Deferred Deferred
}

func (t *ToolLoad) Spec() tool.Spec {
	return tool.Spec{
		Name: "tool_load",
		Description: "Make a tool found with tool_search available. Give its exact name; from " +
			"then on it can be called like any listed tool. Loading it grants nothing: it is " +
			"gated and approved the way it would have been had it been listed from the start.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "description": "The tool's name, exactly as tool_search reported it."
    }
  },
  "required": ["name"],
  "additionalProperties": false
}`),
		Level: tool.LevelInternal,
		Capabilities: tool.Capabilities{
			Provenance:   domain.ProvenanceLocalUnknown,
			Idempotent:   true,
			ParallelSafe: true,
		},
	}
}

// ToolLoadArgs is the shape of a tool_load call, exported so the runtime can
// read the name back out of the log.
type ToolLoadArgs struct {
	Name string `json:"name"`
}

func (t *ToolLoad) Execute(_ context.Context, call tool.Call) (tool.Result, error) {
	var args ToolLoadArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return tool.Result{}, fmt.Errorf("tool_load: unusable arguments: %w", err)
	}

	name := strings.TrimSpace(args.Name)
	if name == "" {
		return refusal("tool_load needs the name of a tool."), nil
	}

	for _, spec := range t.Deferred.DeferredSpecs() {
		if spec.Name != name {
			continue
		}
		return tool.Result{
			Summary: "loaded " + spec.Name,
			Content: fmt.Sprintf(
				"%s is now available and can be called like any other tool.\n\n%s\n\nArguments:\n%s",
				spec.Name, spec.Description, string(spec.InputSchema)),
		}, nil
	}

	// Refused as an error, and that matters: the runtime declares a tool only
	// on a tool_load that succeeded, so a name that is not deferred must not
	// look like one that was loaded.
	return refusal(fmt.Sprintf(
		"No deferred tool is named %q. Use tool_search to find the exact name; a tool already "+
			"listed in this conversation needs no loading.", name)), nil
}

// Package permission decides whether a tool call may proceed.
//
// The decision is made here, outside the model, and it reads the arguments the
// tool will actually receive rather than the model's description of them. No
// amount of persuasion in a prompt, a file, or a tool result can change the
// outcome: text is data, and data does not grant capabilities.
package permission

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
)

// Decision is the outcome of evaluating one call.
type Decision string

const (
	// Allow runs the tool without involving a human.
	Allow Decision = "allow"

	// Ask suspends the run until someone decides.
	Ask Decision = "ask"

	// Deny refuses outright. The model is told, so it can try something else.
	Deny Decision = "deny"
)

// Request is everything the engine needs to decide.
type Request struct {
	Spec tool.Spec
	Call tool.Call

	SessionID domain.SessionID
	RunID     domain.RunID

	// Origin matters: a turn typed by the operator at their own machine is not
	// the same as one arriving from a chat platform, even when the text is
	// identical.
	Origin domain.RunOrigin
}

// Outcome carries the decision and the material a human needs to judge it.
type Outcome struct {
	Decision Decision

	// Summary is one line naming what will happen, in concrete terms.
	Summary string

	// Effects spell out consequences a reader might not infer from the
	// arguments alone.
	Effects []string

	// Reason explains a Deny, or which rule produced an Ask.
	Reason string
}

// Profile is a named set of defaults.
//
// Profiles exist because the right answer depends on where a turn came from.
// The gateway plane will need a distinctly stricter one than a local operator
// typing into their own terminal, and encoding that as a profile keeps the
// difference explicit rather than scattered through conditionals.
type Profile struct {
	Name string

	// Defaults maps a tool's base level to a decision.
	Defaults map[tool.Level]Decision

	// DenyTools always refuses these, whatever their level.
	DenyTools map[string]bool
}

// LocalProfile is for turns from a control-plane client on this machine.
//
// Reads run unattended because an agent that stops to ask before every file it
// looks at is unusable. Anything that modifies the workspace asks, because a
// wrong edit is expensive and a human glance is cheap.
func LocalProfile() Profile {
	return Profile{
		Name: "local",
		Defaults: map[tool.Level]Decision{
			tool.LevelInternal:       Allow,
			tool.LevelWorkspaceRead:  Allow,
			tool.LevelWorkspaceWrite: Ask,
			tool.LevelExecute:        Ask,
			tool.LevelHighImpact:     Deny,
		},
	}
}

// Engine evaluates calls against a profile plus whatever a human has already
// agreed to in this session.
type Engine struct {
	profile Profile

	mu sync.RWMutex
	// granted records standing permissions from earlier decisions, keyed by
	// session and grant key.
	granted map[domain.SessionID]map[string]bool
}

func New(profile Profile) *Engine {
	return &Engine{
		profile: profile,
		granted: make(map[domain.SessionID]map[string]bool),
	}
}

func (e *Engine) Profile() string { return e.profile.Name }

// Evaluate decides one call.
func (e *Engine) Evaluate(_ context.Context, req Request) Outcome {
	summary, effects := describe(req)

	if e.profile.DenyTools[req.Spec.Name] {
		return Outcome{
			Decision: Deny,
			Summary:  summary,
			Effects:  effects,
			Reason:   fmt.Sprintf("%s is not permitted by the %s profile", req.Spec.Name, e.profile.Name),
		}
	}

	decision, ok := e.profile.Defaults[req.Spec.Level]
	if !ok {
		// An unclassified level is a gap in the policy, and a gap must fail
		// closed. Defaulting to Allow would mean every new tool ships
		// unguarded until someone remembers to add a rule.
		return Outcome{
			Decision: Deny,
			Summary:  summary,
			Effects:  effects,
			Reason: fmt.Sprintf("the %s profile has no rule for %s tools",
				e.profile.Name, req.Spec.Level),
		}
	}

	if decision == Ask && e.isGranted(req.SessionID, grantKey(req.Spec.Name)) {
		return Outcome{
			Decision: Allow,
			Summary:  summary,
			Effects:  effects,
			Reason:   fmt.Sprintf("%s was approved for this session", req.Spec.Name),
		}
	}

	return Outcome{
		Decision: decision,
		Summary:  summary,
		Effects:  effects,
		Reason:   fmt.Sprintf("%s tools are set to %s in the %s profile", req.Spec.Level, decision, e.profile.Name),
	}
}

// GrantForSession records a standing approval.
//
// The grant is per tool, never per invocation: remembering "yes" for a specific
// set of arguments would be useless, and remembering it for everything would
// hand over the whole session.
func (e *Engine) GrantForSession(session domain.SessionID, toolName string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	grants, ok := e.granted[session]
	if !ok {
		grants = make(map[string]bool)
		e.granted[session] = grants
	}
	grants[grantKey(toolName)] = true
}

func (e *Engine) isGranted(session domain.SessionID, key string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.granted[session][key]
}

func grantKey(toolName string) string { return "tool:" + toolName }

// describe renders the call for a human.
//
// "The agent wants to use a tool" tells a reviewer nothing. What they need is
// the tool, the target, and what will change — which means reading the actual
// arguments rather than trusting a description of them.
func describe(req Request) (summary string, effects []string) {
	arguments := decodeArguments(req.Call.Arguments)

	if path, ok := arguments["path"].(string); ok {
		summary = fmt.Sprintf("%s %s", req.Spec.Name, path)
	} else {
		summary = req.Spec.Name
	}

	capabilities := req.Spec.Capabilities
	if capabilities.WriteFS {
		if path, ok := arguments["path"].(string); ok {
			effects = append(effects, "Modifies "+path)
		} else {
			effects = append(effects, "Modifies files in the workspace")
		}
	}
	if capabilities.Execute {
		effects = append(effects, "Runs a program on this machine")
	}
	if capabilities.Network {
		effects = append(effects, "Sends a request to a remote host")
	}
	if capabilities.Secrets {
		effects = append(effects, "Reads stored credentials")
	}
	if capabilities.Destructive {
		effects = append(effects, "Cannot be undone by running this again")
	}

	if compact := compactArguments(arguments); compact != "" {
		summary += " " + compact
	}

	return summary, effects
}

func decodeArguments(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}

	var arguments map[string]any
	if err := json.Unmarshal(raw, &arguments); err != nil {
		return nil
	}
	return arguments
}

// compactArguments renders the arguments other than the path, bounded, so an
// approval prompt stays readable.
func compactArguments(arguments map[string]any) string {
	if len(arguments) == 0 {
		return ""
	}

	keys := make([]string, 0, len(arguments))
	for key := range arguments {
		if key != "path" {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return ""
	}

	// Sorted so the same call always reads the same way.
	sortStrings(keys)

	var parts []string
	for _, key := range keys {
		value := fmt.Sprintf("%v", arguments[key])
		value = strings.Join(strings.Fields(value), " ")
		if len(value) > 60 {
			value = value[:60] + "…"
		}
		parts = append(parts, key+"="+value)
	}

	return "(" + strings.Join(parts, ", ") + ")"
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j-1] > values[j]; j-- {
			values[j-1], values[j] = values[j], values[j-1]
		}
	}
}

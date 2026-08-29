package builtin

import (
	"context"
	"encoding/json"

	"github.com/KoukeNeko/JingClaw/core/internal/tool"
)

// AskUser stops the run to ask a person something.
//
// The only tool here whose Execute is never called. The runtime settles it by
// finding an answer somebody typed, which is why the declaration lives here —
// so the model is offered it alongside everything else — and the behaviour
// does not.
//
// It exists because the alternative is what a model does without it: write a
// paragraph ending in a question and stop. That reads as an answer, nothing
// downstream knows the run is waiting, and the reply arrives as a new turn
// with no link to what was asked.
type AskUser struct{}

func (t *AskUser) Spec() tool.Spec {
	return tool.Spec{
		Name: "ask_user",
		Description: "Ask the person a question and wait for their answer. " +
			"Use it when you need a decision only they can make — which of two approaches, " +
			"a branch name, whether something is worth doing — rather than guessing or " +
			"writing the question into your answer and stopping. " +
			"Offer options when you know them; ask for text when you do not. " +
			"The run pauses until somebody answers, so do not use it for anything you " +
			"could work out yourself.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "prompt": {
      "type": "string",
      "minLength": 1,
      "description": "What you want to know, in one sentence."
    },
    "kind": {
      "type": "string",
      "enum": ["choice", "text"],
      "description": "choice offers a fixed set; text asks them to type something. Inferred from whether you gave options if omitted."
    },
    "options": {
      "type": "array",
      "minItems": 2,
      "description": "What may be chosen. Only for a choice.",
      "items": {
        "type": "object",
        "properties": {
          "id": {"type": "string", "minLength": 1, "description": "Short and distinct; this is what comes back."},
          "label": {"type": "string", "minLength": 1, "description": "What the person reads."},
          "detail": {"type": "string", "description": "What they need to tell two options apart when the labels alone do not."}
        },
        "required": ["id", "label"],
        "additionalProperties": false
      }
    }
  },
  "required": ["prompt"],
  "additionalProperties": false
}`),
		// Nothing is read, written or executed. It stops and waits, which is
		// the opposite of a thing worth gating: asking a person to approve
		// being asked a question is one prompt too many.
		Level:        tool.LevelInternal,
		Capabilities: tool.Capabilities{Idempotent: true},
	}
}

// Execute is never reached.
//
// The runtime recognises this tool by name and settles the call from a stored
// answer instead. Written as a refusal rather than left to panic, so that
// wiring it up somewhere the runtime does not know about fails loudly at the
// first call rather than quietly returning nothing.
func (t *AskUser) Execute(_ context.Context, _ tool.Call) (tool.Result, error) {
	return tool.Result{}, tool.Errorf(tool.CodeInternal,
		"",
		"ask_user was run as an ordinary tool; only the runtime can settle it")
}

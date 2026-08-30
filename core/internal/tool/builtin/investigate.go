package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
)

// Delegator answers a bounded question in a context of its own.
//
// An interface rather than the runtime itself, so this package keeps not
// importing it — every other tool here works on a workspace or a store, and
// one that reached into the runtime would be the first able to start work
// from inside it.
type Delegator interface {
	Investigate(ctx context.Context, parent domain.RunID, question string) (string, error)
}

// Investigate hands a question to a search that has its own context.
//
// What delegation buys is that: the hundred tool results a search takes do
// not end up in the conversation the model has to read again. It is not a
// general assistant, and the description says so — a model that thinks it can
// hand over work will hand over work that needs the conversation, and get
// back an answer to a question nobody could ask in a paragraph.
type Investigate struct {
	Delegator Delegator
}

func (t *Investigate) Spec() tool.Spec {
	return tool.Spec{
		Name: "investigate",
		Description: "Hand one question about the workspace to a separate search, " +
			"and get back its answer without the looking filling this conversation.\n\n" +
			"Use it for a question whose answer is a conclusion rather than a file: " +
			"which functions do X, where is Y defined, does Z appear anywhere, what " +
			"calls into this package. If answering would mean opening several files to " +
			"find out something you could state in a sentence, this is the tool.\n\n" +
			"Do not use it to read a file you have already located — read_file is for " +
			"that — or for anything needing what was said earlier here. It starts from " +
			"nothing: it cannot see this conversation, so the question must stand alone " +
			"and name the paths to start from if you know them. It can only read, and " +
			"it cannot delegate again.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "question": {
      "type": "string",
      "description": "One question, complete on its own. Name the paths or symbols to start from if you know them."
    }
  },
  "required": ["question"],
  "additionalProperties": false
}`),

		// It reads, and everything it can reach reads. Nothing it does needs
		// a decision from anybody, which is the point of the tools it was
		// given rather than a promise made here.
		Level:        tool.LevelWorkspaceRead,
		Capabilities: tool.Capabilities{ReadFS: true},
	}
}

type investigateArgs struct {
	Question string `json:"question"`
}

func (t *Investigate) Execute(ctx context.Context, call tool.Call) (tool.Result, error) {
	var args investigateArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments, "", "%v", err)
	}

	question := strings.TrimSpace(args.Question)
	if question == "" {
		return refusal("investigate needs a question."), nil
	}
	if call.Context.RunID == "" {
		return tool.Result{}, tool.Errorf(tool.CodeInternal, "",
			"investigate was called outside a run")
	}

	answer, err := t.Delegator.Investigate(ctx, domain.RunID(call.Context.RunID), question)
	if err != nil {
		// The parent asked a question and did not get an answer, which is
		// something it can act on — try again differently, or look itself.
		// Failing the run over it would end a conversation because a search
		// went nowhere.
		return refusal(fmt.Sprintf("the search did not answer: %v", err)), nil
	}

	// Framed rather than handed over bare. What comes back is one model's
	// account of what it read, and read plainly it is indistinguishable from
	// something this run established itself — which is how a delegated search
	// turns into a way of laundering a claim into a conversation that never
	// saw the evidence.
	return tool.Result{
		Summary: summarise(question),
		Content: fmt.Sprintf(
			"Answer from a delegated search, which read the workspace and reported back. "+
				"It is a second-hand account, not something you have seen: check anything "+
				"you are about to act on. It was asked, exactly:\n\n%s\n\nIt answered:\n\n%s",
			question, answer),
	}, nil
}

// summarise is the question, short enough for a log line.
func summarise(question string) string {
	const most = 60
	if len(question) <= most {
		return "investigate " + question
	}
	return "investigate " + question[:most] + "…"
}

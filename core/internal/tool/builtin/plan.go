package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
)

// Planner is what applies a change to the plan.
//
// An interface rather than the runtime itself, so this package keeps not
// importing it: every other built-in here works on a workspace or a store, and
// a tool that reached into the runtime would be the first one able to start a
// run from inside one.
type Planner interface {
	UpdatePlan(
		ctx context.Context,
		session domain.SessionID,
		ops []domain.PlanOpRequest,
	) ([]domain.PlanItem, error)
}

// TodoUpdate keeps the plan for a session.
//
// The plan is agent state rather than something written into an answer. A
// model that describes its plan in prose has to find it again in its own
// output next turn, and a person watching has to read a paragraph to learn
// what is left.
type TodoUpdate struct {
	Planner Planner
}

func (t *TodoUpdate) Spec() tool.Spec {
	return tool.Spec{
		Name: "todo_update",
		Description: "Keep a short plan for this session: add steps, and mark them as you " +
			"start and finish them. The plan is shown back to you at the start of every turn, " +
			"so it is where multi-step work is remembered rather than in your own answers. " +
			"Send only the changes — adding a step does not disturb the others, and there is " +
			"no operation that rewrites the list. Worth using for work that takes several " +
			"steps; not worth it for a single question.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "operations": {
      "type": "array",
      "minItems": 1,
      "description": "The changes to make, applied in order.",
      "items": {
        "type": "object",
        "properties": {
          "op": {
            "type": "string",
            "enum": ["add", "set_status", "set_title"],
            "description": "add appends a step; set_status moves an existing one; set_title rewords one."
          },
          "id": {
            "type": "string",
            "description": "Which step, for set_status and set_title. Use the id shown in the plan."
          },
          "title": {"type": "string", "description": "The step's text, for add and set_title."},
          "status": {
            "type": "string",
            "enum": ["pending", "in_progress", "completed", "abandoned"],
            "description": "Where the step has got to. Use abandoned, with a note, for something you decided not to do — a step that is deleted reads as one that was finished."
          },
          "note": {"type": "string", "description": "Why. Usually only worth it when abandoning a step."}
        },
        "required": ["op"],
        "additionalProperties": false
      }
    }
  },
  "required": ["operations"],
  "additionalProperties": false
}`),
		// Nothing outside this session changes, nothing is executed, and
		// nothing is read that the model could not already see. Asking a
		// person to approve the agent writing down what it intends to do
		// would train them to approve without reading.
		Level:        tool.LevelInternal,
		Capabilities: tool.Capabilities{Idempotent: false},
	}
}

type todoUpdateArgs struct {
	Operations []struct {
		Op     string `json:"op"`
		ID     string `json:"id"`
		Title  string `json:"title"`
		Status string `json:"status"`
		Note   string `json:"note"`
	} `json:"operations"`
}

func (t *TodoUpdate) Execute(ctx context.Context, call tool.Call) (tool.Result, error) {
	var args todoUpdateArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments, "", "%v", err)
	}
	if len(args.Operations) == 0 {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments,
			"Send at least one operation.", "there is nothing to do")
	}
	if call.Context.SessionID == "" {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments,
			"", "a plan belongs to a session, and this call has none")
	}

	ops := make([]domain.PlanOpRequest, 0, len(args.Operations))
	for _, one := range args.Operations {
		ops = append(ops, domain.PlanOpRequest{
			Op:     one.Op,
			ID:     one.ID,
			Title:  one.Title,
			Status: domain.PlanStatus(one.Status),
			Note:   one.Note,
		})
	}

	items, err := t.Planner.UpdatePlan(ctx, domain.SessionID(call.Context.SessionID), ops)
	if err != nil {
		// The model gets told what went wrong and can correct it: a step id
		// that is not there is usually a model reading its own earlier output
		// instead of the plan it was shown.
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments,
			"The plan you were shown at the start of this turn has the ids that exist.",
			"%v", err)
	}

	return tool.Result{
		Content: renderPlanResult(items),
		Summary: summarisePlan(items),
	}, nil
}

func renderPlanResult(items []domain.PlanItem) string {
	if len(items) == 0 {
		return "The plan is empty."
	}

	var out strings.Builder
	out.WriteString("The plan is now:\n")
	for _, item := range items {
		fmt.Fprintf(&out, "- [%s] %s (%s)\n", item.Status.Mark(), item.Title, item.ID)
		if item.Note != "" {
			fmt.Fprintf(&out, "      %s\n", item.Note)
		}
	}
	return out.String()
}

// summarisePlan is the line a channel or a timeline shows.
//
// Counts rather than the list. This is what appears every time a step moves,
// and a channel showing the whole plan on each change would bury the answer
// it was supposed to be helping somebody read.
func summarisePlan(items []domain.PlanItem) string {
	var done, doing, left int
	for _, item := range items {
		switch item.Status {
		case domain.PlanCompleted, domain.PlanAbandoned:
			done++
		case domain.PlanInProgress:
			doing++
		default:
			left++
		}
	}

	summary := fmt.Sprintf("%d of %d done", done, len(items))
	if doing > 0 {
		summary += fmt.Sprintf(", %d in progress", doing)
	}
	if left > 0 {
		summary += fmt.Sprintf(", %d to go", left)
	}
	return summary
}

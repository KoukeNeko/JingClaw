package viewfixture

import "encoding/json"

// Reduce applies one event to a state.
//
// The reference every client is checked against. Written here rather than
// derived from the runtime's own assembly on purpose: this is the behaviour
// three languages have to share, and a reference that is a wrapper around one
// implementation cannot catch that implementation being wrong.
func Reduce(state State, event Event) State {
	state.HeadSeq = event.Seq

	switch event.Kind {
	case "user.message":
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(event.Body, &payload)
		state.Messages = append(state.Messages, Message{Role: RoleUser, Text: payload.Text})

	case "assistant.delta":
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(event.Body, &payload)
		// Joined onto the open assistant turn. A delta that starts a new
		// message is one word on a line of its own.
		index := openAssistant(state.Messages)
		if index < 0 {
			state.Messages = append(state.Messages, Message{Role: RoleAssistant})
			index = len(state.Messages) - 1
		}
		state.Messages[index].Text += payload.Text

	case "assistant.reasoning":
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(event.Body, &payload)
		// Onto the same turn as the answer, in its own field. A model thinks
		// before it replies, so the working-out reaches a turn that has no
		// text yet and must not be mistaken for the start of one.
		index := openAssistant(state.Messages)
		if index < 0 {
			state.Messages = append(state.Messages, Message{Role: RoleAssistant})
			index = len(state.Messages) - 1
		}
		state.Messages[index].Reasoning += payload.Text

	case "tool.requested":
		var payload struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(event.Body, &payload)
		// Attached to the turn that asked, which is the assistant turn being
		// written — creating one if the model asked for a tool before saying
		// anything.
		index := openAssistant(state.Messages)
		if index < 0 {
			state.Messages = append(state.Messages, Message{Role: RoleAssistant})
			index = len(state.Messages) - 1
		}
		state.Messages[index].ToolCalls = append(
			state.Messages[index].ToolCalls, ToolCall{Name: payload.Name})

	case "tool.completed":
		var payload struct {
			Name     string `json:"name"`
			IsError  bool   `json:"is_error"`
			Artifact *struct {
				ID string `json:"id"`
			} `json:"artifact"`
		}
		_ = json.Unmarshal(event.Body, &payload)
		artifact := ""
		if payload.Artifact != nil {
			artifact = payload.Artifact.ID
		}
		markCompleted(state.Messages, payload.Name, payload.IsError, artifact)

	case "approval.requested":
		var payload struct {
			ApprovalID string `json:"approval_id"`
		}
		_ = json.Unmarshal(event.Body, &payload)
		state.PendingApprovals = append(state.PendingApprovals, payload.ApprovalID)

	case "approval.resolved":
		var payload struct {
			ApprovalID string `json:"approval_id"`
		}
		_ = json.Unmarshal(event.Body, &payload)
		state.PendingApprovals = without(state.PendingApprovals, payload.ApprovalID)

	case "conversation.compacted":
		// Everything before the fold is a summary now, so what a client draws
		// is the notice and whatever follows.
		state.Messages = []Message{{Role: RoleAssistant, Text: foldNotice}}

	case "plan.changed":
		// The whole plan, replacing whatever was there. The event carries the
		// list rather than the change, so a client that joined late reads one
		// entry and knows where things stand.
		var payload struct {
			Items []struct {
				ID     string `json:"id"`
				Title  string `json:"title"`
				Status string `json:"status"`
			} `json:"items"`
		}
		_ = json.Unmarshal(event.Body, &payload)
		state.Plan = nil
		for _, item := range payload.Items {
			state.Plan = append(state.Plan, PlanStep{
				ID: item.ID, Title: item.Title, Status: item.Status,
			})
		}

	case "run.state_changed":
		var payload struct {
			RunID  string `json:"run_id"`
			Status string `json:"status"`
		}
		_ = json.Unmarshal(event.Body, &payload)
		switch payload.Status {
		case "completed", "failed", "cancelled":
			if state.ActiveRun == payload.RunID {
				state.ActiveRun = ""
			}
		default:
			state.ActiveRun = payload.RunID
		}
	}

	return state
}

// ReduceAll folds a whole sequence, which is what a client does on attach.
func ReduceAll(events []Event) State {
	state := State{}
	for _, event := range events {
		state = Reduce(state, event)
	}
	return state
}

// openAssistant is the assistant turn currently being written, or -1.
//
// The last message when it is the assistant's. A user turn closes it: what
// follows belongs to the answer to that turn and not to the one before it.
func openAssistant(messages []Message) int {
	if len(messages) == 0 {
		return -1
	}
	last := len(messages) - 1
	if messages[last].Role != RoleAssistant {
		return -1
	}
	return last
}

func markCompleted(messages []Message, name string, failed bool, artifact string) {
	// The last call of that name that has not finished. Names repeat within a
	// turn, and marking the first would report the wrong one as done.
	for i := len(messages) - 1; i >= 0; i-- {
		calls := messages[i].ToolCalls
		for j := len(calls) - 1; j >= 0; j-- {
			if calls[j].Name == name && !calls[j].Completed {
				calls[j].Completed = true
				calls[j].IsError = failed
				calls[j].Artifact = artifact
				return
			}
		}
	}
}

func without(values []string, unwanted string) []string {
	kept := values[:0]
	for _, value := range values {
		if value != unwanted {
			kept = append(kept, value)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

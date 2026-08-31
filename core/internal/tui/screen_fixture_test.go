package tui_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime/viewfixture"
	"github.com/KoukeNeko/JingClaw/core/internal/tui"
)

// The panel draws what every client agreed to draw.
//
// Read from the written file rather than from viewfixture.Cases(), because
// the file is what a client in another language would read and the two can
// come apart. A panel checked against the Go builder would agree with a set
// of cases nobody else has.
func TestThePanelAgreesWithTheRecordedCases(t *testing.T) {
	for _, agreed := range recordedCases(t) {
		t.Run(agreed.Name, func(t *testing.T) {
			drawn := tui.FoldAll(asDomain(t, agreed.Events))

			got := asFixtureState(drawn)
			if !reflect.DeepEqual(got, agreed.Expected) {
				gotJSON, _ := json.MarshalIndent(got, "", "  ")
				wantJSON, _ := json.MarshalIndent(agreed.Expected, "", "  ")
				t.Errorf("%s\n\ngot:\n%s\n\nwant:\n%s",
					agreed.Why, gotJSON, wantJSON)
			}
		})
	}
}

// Every recorded case reaches the panel.
//
// Without this the check above passes on an empty file, which is exactly what
// a bad path or a renamed field would produce.
func TestEveryRecordedCaseIsActuallyChecked(t *testing.T) {
	recorded := recordedCases(t)
	if len(recorded) != len(viewfixture.Cases()) {
		t.Errorf("read %d recorded cases but the reference has %d",
			len(recorded), len(viewfixture.Cases()))
	}
	for _, agreed := range recorded {
		if len(agreed.Events) == 0 {
			t.Errorf("%q arrived with no events, so it proves nothing", agreed.Name)
		}
	}
}

// recordedCases reads the file the fixtures live in.
func recordedCases(t *testing.T) []viewfixture.Case {
	t.Helper()

	path := filepath.Join("..", "..", "..", "fixtures", "session-view.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the recorded cases are unreadable: %v", err)
	}

	var file struct {
		Cases []viewfixture.Case `json:"cases"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("the recorded cases are unparseable: %v", err)
	}
	if len(file.Cases) == 0 {
		t.Fatal("the recorded cases are empty, so nothing below checks anything")
	}
	return file.Cases
}

// asDomain turns wire-format fixtures into the events the panel receives.
//
// The panel folds domain events, because that is what the daemon sends it
// once the control layer has decoded them. The fixtures are wire format
// because that is what a client in another language can read. This is the
// join, and it lives in the test: shipping it would mean the panel had a
// second way in that nothing uses.
func asDomain(t *testing.T, events []viewfixture.Event) []domain.Event {
	t.Helper()

	decoded := make([]domain.Event, 0, len(events))
	for _, event := range events {
		var wire struct {
			MessageID  string `json:"message_id"`
			Text       string `json:"text"`
			CallID     string `json:"call_id"`
			Name       string `json:"name"`
			IsError    bool   `json:"is_error"`
			ApprovalID string `json:"approval_id"`
			RunID      string `json:"run_id"`
			Status     string `json:"status"`
			ThroughSeq uint64 `json:"through_seq"`
			Artifact   *struct {
				ID   string `json:"id"`
				Size int64  `json:"size"`
			} `json:"artifact"`
			Items []struct {
				ID     string `json:"id"`
				Title  string `json:"title"`
				Status string `json:"status"`
			} `json:"items"`
		}
		if err := json.Unmarshal(event.Body, &wire); err != nil {
			t.Fatalf("a %s fixture body will not decode: %v", event.Kind, err)
		}

		out := domain.Event{Seq: domain.Seq(event.Seq)}
		switch event.Kind {
		case "user.message":
			out.Kind = domain.EventUserMessageAdded
			out.Payload = domain.UserMessageAdded{
				MessageID: domain.MessageID(wire.MessageID), Text: wire.Text,
			}

		case "assistant.delta":
			out.Kind = domain.EventAssistantTextDelta
			out.Payload = domain.AssistantTextDelta{
				MessageID: domain.MessageID(wire.MessageID), Text: wire.Text,
			}

		case "assistant.reasoning":
			out.Kind = domain.EventAssistantReasoningDelta
			out.Payload = domain.AssistantReasoningDelta{
				MessageID: domain.MessageID(wire.MessageID), Text: wire.Text,
			}

		case "assistant.completed":
			out.Kind = domain.EventAssistantMessageCompleted
			out.Payload = domain.AssistantMessageCompleted{
				MessageID: domain.MessageID(wire.MessageID),
			}

		case "tool.requested":
			out.Kind = domain.EventToolCallRequested
			out.Payload = domain.ToolCallRequested{
				CallID: domain.ToolCallID(wire.CallID), Name: wire.Name,
			}

		case "tool.completed":
			done := domain.ToolCallCompleted{
				CallID: domain.ToolCallID(wire.CallID),
				Name:   wire.Name, IsError: wire.IsError,
			}
			if wire.Artifact != nil {
				done.Artifact = &domain.Artifact{
					ID: wire.Artifact.ID, Size: wire.Artifact.Size,
				}
			}
			out.Kind = domain.EventToolCallCompleted
			out.Payload = done

		case "approval.requested":
			out.Kind = domain.EventApprovalRequested
			out.Payload = domain.ApprovalRequested{
				ApprovalID: domain.ApprovalID(wire.ApprovalID),
			}

		case "approval.resolved":
			out.Kind = domain.EventApprovalResolved
			out.Payload = domain.ApprovalResolved{
				ApprovalID: domain.ApprovalID(wire.ApprovalID),
			}

		case "conversation.compacted":
			out.Kind = domain.EventConversationCompacted
			out.Payload = domain.ConversationCompacted{
				ThroughSeq: domain.Seq(wire.ThroughSeq),
			}

		case "plan.changed":
			changed := domain.PlanChanged{}
			for _, item := range wire.Items {
				changed.Items = append(changed.Items, domain.PlanItem{
					ID: item.ID, Title: item.Title,
					Status: domain.PlanStatus(item.Status),
				})
			}
			out.Kind = domain.EventPlanChanged
			out.Payload = changed

		case "run.state_changed":
			out.Kind = domain.EventRunStateChanged
			out.RunID = domain.RunID(wire.RunID)
			out.Payload = domain.RunStateChanged{
				Status: domain.RunStatus(wire.Status),
			}

		default:
			// Not skipped. A fixture kind this bridge does not know is a case
			// silently not being checked, which is the failure the recorded
			// cases exist to prevent.
			t.Fatalf("no idea how to deliver a %q to the panel", event.Kind)
		}

		decoded = append(decoded, out)
	}
	return decoded
}

// asFixtureState says what the panel drew in the shape the cases are written
// in, so a disagreement reads as a difference of screens.
func asFixtureState(screen tui.Screen) viewfixture.State {
	state := viewfixture.State{
		ActiveRun: string(screen.ActiveRun),
		HeadSeq:   uint64(screen.HeadSeq),
	}
	for _, request := range screen.Waiting {
		state.PendingApprovals = append(state.PendingApprovals, string(request.ID))
	}
	for _, step := range screen.Plan {
		state.Plan = append(state.Plan, viewfixture.PlanStep{
			ID: step.ID, Title: step.Title, Status: string(step.Status),
		})
	}
	for _, message := range screen.Messages {
		drawn := viewfixture.Message{
			Role:      viewfixture.RoleOf(message.Role),
			Text:      message.Text,
			Reasoning: message.Reasoning,
		}
		for _, call := range message.ToolCalls {
			drawn.ToolCalls = append(drawn.ToolCalls, viewfixture.ToolCall{
				ID: call.ID, Name: call.Name, Completed: call.Completed,
				IsError: call.IsError, Artifact: call.Artifact,
			})
		}
		state.Messages = append(state.Messages, drawn)
	}
	if state.Messages == nil {
		state.Messages = []viewfixture.Message{}
	}
	return state
}

var _ = fmt.Sprintf

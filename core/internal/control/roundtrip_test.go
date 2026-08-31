package control

import (
	"reflect"
	"testing"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/domain/domaintest"
)

// Every payload the log can hold survives being sent and read back.
//
// The list is the one storage checks itself against, so a kind cannot be
// added without this failing too — which is the point. A mapping written in
// one direction and forgotten in the other produces an event that arrives
// with its fields empty, and nothing says so.
func TestEveryPayloadSurvivesTheWire(t *testing.T) {
	for kind, payload := range domaintest.Payloads() {
		original := domain.Event{
			ID:        "evt_1",
			SessionID: "ses_1",
			RunID:     "run_1",
			Seq:       7,
			GlobalSeq: 91827,
			Kind:      kind,
			Payload:   payload,
		}

		sent, err := eventToProto(original)
		if err != nil {
			t.Errorf("%s could not be sent: %v", kind, err)
			continue
		}

		back, err := EventFromProto(sent)
		if err != nil {
			t.Errorf("%s could not be read back: %v", kind, err)
			continue
		}

		if back.Kind != kind {
			t.Errorf("%s came back as %s", kind, back.Kind)
		}
		if back.GlobalSeq != original.GlobalSeq {
			t.Errorf("%s lost its position in the log: %d", kind, back.GlobalSeq)
		}
		if want := onTheWire(original.Payload); !reflect.DeepEqual(back.Payload, want) {
			t.Errorf("%s changed on the way:\n sent %#v\n back %#v", kind, want, back.Payload)
		}
	}
}

// onTheWire drops what is deliberately not sent to clients.
//
// One field: a tool call's ProviderMetadata, which is opaque continuity state
// belonging to whichever model produced the call. It is kept in the log
// because the conversation is rebuilt from there, and it is not on the wire
// because nothing outside the provider adapter interprets it — a client
// receiving it could only pass it back or leak it.
//
// Written here rather than left as an exception in the comparison, so that
// putting the field on the wire later makes this the place that says so.
func onTheWire(payload domain.EventPayload) domain.EventPayload {
	if asked, ok := payload.(domain.ToolCallRequested); ok {
		asked.ProviderMetadata = ""
		return asked
	}
	return payload
}

// The enums are the easiest thing to get wrong, because a missed case gives
// the zero value rather than an error.
func TestEveryValueSurvivesTheWire(t *testing.T) {
	t.Run("trust", func(t *testing.T) {
		for _, value := range []domain.TrustLevel{
			domain.TrustUntrusted, domain.TrustUser, domain.TrustWorkspace, domain.TrustSystem,
		} {
			if back := trustFromProto(trustToProto(value)); back != value {
				t.Errorf("%q came back as %q", value, back)
			}
		}
	})

	t.Run("run status", func(t *testing.T) {
		for _, value := range []domain.RunStatus{
			domain.RunQueued, domain.RunRunning, domain.RunAwaitingApproval,
			domain.RunAwaitingInput, domain.RunCancelling, domain.RunCompleted,
			domain.RunFailed, domain.RunCancelled,
		} {
			if back := runStatusFromProto(runStatusToProto(value)); back != value {
				t.Errorf("%q came back as %q", value, back)
			}
		}
	})

	t.Run("stop reason", func(t *testing.T) {
		for _, value := range []domain.StopReason{
			domain.StopEndTurn, domain.StopMaxTokens, domain.StopContentFilter,
			domain.StopCancelled, domain.StopError, domain.StopToolUse,
		} {
			if back := stopReasonFromProto(stopReasonToProto(value)); back != value {
				t.Errorf("%q came back as %q", value, back)
			}
		}
	})

	t.Run("question kind", func(t *testing.T) {
		for _, value := range []domain.QuestionKind{domain.QuestionChoice, domain.QuestionText} {
			if back := questionKindFromProto(questionKindToProto(value)); back != value {
				t.Errorf("%q came back as %q", value, back)
			}
		}
	})

	t.Run("question status", func(t *testing.T) {
		for _, value := range []domain.QuestionStatus{
			domain.AnswerPending, domain.AnswerGiven, domain.AnswerAbandoned,
		} {
			if back := questionStatusFromProto(questionStatusToProto(value)); back != value {
				t.Errorf("%q came back as %q", value, back)
			}
		}
	})

	t.Run("approval status", func(t *testing.T) {
		for _, value := range []domain.ApprovalStatus{
			domain.ApprovalPending, domain.ApprovalAllowed, domain.ApprovalDenied,
		} {
			if back := approvalStatusFromProto(approvalStatusToProto(value)); back != value {
				t.Errorf("%q came back as %q", value, back)
			}
		}
	})

	t.Run("plan status", func(t *testing.T) {
		for _, value := range []domain.PlanStatus{
			domain.PlanPending, domain.PlanInProgress, domain.PlanCompleted, domain.PlanAbandoned,
		} {
			if back := PlanStatusFromProto(planStatusToProto(value)); back != value {
				t.Errorf("%q came back as %q", value, back)
			}
		}
	})
}

// Nobody named and somebody named are different facts, and a pointer that
// came back non-nil and empty would say the wrong one.
func TestNobodyNamedStaysNobody(t *testing.T) {
	local := domain.FromTheMachine("jingclaw-console")

	back := originFromProto(originToProto(local))
	if back.Principal != nil {
		t.Errorf("a decision at the machine gained a person: %+v", back.Principal)
	}
	if back.Kind != domain.OriginLocalClient || back.ClientID != "jingclaw-console" {
		t.Errorf("came back as %+v", back)
	}

	named := domain.FromAPlatformAccount("discord", "77", "Alice")
	back = originFromProto(originToProto(named))
	if back.Principal == nil {
		t.Fatal("a named account came back as nobody")
	}
	if *back.Principal != *named.Principal {
		t.Errorf("came back as %+v, want %+v", back.Principal, named.Principal)
	}
}

// An event the reader does not recognise is an error rather than one with an
// empty payload, which would reach a client as a thing that never happened.
func TestSomethingUnrecognisedIsAnError(t *testing.T) {
	if _, err := EventFromProto(&controlv1.Event{Id: "evt_1"}); err == nil {
		t.Error("an event with no payload was read without complaint")
	}
}

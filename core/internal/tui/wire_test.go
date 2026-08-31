package tui

import (
	"testing"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// A decision reaches the wire as the decision it was.
//
// Checked here because every other check in this package stops at the port:
// a fake records what the panel meant, and the translation to the wire is the
// step where a deny could become an allow with nothing to notice. The field
// this crosses was reserved once for exactly that reason.
func TestEveryDecisionCrossesToTheWireIntact(t *testing.T) {
	for _, crossing := range []struct {
		status domain.ApprovalStatus
		wanted controlv1.ApprovalDecision
	}{
		{domain.ApprovalAllowed, controlv1.ApprovalDecision_APPROVAL_DECISION_ALLOW},
		{domain.ApprovalDenied, controlv1.ApprovalDecision_APPROVAL_DECISION_DENY},

		// Neither, which must not become an allow. A status the panel did
		// not recognise is not permission to run something.
		{domain.ApprovalPending, controlv1.ApprovalDecision_APPROVAL_DECISION_DENY},
		{"", controlv1.ApprovalDecision_APPROVAL_DECISION_DENY},
	} {
		if got := decisionToProto(crossing.status); got != crossing.wanted {
			t.Errorf("%q crossed as %v, wanted %v", crossing.status, got, crossing.wanted)
		}
	}
}

// And so does how far it carries.
func TestEveryScopeCrossesToTheWireIntact(t *testing.T) {
	for _, crossing := range []struct {
		scope  domain.RememberScope
		wanted controlv1.RememberScope
	}{
		{domain.RememberSession, controlv1.RememberScope_REMEMBER_SCOPE_SESSION},
		{domain.RememberOnce, controlv1.RememberScope_REMEMBER_SCOPE_ONCE},

		// Anything else is once. A scope the panel did not recognise must not
		// widen into standing permission for the rest of the session.
		{"", controlv1.RememberScope_REMEMBER_SCOPE_ONCE},
	} {
		if got := scopeToProto(crossing.scope); got != crossing.wanted {
			t.Errorf("%q crossed as %v, wanted %v", crossing.scope, got, crossing.wanted)
		}
	}
}

// A question keeps the shape of answer it wants.
//
// A choice drawn as free text lets somebody type an answer the run then
// rejects, and they find out after the run has already refused it.
func TestAQuestionKeepsTheShapeOfAnswerItWants(t *testing.T) {
	for _, crossing := range []struct {
		kind   controlv1.QuestionKind
		wanted domain.QuestionKind
	}{
		{controlv1.QuestionKind_QUESTION_KIND_CHOICE, domain.QuestionChoice},
		{controlv1.QuestionKind_QUESTION_KIND_TEXT, domain.QuestionText},

		// Unrecognised is text. A panel offering no way to answer would leave
		// the run parked with nothing anybody could do about it.
		{controlv1.QuestionKind_QUESTION_KIND_UNSPECIFIED, domain.QuestionText},
	} {
		if got := questionKindOf(crossing.kind); got != crossing.wanted {
			t.Errorf("%v arrived as %q, wanted %q", crossing.kind, got, crossing.wanted)
		}
	}
}

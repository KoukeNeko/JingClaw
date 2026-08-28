package provider_test

import (
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// A model's working-out is not its answer. Keeping them apart in the type
// system is what stops the first backend that exposes reasoning from having it
// posted wherever the answer goes.
func TestReasoningIsNotText(t *testing.T) {
	var event provider.Event = provider.ReasoningDelta{Text: "let me think about the password"}

	if _, isText := event.(provider.TextDelta); isText {
		t.Fatal("reasoning satisfies TextDelta, so anything rendering answers will render it")
	}
	if _, isReasoning := event.(provider.ReasoningDelta); !isReasoning {
		t.Fatal("reasoning is not its own event")
	}
}

// Hardware and billing are different failures with different answers. One is
// fixed by a smaller request or a freer machine, the other by an operator.
func TestRunningOutOfMemoryIsNotRunningOutOfAllowance(t *testing.T) {
	if provider.KindResourceExhausted == provider.KindQuotaExhausted {
		t.Fatal("a machine out of memory is indistinguishable from an account out of allowance")
	}
	// Neither is worth resending unchanged: one needs a person, the other
	// needs the request to be smaller or the machine to be freer.
	if provider.KindResourceExhausted.Retryable() {
		t.Error("a request that did not fit is resent unchanged")
	}
	if !provider.KindResourceExhausted.NeedsOperator() {
		t.Error("a machine that cannot serve the request does not surface to anybody")
	}
}

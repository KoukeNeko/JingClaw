package console

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/internal/console"
)

// theTail is the part of a reason a 72-cell line does not reach: what a person
// reading a failure is actually after, and what the console used to lose.
const theTail = "litellm.BadRequestError: model Furen-max does not accept image parts"

func aFailedRun(reason, kind string) *controlv1.Event {
	return &controlv1.Event{
		SessionId:  "FX4K6XK8ABCDEF",
		GlobalSeq:  1,
		OccurredAt: timestamppb.Now(),
		Payload: &controlv1.Event_RunStateChanged{
			RunStateChanged: &controlv1.RunStateChanged{
				Status:      controlv1.RunStatus_RUN_STATUS_FAILED,
				Reason:      reason,
				FailureKind: kind,
			},
		},
	}
}

// A session writing to a buffer, so what it printed can be read back.
func aConsole() (*session, *bytes.Buffer) {
	var out bytes.Buffer
	screen := console.NewScreen(&out, func() int { return 80 })
	return &session{screen: screen}, &out
}

// The whole reason a run failed is printed when it fails, not the clipped line
// that stops before the part worth reading.
func TestAFailureIsPrintedInFull(t *testing.T) {
	s, out := aConsole()
	reason := "openai-compatible (generic, Furen-max): invalid_request: " + theTail

	s.show(aFailedRun(reason, "invalid_request"))

	if !strings.Contains(out.String(), theTail) {
		t.Errorf("the end of the reason was not printed: %q", out.String())
	}
}

// Once a failure has scrolled off, `why` brings the whole of it back — which
// it can only do because ListRuns does not carry the reason and the console
// kept it as it went past.
func TestWhyReprintsTheLastFailure(t *testing.T) {
	s, out := aConsole()
	reason := "provider gemini (gemini-3.8-flash): quota_exhausted: " + theTail
	s.show(aFailedRun(reason, "quota_exhausted"))
	out.Reset()

	s.whyLastFailed()

	written := out.String()
	for _, wanted := range []string{"quota_exhausted", theTail} {
		if !strings.Contains(written, wanted) {
			t.Errorf("why did not reprint %q: %s", wanted, written)
		}
	}
}

// `why` with nothing to show says so, rather than an empty answer that reads
// like a bug.
func TestWhyWithNoFailureSaysSo(t *testing.T) {
	s, out := aConsole()

	s.whyLastFailed()

	if !strings.Contains(out.String(), "no run has failed") {
		t.Errorf("why said something other than that nothing failed: %q", out.String())
	}
}

// A failure that carried no reason is reported as that, not as a blank the
// reader has to guess at.
func TestWhyOnAReasonlessFailure(t *testing.T) {
	s, out := aConsole()
	s.show(aFailedRun("", ""))
	out.Reset()

	s.whyLastFailed()

	if !strings.Contains(out.String(), "gave no reason") {
		t.Errorf("a reasonless failure was not reported as one: %q", out.String())
	}
}

// The `why` verb is wired through the same command path as the rest.
func TestWhyRunsThroughTheCommandPath(t *testing.T) {
	s, out := aConsole()
	s.show(aFailedRun("boom: "+theTail, "invalid_request"))
	out.Reset()

	if leave := s.run(context.Background(), "why"); leave {
		t.Fatal("why asked the console to close")
	}
	if !strings.Contains(out.String(), theTail) {
		t.Errorf("why through the command path did not reprint the reason: %q", out.String())
	}
}

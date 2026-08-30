package console

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

func at(text string) time.Time {
	moment, err := time.Parse(time.RFC3339, text)
	if err != nil {
		panic(err)
	}
	return moment
}

func anEvent(payload domain.EventPayload) domain.Event {
	return domain.Event{
		SessionID:  "abcdef0123456789",
		OccurredAt: at("2026-08-30T16:23:04Z"),
		Payload:    payload,
	}
}

// The fields are what tells one line from the hundred around it, so they come
// before anything that can push them off.
func TestTheFieldsComeInAFixedOrder(t *testing.T) {
	line, shown := Describe(anEvent(domain.ToolCallRequested{
		Name:      "exec_command",
		Arguments: `{"command":"git status"}`,
	}))
	if !shown {
		t.Fatal("a tool call produced no line")
	}

	written := line.String()
	for _, wanted := range []string{"16:23:04", "#abcdef01", "TOOL", "→", "exec_command"} {
		if !strings.Contains(written, wanted) {
			t.Errorf("the line does not contain %q: %s", wanted, written)
		}
	}
	if index := strings.Index(written, "exec_command"); index > strings.Index(written, "git status") {
		t.Errorf("the payload came before the tool name: %s", written)
	}
}

// A console has to say which session a line belongs to, because several of
// them write to it at once.
func TestEveryLineNamesItsSession(t *testing.T) {
	for _, payload := range []domain.EventPayload{
		domain.UserMessageAdded{Text: "hello"},
		domain.RunStateChanged{Status: domain.RunRunning},
		domain.ToolCallCompleted{Name: "read_file"},
		domain.ApprovalRequested{ToolName: "exec_command"},
		domain.QuestionAsked{Prompt: "which one?"},
	} {
		line, shown := Describe(anEvent(payload))
		if !shown {
			t.Errorf("%T produced no line", payload)
			continue
		}
		if !strings.Contains(line.String(), "#abcdef01") {
			t.Errorf("%T does not name its session: %s", payload, line.String())
		}
	}
}

// A token at a time is not something anyone reads; the completed message says
// it once.
func TestDeltasAreNotShown(t *testing.T) {
	for _, payload := range []domain.EventPayload{
		domain.AssistantTextDelta{Text: "he"},
		domain.AssistantReasoningDelta{Text: "thinking"},
	} {
		if _, shown := Describe(anEvent(payload)); shown {
			t.Errorf("%T produced a line", payload)
		}
	}
}

// Deciding whether to run a command means deciding about the command. A
// shortened or reworded one is a different thing being approved.
func TestAnApprovalShowsTheCommandItWasAskedWith(t *testing.T) {
	line, _ := Describe(anEvent(domain.ApprovalRequested{
		ToolName:  "exec_command",
		Arguments: `{"command":"rm -rf ./cache"}`,
		Summary:   "tidy up some temporary files",
		Effects:   []string{"delete"},
	}))

	written := line.String()
	if !strings.Contains(written, "rm -rf ./cache") {
		t.Errorf("the command is not on the line: %s", written)
	}
	if strings.Contains(written, "tidy up") {
		t.Errorf("the line shows a summary of the command instead of the command: %s", written)
	}
	if !strings.Contains(written, "delete") {
		t.Errorf("what it would do is not on the line: %s", written)
	}
}

// A hundred lines of tool output in the running log is the rest of the log
// gone.
func TestAMultiLinePayloadBecomesOneLine(t *testing.T) {
	line, _ := Describe(anEvent(domain.ToolCallCompleted{
		Name:    "exec_command",
		Content: "first\nsecond\nthird",
	}))

	if strings.Contains(line.String(), "\n") {
		t.Errorf("the line contains a newline: %q", line.String())
	}
}

// Counting bytes or runes puts the ellipsis somewhere other than where the
// reader sees it.
func TestClippingCountsWhatTheTerminalDraws(t *testing.T) {
	wide := strings.Repeat("中", 60)
	clipped := clip(wide, previewWidth)

	if width := ansi.GraphemeWidth.StringWidth(clipped); width > previewWidth {
		t.Errorf("clipped to %d cells, wanted at most %d: %q", width, previewWidth, clipped)
	}
	if !strings.HasSuffix(clipped, "…") {
		t.Errorf("nothing was cut: %q", clipped)
	}
	// Sixty of them is a hundred and twenty cells, so about half should have
	// survived; counting runes would have kept all sixty.
	if count := len([]rune(clipped)); count > 40 {
		t.Errorf("kept %d characters, which is more than fits: %q", count, clipped)
	}
}

func TestSomethingShortIsNotTouched(t *testing.T) {
	if clipped := clip("git status", previewWidth); clipped != "git status" {
		t.Errorf("clip changed a short string: %q", clipped)
	}
}

// Who said it, not what their identifier is.
func TestAMessageSaysWhoSentIt(t *testing.T) {
	line, _ := Describe(anEvent(domain.UserMessageAdded{
		Text: "production still returns 502",
		Origin: domain.RunOrigin{
			Kind:      domain.OriginGateway,
			Principal: &domain.ExternalPrincipal{PrincipalID: "77", DisplayName: "Alice"},
		},
	}))

	written := line.String()
	if !strings.Contains(written, "Alice") {
		t.Errorf("the line does not say who sent it: %s", written)
	}
	if strings.Contains(written, "77") {
		t.Errorf("the line shows an identifier rather than a name: %s", written)
	}
}

func TestAPlanSaysHowFarItHasGot(t *testing.T) {
	line, _ := Describe(anEvent(domain.PlanChanged{Items: []domain.PlanItem{
		{Title: "read the config", Status: domain.PlanCompleted},
		{Title: "write the file", Status: domain.PlanInProgress},
		{Title: "check it", Status: domain.PlanPending},
	}}))

	written := line.String()
	if !strings.Contains(written, "1/3") {
		t.Errorf("the line does not count the steps: %s", written)
	}
	if !strings.Contains(written, "write the file") {
		t.Errorf("the line does not say what it is doing: %s", written)
	}
}

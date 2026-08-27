package runtime

import (
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

func userMessage(seq int, text string) boundedMessage {
	return boundedMessage{
		Message: provider.Message{Role: provider.RoleUser, Content: provider.Text(text)},
		LastSeq: domain.Seq(seq),
	}
}

func assistantCall(seq int, id, name string) boundedMessage {
	return boundedMessage{
		Message: provider.Message{
			Role:    provider.RoleAssistant,
			Content: []provider.ContentBlock{provider.ToolUseBlock{ID: id, Name: name}},
		},
		LastSeq: domain.Seq(seq),
	}
}

func toolResult(seq int, id, name, content string) boundedMessage {
	return boundedMessage{
		Message: provider.Message{
			Role: provider.RoleTool,
			Content: []provider.ContentBlock{
				provider.ToolResultBlock{ToolUseID: id, Name: name, Content: content},
			},
		},
		LastSeq: domain.Seq(seq),
	}
}

// A tool result with no call in front of it is not a smaller conversation, it
// is an invalid one, and providers reject it outright. This is the property
// the cut exists to preserve.
func TestCutNeverSeparatesAToolCallFromItsResult(t *testing.T) {
	messages := []boundedMessage{
		userMessage(1, strings.Repeat("a", 4000)),
		assistantCall(2, "call_1", "read_file"),
		toolResult(3, "call_1", "read_file", strings.Repeat("b", 4000)),
		userMessage(4, "and now?"),
		assistantCall(5, "call_2", "grep"),
		toolResult(6, "call_2", "grep", strings.Repeat("c", 4000)),
	}

	// Every budget, not one convenient one: the cut has to be legal whatever
	// size it is asked for.
	for keep := int64(0); keep <= 4000; keep += 37 {
		cut := chooseCut(messages, keep)

		if cut <= 0 || cut >= len(messages) {
			t.Fatalf("keep=%d produced cut %d, which folds everything or nothing", keep, cut)
		}
		if role := messages[cut].Message.Role; role == provider.RoleTool {
			t.Fatalf("keep=%d cut at %d, where the tail begins with tool results whose call was folded",
				keep, cut)
		}
	}
}

// Folding everything would leave the model a summary and no idea what it was
// just asked.
func TestCutKeepsTheMostRecentTurnHoweverSmallTheBudget(t *testing.T) {
	messages := []boundedMessage{
		userMessage(1, strings.Repeat("a", 8000)),
		userMessage(2, strings.Repeat("b", 8000)),
		userMessage(3, strings.Repeat("c", 8000)),
	}

	cut := chooseCut(messages, 1)
	if cut != len(messages)-1 {
		t.Fatalf("cut is %d, want %d so the last message survives", cut, len(messages)-1)
	}
}

// One message cannot be folded into a summary and also kept, so a conversation
// that is over budget on its own has nothing safe to do.
func TestCutRefusesWhenThereIsOnlyOneMessage(t *testing.T) {
	messages := []boundedMessage{userMessage(1, strings.Repeat("a", 40000))}

	if cut := chooseCut(messages, 1); cut != 0 {
		t.Errorf("cut is %d, want 0 so that nothing is folded", cut)
	}
}

func TestEstimateGrowsWithWhatIsSent(t *testing.T) {
	short := estimateMessage(provider.Message{
		Role: provider.RoleUser, Content: provider.Text("hi")})
	long := estimateMessage(provider.Message{
		Role: provider.RoleUser, Content: provider.Text(strings.Repeat("hi", 1000))})

	if long <= short {
		t.Errorf("a longer message estimated %d against %d for a shorter one", long, short)
	}

	// Tool results are usually the largest thing in a conversation, so leaving
	// them out of the estimate would defeat the purpose of having one.
	withResult := estimateMessage(provider.Message{
		Role: provider.RoleTool,
		Content: []provider.ContentBlock{
			provider.ToolResultBlock{Name: "read_file", Content: strings.Repeat("x", 8000)},
		},
	})
	if withResult < 2000 {
		t.Errorf("an 8000-character tool result estimated %d tokens", withResult)
	}
}

// The system prompt and the tool schemas are sent on every turn and are not
// small; history must not be allowed to grow into the space they occupy.
func TestOverheadCountsToolSchemas(t *testing.T) {
	overhead := estimateRequestOverhead(
		provider.Text("you are an agent"),
		[]provider.ToolDeclaration{{
			Name:        "read_file",
			Description: "Read part of a file.",
			InputSchema: []byte(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		}},
	)

	if overhead == 0 {
		t.Fatal("the overhead of a system prompt and a tool schema estimated as nothing")
	}

	bare := estimateRequestOverhead(provider.Text("you are an agent"), nil)
	if overhead <= bare {
		t.Errorf("declaring a tool did not raise the overhead: %d against %d", overhead, bare)
	}
}

// The request that asks for a summary has to fit in the window too, so an
// over-long transcript loses its middle rather than its end.
func TestTranscriptKeepsBothEndsWhenItIsTooLong(t *testing.T) {
	messages := []boundedMessage{
		userMessage(1, "THE-ORIGINAL-TASK"),
		userMessage(2, strings.Repeat("filler ", 5000)),
		userMessage(3, "THE-LATEST-STATE"),
	}

	transcript := renderTranscript(messages, 2000)

	if len(transcript) > 2200 {
		t.Errorf("the transcript is %d bytes against a 2000-byte bound", len(transcript))
	}
	for _, want := range []string{"THE-ORIGINAL-TASK", "THE-LATEST-STATE", "omitted"} {
		if !strings.Contains(transcript, want) {
			t.Errorf("the bounded transcript lost %q", want)
		}
	}
}

func TestTranscriptRecordsCallsAndTheirResults(t *testing.T) {
	transcript := renderTranscript([]boundedMessage{
		assistantCall(1, "call_1", "exec_command"),
		toolResult(2, "call_1", "exec_command", "FAIL TestCountVowels"),
	}, 0)

	for _, want := range []string{"exec_command", "FAIL TestCountVowels"} {
		if !strings.Contains(transcript, want) {
			t.Errorf("the transcript does not mention %q:\n%s", want, transcript)
		}
	}
}

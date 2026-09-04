package runtime

import (
	"context"
	"fmt"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

func personTurn(words string) provider.Message {
	return provider.Message{
		Role: provider.RoleUser,
		Content: []provider.ContentBlock{
			provider.TextBlock{Text: "[1970-01-01T00:00:00Z · from this machine]", Annotation: true},
			provider.TextBlock{Text: words},
		},
	}
}

func recallingRuntime(recall func(context.Context, domain.Run, string) string) *Runtime {
	return &Runtime{
		opts:   Options{Recall: recall},
		active: map[domain.RunID]*activeRun{},
	}
}

func constantLabel(label string) func(context.Context, domain.Run, string) string {
	return func(context.Context, domain.Run, string) string { return label }
}

// The label goes after what this machine already wrote in front of the turn
// and before the person's words, which stay a block of their own.
func TestTheLabelSitsBetweenTheStampAndTheWords(t *testing.T) {
	rt := recallingRuntime(constantLabel("[noted]"))
	messages := []provider.Message{personTurn("hello")}

	labelled := rt.withRecalled(context.Background(), domain.Run{ID: "run_1"}, messages)

	got := labelled[0].Content
	if len(got) != 3 {
		t.Fatalf("expected stamp, label, words; got %d blocks", len(got))
	}
	if text := got[1].(provider.TextBlock); text.Text != "[noted]" || !text.Annotation {
		t.Errorf("the middle block is %+v, not the label as an annotation", text)
	}
	if text := got[2].(provider.TextBlock); text.Text != "hello" || text.Annotation {
		t.Errorf("the last block is %+v, not the person's own words", text)
	}

	// And the messages handed in are not what was changed.
	if len(messages[0].Content) != 2 {
		t.Error("the conversation handed in was edited in place")
	}
}

// A worker is reading files and gets no label, however the hook answers.
func TestAWorkerGetsNoLabel(t *testing.T) {
	rt := recallingRuntime(constantLabel("[noted]"))
	messages := []provider.Message{personTurn("investigate")}

	labelled := rt.withRecalled(context.Background(),
		domain.Run{ID: "run_w", Kind: domain.RunWorker}, messages)

	if len(labelled[0].Content) != 2 {
		t.Error("a worker's turn was labelled")
	}
}

// Nothing noted, nothing added: not an empty label block.
func TestAnEmptyLabelAddsNothing(t *testing.T) {
	rt := recallingRuntime(constantLabel(""))
	labelled := rt.withRecalled(context.Background(), domain.Run{ID: "run_1"},
		[]provider.Message{personTurn("hello")})

	if len(labelled[0].Content) != 2 {
		t.Errorf("an empty label still added a block: %+v", labelled[0].Content)
	}
}

// One run, one answer. Every request the run makes carries the same label,
// even when what the hook would say has changed in between — a label that
// changed between requests would change the prefix everything after it is
// cached against.
func TestTheLabelIsAskedOncePerRun(t *testing.T) {
	asked := 0
	rt := recallingRuntime(func(context.Context, domain.Run, string) string {
		asked++
		return fmt.Sprintf("[noted %d]", asked)
	})
	run := domain.Run{ID: "run_1"}
	rt.active[run.ID] = &activeRun{}

	first := rt.withRecalled(context.Background(), run, []provider.Message{personTurn("hello")})
	second := rt.withRecalled(context.Background(), run, []provider.Message{personTurn("hello")})

	if asked != 1 {
		t.Errorf("the hook was asked %d times for one run", asked)
	}
	if a, b := first[0].Content[1].(provider.TextBlock).Text, second[0].Content[1].(provider.TextBlock).Text; a != b {
		t.Errorf("the label changed within a run: %q then %q", a, b)
	}
}
